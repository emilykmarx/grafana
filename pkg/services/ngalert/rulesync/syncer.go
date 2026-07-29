package rulesync

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/open-feature/go-sdk/openfeature"

	"github.com/grafana/grafana/pkg/apimachinery/identity"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/services/datasourceproxy"
	"github.com/grafana/grafana/pkg/services/datasources"
	"github.com/grafana/grafana/pkg/services/featuremgmt"
	"github.com/grafana/grafana/pkg/services/folder"
	"github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/services/ngalert/prom"
	"github.com/grafana/grafana/pkg/services/ngalert/provisioning"
	"github.com/grafana/grafana/pkg/setting"
)

// rootFolderTitle is the folder the syncer lands imported namespaces under, so
// they are isolated from user-managed folders. One ruler datasource per org, so
// a single fixed title is used. The syncer creates one subfolder per upstream
// namespace under this root, and ownership of converted rules is defined as
// "converted_prometheus rules under this folder subtree".
//
// NOTE: a user folder with the same title would be reused. This is
// non-destructive because prune only ever deletes converted_prometheus rules
// that live under this dedicated folder subtree.
const rootFolderTitle = "External Ruler Sync"

const versionMessage = "external ruler sync"

const promoteVersionMessage = "external ruler sync: promote to native rules"

// ruleService is the subset of provisioning.AlertRuleService the syncer needs.
type ruleService interface {
	ReplaceRuleGroups(ctx context.Context, user identity.Requester, groups []*models.AlertRuleGroup, provenance models.Provenance, versionMessage string) error
	DeleteRuleGroups(ctx context.Context, user identity.Requester, provenance models.Provenance, filterOpts *provisioning.FilterOptions) error
	GetAlertGroupsWithFolderFullpath(ctx context.Context, user identity.Requester, filterOpts *provisioning.FilterOptions) ([]models.AlertRuleGroupWithFolderFullpath, error)
	GetAlertRules(ctx context.Context, user identity.Requester) ([]*models.AlertRule, map[string]models.Provenance, error)
}

// namespaceStore creates/looks up the folders the imported rules live in.
type namespaceStore interface {
	GetOrCreateNamespaceByTitle(ctx context.Context, title string, orgID int64, user identity.Requester, parentUID string) (*folder.FolderReference, bool, error)
	GetNamespaceChildren(ctx context.Context, uid string, orgID int64, user identity.Requester) ([]*folder.FolderReference, error)
}

// rulerFetcher fetches the upstream ruler config. Satisfied by *RulerFetcher.
type rulerFetcher interface {
	Fetch(ctx context.Context, ds *datasources.DataSource) (RulerConfig, uint64, error)
}

type datasourceGetter interface {
	GetDataSource(ctx context.Context, query *datasources.GetDataSourceQuery) (*datasources.DataSource, error)
}

type orgStore interface {
	FetchOrgIds(ctx context.Context) ([]int64, error)
}

// ExternalRulerSyncer mirrors alert rules from a configured external Mimir/Cortex
// ruler datasource into Grafana as converted-Prometheus rules. It is the rule
// analogue of ExternalAMSyncer. The loop driver (Run) is intentionally thin so
// the same SyncOrg core could later be hosted by an app runner instead.
//
// Promotion (the one-way exit): when spec.externalRulerSync.promote is set, the
// worker converts the rules it synced into native Grafana rules the org owns by
// flipping their provenance (converted_prometheus -> none, which makes them
// editable), then stops syncing and records the terminal PromotionCommitted
// status. After the flip the rules are provenance=none, so a re-run selects
// nothing and promotion is terminal. This is the rule-side analogue of the
// Alertmanager sync's merge-commit.
type ExternalRulerSyncer struct {
	settings *setting.UnifiedAlertingSettings
	logger   log.Logger
	metrics  *Metrics

	datasources    datasourceGetter
	fetcher        rulerFetcher
	ruleService    ruleService
	namespaceStore namespaceStore
	orgStore       orgStore
	configStore    rulesConfigStore

	// featureEnabled reports whether the ruler sync feature flag is on. Injected
	// so tests don't need OpenFeature plumbing.
	featureEnabled func(ctx context.Context) bool

	lastSyncHashMu sync.RWMutex
	lastSyncHash   map[int64]uint64

	// managedRootUID caches the UID of each org's dedicated sync root folder
	// ("External Ruler Sync"), resolved during apply. The convert API's 409 gate
	// uses it (via IsManagedFolder) to reject only imports that target the
	// sync-managed folder subtree. Guarded by managedRootMu.
	managedRootMu  sync.RWMutex
	managedRootUID map[int64]string
}

