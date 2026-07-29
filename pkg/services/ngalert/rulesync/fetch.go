package rulesync

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httptest"

	"go.yaml.in/yaml/v3"

	"github.com/grafana/grafana/pkg/apimachinery/identity"
	contextmodel "github.com/grafana/grafana/pkg/services/contexthandler/model"
	"github.com/grafana/grafana/pkg/services/datasources"
	apimodels "github.com/grafana/grafana/pkg/services/ngalert/api/tooling/definitions"
	"github.com/grafana/grafana/pkg/services/user"
	"github.com/grafana/grafana/pkg/web"
)

// rulerConfigAPIPath is the Mimir/Cortex ruler config API path, proxied through
// to the datasource. It is deliberately the config API (rule group definitions),
// NOT the query API /api/v1/rules that vanilla Prometheus serves (which returns
// rule state, a different shape).
const rulerConfigAPIPath = "/config/v1/rules"

// RulerConfig is the namespace-grouped rule configuration returned by a
// Mimir/Cortex ruler config API — the exact shape the convert API already
// accepts (map[namespace][]PrometheusRuleGroup).
type RulerConfig = map[string][]apimodels.PrometheusRuleGroup

// ErrNotARuler indicates the datasource did not respond as a Mimir/Cortex ruler
// config API (unexpected non-2xx, or a 200 that does not parse as
// namespace-grouped rule configs), letting callers distinguish a misconfigured
// datasource from a transient network failure. An empty ruler (no rule groups)
// is NOT an error; see Fetch.
var ErrNotARuler = errors.New("datasource does not expose a Mimir/Cortex ruler config API")

// datasourceProxy routes an outbound request through Grafana's datasource proxy
// service, so the datasource's configured auth/TLS/headers are honoured and the
// same egress allow/deny-list validation the user-driven proxy runs is applied.
// *datasourceproxy.DataSourceProxyService satisfies it; a fake stands in for it
// in tests.
type datasourceProxy interface {
	ProxyDatasourceRequestWithUID(c *contextmodel.ReqContext, dsUID string)
}

// RulerFetcher fetches namespace-grouped rule configs from a Mimir/Cortex ruler
// datasource by routing the ruler config GET through Grafana's datasource proxy
// service (transport, auth and egress validation are all handled there). Shared
// by the sync worker and the Config admission validator.
type RulerFetcher struct {
	proxy datasourceProxy
}

// NewRulerFetcher constructs a RulerFetcher around the datasource proxy service.
func NewRulerFetcher(proxy datasourceProxy) *RulerFetcher {
	return &RulerFetcher{proxy: proxy}
}

// Fetch retrieves the ruler configuration from ds, returning the parsed configs
// and the FNV-1a hash of the raw body (for cross-tick dedup). A 404 is "no rules
// configured" (empty RulerConfig, nil error). A non-404 non-2xx, or a 200 whose
// body does not parse, yields ErrNotARuler.
//
// The request is routed through the datasource proxy service: the proxy loads
// the datasource by UID (access-checked against SignedInUser), validates egress,
// and writes the proxied datasource response into the ReqContext's ResponseWriter.
// The proxy derives the upstream path from the request URL
// (/api/datasources/proxy/uid/<uid>/config/v1/rules -> config/v1/rules), which is
// then joined onto the datasource URL, preserving any configured HTTP prefix.
//
// TODO: verify Mimir's empty-vs-absent response (404 vs 200 empty body) against
// a live ruler; the 404 handling mirrors Grafana's frontend ruler client.
func (f *RulerFetcher) Fetch(ctx context.Context, ds *datasources.DataSource) (RulerConfig, uint64, error) {
	// Build a service-identity context for the org so the proxy's requester
	// lookup (identity.GetRequester) succeeds. Fetch runs from a background job
	// (the sync worker) or the Config admission validator, neither of which
	// carries a user request context.
	svcCtx, _ := identity.WithServiceIdentity(ctx, ds.OrgID)

	// The proxy derives the upstream path from the request URL by stripping the
	// /api/datasources/proxy/uid/<uid>/ prefix, so embed the ruler config path
	// after it: the datasource then receives config/v1/rules.
	proxyURL := "/api/datasources/proxy/uid/" + ds.UID + rulerConfigAPIPath
	req, err := http.NewRequestWithContext(svcCtx, http.MethodGet, proxyURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create HTTP request: %w", err)
	}
	// Mimir/Cortex serve the ruler config API as YAML.
	req.Header.Set("Accept", "application/yaml")

	// The proxy access-checks c.SignedInUser via DataSourceCache.GetDatasourceByUID.
	// Provide a service identity scoped to the org carrying the datasource
	// query/read permissions the shared service identity already holds, so the
	// background fetch passes the same access control a user-driven proxy would.
	recorder := httptest.NewRecorder()
	c := &contextmodel.ReqContext{
		Context: &web.Context{
			Req:  req,
			Resp: web.NewResponseWriter(req.Method, recorder),
		},
		SignedInUser: serviceIdentityUser(ds.OrgID),
	}

	f.proxy.ProxyDatasourceRequestWithUID(c, ds.UID)

	// 404 → the tenant has no rule groups. Mirrors Grafana's frontend ruler
	// client, which treats 404 as an empty result.
	if recorder.Code == http.StatusNotFound {
		return RulerConfig{}, emptyHash, nil
	}

	if recorder.Code/100 != 2 {
		body := recorder.Body.Bytes()
		if len(body) > 1024 {
			body = body[:1024]
		}
		return nil, 0, fmt.Errorf("%w: unexpected HTTP status %d: %s", ErrNotARuler, recorder.Code, string(body))
	}

	body := recorder.Body.Bytes()
	var cfg RulerConfig
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		return nil, 0, fmt.Errorf("%w: failed to parse response as ruler config: %v", ErrNotARuler, err)
	}

	h := fnv.New64a()
	_, _ = h.Write(body)
	return cfg, h.Sum64(), nil
}

// serviceIdentityUser builds the *user.SignedInUser the datasource proxy
// access-checks (c.SignedInUser). It mirrors the org-scoped service identity
// (identity.WithServiceIdentity) — which yields a *identity.StaticRequester, not
// the *user.SignedInUser the ReqContext requires — carrying the datasource
// query/read permissions that identity already grants, so the background fetch
// passes DataSourceCache.GetDatasourceByUID legitimately rather than bypassing
// the check.
func serviceIdentityUser(orgID int64) *user.SignedInUser {
	return &user.SignedInUser{
		OrgID:          orgID,
		OrgRole:        identity.RoleAdmin,
		Login:          "grafana_external_ruler_sync",
		IsGrafanaAdmin: true,
		Permissions: map[int64]map[string][]string{
			orgID: {
				datasources.ActionQuery: {datasources.ScopeAll},
				datasources.ActionRead:  {datasources.ScopeAll},
			},
		},
	}
}

// emptyHash is the FNV-1a hash of an empty body, used for the no-rules (404)
// case so dedup treats "still empty" as unchanged across ticks.
var emptyHash = func() uint64 {
	h := fnv.New64a()
	return h.Sum64()
}()
