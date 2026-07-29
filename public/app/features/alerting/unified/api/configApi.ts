import { generatedAPI } from '@grafana/api-clients/rtkq/notifications.alerting/v0alpha1';

export const configApi = generatedAPI;

/**
 * Config is a per-org singleton served at this fixed name (backend ConfigSingletonName). Humans
 * cannot create it — create is denied to non-service identities, and a PUT to a missing object is
 * re-authorized as create — so a 404 means "the sync worker has not seeded it yet", not "wrong name".
 */
export const CONFIG_SINGLETON_NAME = 'default';

/**
 * Condition type the sync worker writes onto Config.status; mirrors the backend constant. Lives here
 * so production code and the MSW handlers cannot drift apart on the spelling.
 */
export const SYNCED_CONDITION_TYPE = 'ExternalAlertmanagerSynced';

/**
 * Terminal reason on the Synced condition: the external configuration was imported as a managed
 * route and the worker has stopped syncing. The worker writes it with status=True to preserve the
 * synced-at timestamp, so the reason is the only thing separating a stopped sync from a running one.
 */
export const MERGE_COMMITTED_REASON = 'MergeCommitted';