// NewExternalRulerSyncer constructs an ExternalRulerSyncer. The ruler config GET
// is routed through the datasource proxy service (transport, auth and egress
// validation are handled there). The syncer runs from the ini setting alone;
// per-org config and status arrive with the rules-app Config store in the
// app-resource PR (#127756).
func NewExternalRulerSyncer(
	settings *setting.UnifiedAlertingSettings,
	logger log.Logger,
	m *Metrics,
	datasourceService datasources.DataSourceService,
	proxy *datasourceproxy.DataSourceProxyService,
	ruleSvc ruleService,
	namespaceStore namespaceStore,
	orgStore orgStore,
) *ExternalRulerSyncer {
	return &ExternalRulerSyncer{
		settings:       settings,
		logger:         logger,
		metrics:        m,
		datasources:    datasourceService,
		fetcher:        NewRulerFetcher(proxy),
		ruleService:    ruleSvc,
		namespaceStore: namespaceStore,
		orgStore:       orgStore,
		// TODO(app-resource PR #127756): inject the rules-app Config store here so
		// per-org spec.externalRulerSync + status.externalRulerSync take effect.
		configStore: noopConfigStore{},
		featureEnabled: func(ctx context.Context) bool {
			return openfeature.NewDefaultClient().Boolean(ctx, featuremgmt.FlagAlertingSyncExternalRuler, false, openfeature.TransactionContext(ctx))
		},
		lastSyncHash:   make(map[int64]uint64),
		managedRootUID: make(map[int64]string),
	}
}

// Run polls all orgs at AdminConfigPollInterval until ctx is cancelled.
func (s *ExternalRulerSyncer) Run(ctx context.Context) error {
	s.logger.Info("Starting external ruler syncer", "poll_interval", s.settings.AdminConfigPollInterval)
	ticker := time.NewTicker(s.settings.AdminConfigPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.syncAllOrgs(ctx)
		}
	}
}

func (s *ExternalRulerSyncer) syncAllOrgs(ctx context.Context) {
	if !s.featureEnabled(ctx) {
		return
	}
	orgIDs, err := s.orgStore.FetchOrgIds(ctx)
	if err != nil {
		s.logger.Error("Failed to fetch org IDs for external ruler sync", "error", err)
		return
	}
	for _, orgID := range orgIDs {
		if _, disabled := s.settings.DisabledOrgs[orgID]; disabled {
			continue
		}
		// SyncOrg isolates per-org failures (logs + metrics internally).
		s.SyncOrg(ctx, orgID)
	}
}

// IsConfiguredForOrg reports whether external ruler sync is configured for the
// org — the operator ini override is set, or a non-empty datasource UID is set
// on the org's Config resource. Used by the convert API to reject manual rule
// imports while sync owns the org's rules.
func (s *ExternalRulerSyncer) IsConfiguredForOrg(ctx context.Context, orgID int64) (bool, error) {
	if s.settings.ExternalRulerUID != "" {
		return true, nil
	}
	spec, err := s.configStore.GetSyncSpec(ctx, orgID)
	if err != nil {
		return false, err
	}
	return spec.DatasourceUID != "", nil
}

// IsManagedFolder reports whether folderUID is inside the org's sync-managed
// folder subtree — the dedicated root folder or one of its namespace
// subfolders. The convert API uses it to fold the 409 gate down to just the
// folders the sync worker owns, so manual imports into unrelated folders are
// still allowed. If sync hasn't run for the org yet there is no cached root
// UID, so nothing is managed there and this returns false (allow).
func (s *ExternalRulerSyncer) IsManagedFolder(ctx context.Context, orgID int64, folderUID string) (bool, error) {
	s.managedRootMu.RLock()
	rootUID, ok := s.managedRootUID[orgID]
	s.managedRootMu.RUnlock()
	if !ok || rootUID == "" {
		return false, nil
	}
	if folderUID == rootUID {
		return true, nil
	}
	_, user := identity.WithServiceIdentity(ctx, orgID)
	children, err := s.namespaceStore.GetNamespaceChildren(ctx, rootUID, orgID, user)
	if err != nil {
		return false, fmt.Errorf("list sync folder children: %w", err)
	}
	for _, child := range children {
		if child.UID == folderUID {
			return true, nil
		}
	}
	return false, nil
}

// SyncOrg runs one sync tick for a single org. It never returns an error;
// failures are logged, counted, and reflected in the Config status so a bad org
// can't break the others.
func (s *ExternalRulerSyncer) SyncOrg(ctx context.Context, orgID int64) {
	if !s.featureEnabled(ctx) {
		return
	}

	orgIDStr := strconv.FormatInt(orgID, 10)

	rc, err := s.resolveExternalRulerConfig(ctx, orgID)
	if err != nil {
		s.recordFailure(ctx, orgID, orgIDStr, rc.queryUID, rc.origin, &SyncError{Reason: ReasonConfigRead, Cause: err})
		return
	}
	if rc.queryUID == "" {
		// Not configured here: seed the singleton (Unknown/NotConfigured) so it
		// exists without a manual create, mirroring the AM sync.
		s.recordNotConfigured(ctx, orgID)
		return
	}

	svcCtx, svcUser := identity.WithServiceIdentity(ctx, orgID)

	// Promotion is the terminal exit: convert the synced rules to native rules
	// the org owns and stop syncing. Idempotent — once promoted there are no
	// owned rules left, so subsequent ticks are cheap no-ops that just re-assert
	// the terminal status.
	if rc.promote {
		if err := s.promote(svcCtx, svcUser, orgID); err != nil {
			s.recordFailure(ctx, orgID, orgIDStr, rc.queryUID, rc.origin, &SyncError{Reason: ReasonPromote, Cause: err})
			return
		}
		s.recordPromotionCommitted(ctx, orgID, rc.queryUID, rc.origin)
		return
	}

	start := time.Now()
	defer func() { s.metrics.SyncDuration.WithLabelValues(orgIDStr).Observe(time.Since(start).Seconds()) }()

	ds, err := s.datasources.GetDataSource(svcCtx, &datasources.GetDataSourceQuery{UID: rc.queryUID, OrgID: orgID})
	if err != nil {
		s.recordFailure(ctx, orgID, orgIDStr, rc.queryUID, rc.origin, &SyncError{Reason: ReasonDatasourceLookup, Cause: err})
		return
	}

	// Recording rules write to the target datasource; it defaults to the query
	// datasource. Only resolve a distinct target when one is configured.
	targetDS := ds
	if rc.targetUID != "" && rc.targetUID != rc.queryUID {
		targetDS, err = s.datasources.GetDataSource(svcCtx, &datasources.GetDataSourceQuery{UID: rc.targetUID, OrgID: orgID})
		if err != nil {
			s.recordFailure(ctx, orgID, orgIDStr, rc.queryUID, rc.origin, &SyncError{Reason: ReasonDatasourceLookup, Cause: fmt.Errorf("target datasource %q: %w", rc.targetUID, err)})
			return
		}
	}

	cfg, hash, err := s.fetcher.Fetch(svcCtx, ds)
	if err != nil {
		reason := ReasonRulerFetch
		if errors.Is(err, ErrNotARuler) {
			reason = ReasonNotARuler
		}
		s.recordFailure(ctx, orgID, orgIDStr, rc.queryUID, rc.origin, &SyncError{Reason: reason, Cause: err})
		return
	}
	s.metrics.SyncTotal.WithLabelValues(orgIDStr).Inc()

	// Skip if the upstream is unchanged since the last successful apply. The
	// persisted hash (from Config status) survives restarts/replicas; the
	// in-memory map is the fast path and the fallback when it's unavailable.
	hashStr := strconv.FormatUint(hash, 10)
	if rc.persistedHash != "" && rc.persistedHash == hashStr {
		s.logger.Debug("External ruler config unchanged since last sync (persisted)", "org_id", orgID)
		return
	}
	s.lastSyncHashMu.RLock()
	prev, has := s.lastSyncHash[orgID]
	s.lastSyncHashMu.RUnlock()
	if has && prev == hash {
		s.logger.Debug("External ruler config unchanged since last sync", "org_id", orgID)
		return
	}

	if applyErr := s.apply(svcCtx, svcUser, orgID, ds, targetDS, cfg); applyErr != nil {
		s.recordFailure(ctx, orgID, orgIDStr, rc.queryUID, rc.origin, applyErr)
		return
	}

	s.lastSyncHashMu.Lock()
	s.lastSyncHash[orgID] = hash
	s.lastSyncHashMu.Unlock()
	s.metrics.SyncHash.WithLabelValues(orgIDStr).Set(float64(hash & mask53))
	s.recordSyncResult(ctx, orgID, rc.queryUID, rc.origin, nil, hashStr)
	s.logger.Debug("External ruler sync applied", "org_id", orgID, "namespaces", len(cfg))
}

type groupKey struct {
	folderUID string
	group     string
}

// apply converts the fetched ruler config into Grafana rule groups, persists
// them, and prunes previously-synced groups that vanished upstream. Returns a
// classified *SyncError on failure.
func (s *ExternalRulerSyncer) apply(ctx context.Context, user identity.Requester, orgID int64, ds *datasources.DataSource, targetDS *datasources.DataSource, cfg RulerConfig) *SyncError {
	root, _, err := s.namespaceStore.GetOrCreateNamespaceByTitle(ctx, rootFolderTitle, orgID, user, "")
	if err != nil {
		return &SyncError{Reason: ReasonSave, Cause: fmt.Errorf("get-or-create root folder: %w", err)}
	}
	// Cache the resolved root folder UID so the convert API's 409 gate
	// (IsManagedFolder) can fold rejections down to the sync-managed subtree.
	s.managedRootMu.Lock()
	s.managedRootUID[orgID] = root.UID
	s.managedRootMu.Unlock()

	groups := make([]*models.AlertRuleGroup, 0)
	desired := make(map[groupKey]struct{})
	for namespace, promGroups := range cfg {
		nsFolder, _, err := s.namespaceStore.GetOrCreateNamespaceByTitle(ctx, namespace, orgID, user, root.UID)
		if err != nil {
			return &SyncError{Reason: ReasonSave, Cause: fmt.Errorf("get-or-create namespace folder %q: %w", namespace, err)}
		}
		for _, promGroup := range promGroups {
			group, err := prom.ConvertRuleGroup(s.settings, ds, targetDS, orgID, nsFolder.UID, promGroup, prom.Options{
				KeepOriginalRuleDefinition: true,
			})
			if err != nil {
				return &SyncError{Reason: ReasonConvert, Cause: fmt.Errorf("convert group %q in namespace %q: %w", promGroup.Name, namespace, err)}
			}
			groups = append(groups, group)
			desired[groupKey{folderUID: nsFolder.UID, group: group.Title}] = struct{}{}
		}
	}

	if err := s.ruleService.ReplaceRuleGroups(ctx, user, groups, models.ProvenanceConvertedPrometheus, versionMessage); err != nil {
		return &SyncError{Reason: ReasonSave, Cause: err}
	}

	if err := s.prune(ctx, user, orgID, root.UID, desired); err != nil {
		return &SyncError{Reason: ReasonPrune, Cause: err}
	}
	return nil
}

// prune deletes converted-Prometheus rule groups that live under the sync's
// dedicated folder subtree (the root folder and its namespace subfolders) but
// that are no longer present upstream. Scoping the store queries to those
// folders ensures we never enumerate or delete converted rules that live in
// user-managed folders.
func (s *ExternalRulerSyncer) prune(ctx context.Context, user identity.Requester, orgID int64, rootUID string, desired map[groupKey]struct{}) error {
	children, err := s.namespaceStore.GetNamespaceChildren(ctx, rootUID, orgID, user)
	if err != nil {
		return fmt.Errorf("list sync folder children: %w", err)
	}
	nsUIDs := make([]string, 0, len(children)+1)
	nsUIDs = append(nsUIDs, rootUID)
	for _, child := range children {
		nsUIDs = append(nsUIDs, child.UID)
	}

	existing, err := s.ruleService.GetAlertGroupsWithFolderFullpath(ctx, user, &provisioning.FilterOptions{
		NamespaceUIDs:               nsUIDs,
		HasPrometheusRuleDefinition: new(true),
	})
	if err != nil {
		return fmt.Errorf("list converted rule groups: %w", err)
	}

	for _, g := range existing {
		if g.AlertRuleGroup == nil || len(g.Rules) == 0 {
			continue
		}
		if _, ok := desired[groupKey{folderUID: g.FolderUID, group: g.Title}]; ok {
			continue // still present upstream
		}
		if err := s.ruleService.DeleteRuleGroups(ctx, user, models.ProvenanceConvertedPrometheus, &provisioning.FilterOptions{
			NamespaceUIDs:               []string{g.FolderUID},
			RuleGroups:                  []string{g.Title},
			HasPrometheusRuleDefinition: new(true),
		}); err != nil {
			return fmt.Errorf("delete stale rule group %q in folder %q: %w", g.Title, g.FolderUID, err)
		}
		s.logger.Info("Pruned external ruler rule group no longer present upstream", "folder_uid", g.FolderUID, "group", g.Title)
	}
	return nil
}

// promote converts the rules this sync produced into native Grafana rules the
// org owns. It selects every converted_prometheus rule under the sync's folder
// subtree and rewrites its group with provenance=none, which the provisioning
// service permits for a converted_prometheus -> none transition and which makes
// the rule editable. After the flip the rules are provenance=none, so a re-run
// selects nothing: promotion is terminal and idempotent.
func (s *ExternalRulerSyncer) promote(ctx context.Context, user identity.Requester, orgID int64) error {
	root, _, err := s.namespaceStore.GetOrCreateNamespaceByTitle(ctx, rootFolderTitle, orgID, user, "")
	if err != nil {
		return fmt.Errorf("get-or-create root folder: %w", err)
	}
	children, err := s.namespaceStore.GetNamespaceChildren(ctx, root.UID, orgID, user)
	if err != nil {
		return fmt.Errorf("list sync folder children: %w", err)
	}
	nsUIDs := make(map[string]struct{}, len(children)+1)
	nsUIDs[root.UID] = struct{}{}
	for _, child := range children {
		nsUIDs[child.UID] = struct{}{}
	}

	rules, provenances, err := s.ruleService.GetAlertRules(ctx, user)
	if err != nil {
		return fmt.Errorf("list alert rules: %w", err)
	}

	selected := make([]*models.AlertRule, 0)
	for _, rule := range rules {
		if _, ok := nsUIDs[rule.NamespaceUID]; !ok {
			continue // not under the sync's folder subtree
		}
		if provenances[rule.ResourceID()] != models.ProvenanceConvertedPrometheus {
			continue // already promoted / user-owned
		}
		selected = append(selected, rule)
	}
	if len(selected) == 0 {
		return nil // already promoted / nothing synced
	}

	byKey := models.GroupByAlertRuleGroupKey(selected)
	groups := make([]*models.AlertRuleGroup, 0, len(byKey))
	for key, rg := range byKey {
		groupRules := make([]models.AlertRule, 0, len(rg))
		for _, r := range rg {
			groupRules = append(groupRules, *r)
		}
		var interval int64
		if len(groupRules) > 0 {
			interval = groupRules[0].IntervalSeconds
		}
		groups = append(groups, &models.AlertRuleGroup{
			Title:     key.RuleGroup,
			FolderUID: key.NamespaceUID,
			Interval:  interval,
			Rules:     groupRules,
		})
	}

	// provenance=none flips the stored converted_prometheus rules to user-owned.
	if err := s.ruleService.ReplaceRuleGroups(ctx, user, groups, models.ProvenanceNone, promoteVersionMessage); err != nil {
		return fmt.Errorf("promote rule groups: %w", err)
	}
	s.logger.Info("Promoted external ruler rules to native Grafana rules", "org_id", orgID, "groups", len(groups))
	return nil
}

// resolvedSync is the effective external-ruler-sync configuration for one org,
// after applying the ini override and target/promote defaults.
type resolvedSync struct {
	queryUID      string     // datasource to sync rules from
	targetUID     string     // recording-rules write target (defaults to queryUID)
	origin        syncOrigin // where queryUID came from (ini vs api)
	promote       bool       // one-way promote-to-native requested
	persistedHash string     // last-applied upstream hash from Config status
}

// resolveExternalRulerConfig computes the effective sync config for the org.
// The operator-level ini override wins over the per-org Config value for the
// query datasource, and ini UID resolution never fails on a Config read error
// (so ini-only deployments stay unaffected by apiserver availability). Under
// ini, target/promote (spec-only knobs) are off, but the persisted dedup hash
// is still read best-effort so both routes dedup the same way.
func (s *ExternalRulerSyncer) resolveExternalRulerConfig(ctx context.Context, orgID int64) (resolvedSync, error) {
	if iniUID := s.settings.ExternalRulerUID; iniUID != "" {
		rc := resolvedSync{queryUID: iniUID, targetUID: iniUID, origin: originIni}
		// Best-effort read of the persisted hash so the ini route dedups like the
		// api route; a Config read failure just falls back to the in-memory cache.
		if spec, err := s.configStore.GetSyncSpec(ctx, orgID); err == nil {
			rc.persistedHash = spec.LastAppliedHash
		}
		return rc, nil
	}
	spec, err := s.configStore.GetSyncSpec(ctx, orgID)
	if err != nil {
		return resolvedSync{origin: originAPI}, err
	}
	targetUID := spec.TargetDatasourceUID
	if targetUID == "" {
		targetUID = spec.DatasourceUID
	}
	return resolvedSync{queryUID: spec.DatasourceUID, targetUID: targetUID, origin: originAPI, promote: spec.Promote, persistedHash: spec.LastAppliedHash}, nil
}

// recordFailure logs, counts, and records a classified failure as the org's
// sync outcome.
func (s *ExternalRulerSyncer) recordFailure(ctx context.Context, orgID int64, orgIDStr, uid string, origin syncOrigin, syncErr *SyncError) {
	s.logger.Warn("External ruler sync failed", "org_id", orgID, "reason", syncErr.Reason.Label(), "error", syncErr)
	s.metrics.SyncFailures.WithLabelValues(orgIDStr, syncErr.Reason.Label()).Inc()
	s.recordSyncResult(ctx, orgID, uid, origin, syncErr, "")
}

// recordNotConfigured records that the feature is on but no datasource is
// configured for the org. Best-effort.
func (s *ExternalRulerSyncer) recordNotConfigured(ctx context.Context, orgID int64) {
	s.writeOutcome(ctx, orgID, syncOutcome{state: outcomeNotConfigured})
}

// recordPromotionCommitted records the terminal promoted outcome once the org's
// synced rules have been promoted to native rules (sync stops).
func (s *ExternalRulerSyncer) recordPromotionCommitted(ctx context.Context, orgID int64, uid string, origin syncOrigin) {
	s.writeOutcome(ctx, orgID, syncOutcome{state: outcomePromoted, datasourceUID: uid, origin: origin})
}

// recordSyncResult records the latest outcome (nil syncErr = success) for the
// org. Best-effort.
func (s *ExternalRulerSyncer) recordSyncResult(ctx context.Context, orgID int64, uid string, origin syncOrigin, syncErr error, appliedHash string) {
	out := syncOutcome{state: outcomeSuccess, datasourceUID: uid, origin: origin, appliedHash: appliedHash}
	if syncErr != nil {
		out = syncOutcome{state: outcomeFailure, datasourceUID: uid, origin: origin, reason: reasonOf(syncErr), errMsg: syncErr.Error()}
	}
	s.writeOutcome(ctx, orgID, out)
}

// writeOutcome persists a sync outcome via the config store. Best-effort.
func (s *ExternalRulerSyncer) writeOutcome(ctx context.Context, orgID int64, out syncOutcome) {
	if err := s.configStore.WriteStatus(ctx, orgID, out); err != nil {
		s.logger.Warn("Failed to write external ruler sync status", "org_id", orgID, "error", err)
	}
}
