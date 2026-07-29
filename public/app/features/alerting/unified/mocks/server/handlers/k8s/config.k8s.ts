import { HttpResponse, http } from 'msw';
import { type SetupServer } from 'msw/node';

import { API_GROUP, API_VERSION, type Config } from '@grafana/api-clients/rtkq/notifications.alerting/v0alpha1';
import {
  CONFIG_SINGLETON_NAME,
  type MERGE_COMMITTED_REASON,
  SYNCED_CONDITION_TYPE,
} from 'app/features/alerting/unified/api/configApi';
import { ALERTING_API_SERVER_BASE_URL } from 'app/features/alerting/unified/mocks/server/utils';

const CONFIG_URL = `${ALERTING_API_SERVER_BASE_URL}/namespaces/:namespace/configs/:name`;

/**
 * Reasons the worker actually writes onto the condition. These are the PascalCase strings produced
 * by SyncReason.ConditionReason() in external_am_syncer.go — NOT the snake_case Go enum values
 * (`mimir_fetch`, …), which never leave the backend. The ones production code matches on come from
 * the shared constant, so a handler and the code reading it cannot drift apart on the spelling.
 */
type SyncConditionReason =
  | 'SyncSucceeded'
  | typeof MERGE_COMMITTED_REASON
  | 'NotConfigured'
  | 'DatasourceLookupFailed'
  | 'MimirFetchFailed'
  | 'ValidationFailed'
  | 'SaveFailed'
  | 'IdentifierMismatch'
  | 'NoUpstreamConfig'
  | 'ConfigReadFailed'
  | 'SyncFailed';

interface SyncConditionOptions {
  status: 'True' | 'False' | 'Unknown';
  reason: SyncConditionReason;
  message?: string;
}

interface AutoSyncConfigOptions {
  /**
   * UID on spec.externalAlertmanagerSync — the desired configuration, and the field
   * `useIsAutoSyncActive` reads. Set it to simulate an active sync; omit it for an inactive sync.
   */
  specUid?: string;
  /**
   * UID on status.externalAlertmanagerSync — the last sync attempt, which can lag spec.
   * `useIsAutoSyncActive` only reads this when `origin` is 'ini'; set it with the default
   * `origin: 'api'` (and specUid omitted) to simulate a stale status that must not count as active.
   */
  statusUid?: string;
  /**
   * origin on status.externalAlertmanagerSync. 'ini' marks the org as operator-managed: the
   * grafana.ini key is authoritative, spec is dormant, and UID writes are rejected on admission.
   */
  origin?: 'api' | 'ini';
  /** The ExternalAlertmanagerSynced condition. Omit for a Config with no conditions recorded yet. */
  condition?: SyncConditionOptions;
}

function buildAutoSyncConfig(name: string, options: AutoSyncConfigOptions = {}): Config {
  const { specUid, statusUid, origin = 'api', condition } = options;
  return {
    apiVersion: `${API_GROUP}/${API_VERSION}`,
    kind: 'Config',
    metadata: { name, namespace: 'default', resourceVersion: '1' },
    spec: specUid ? { externalAlertmanagerSync: { datasourceUid: specUid } } : {},
    status: {
      ...(statusUid ? { externalAlertmanagerSync: { datasourceUid: statusUid, origin } } : {}),
      ...(condition
        ? {
            conditions: [
              {
                type: SYNCED_CONDITION_TYPE,
                status: condition.status,
                reason: condition.reason,
                ...(condition.message ? { message: condition.message } : {}),
                lastTransitionTime: '2026-01-01T00:00:00.000Z',
              },
            ],
          }
        : {}),
    },
  };
}

function notFoundStatus() {
  return {
    kind: 'Status',
    apiVersion: 'v1',
    metadata: {},
    status: 'Failure',
    message: `configs.notifications.alerting.grafana.app "${CONFIG_SINGLETON_NAME}" not found`,
    reason: 'NotFound',
    code: 404,
  };
}

const configHandler = (options: AutoSyncConfigOptions = {}, onRequest?: () => void) =>
  http.get<{ namespace: string; name: string }>(CONFIG_URL, ({ params }) => {
    onRequest?.();
    return HttpResponse.json(buildAutoSyncConfig(params.name, options));
  });

/**
 * Override the Config GET to drive external Alertmanager auto-sync state. Pass `specUid` to
 * simulate an API-configured active sync; omit it for an inactive, empty Config. Pass `statusUid`
 * alone to simulate a stale status that must not count as active. Pass `origin: 'ini'` with
 * `statusUid` for an operator-managed org (also an active sync), and `condition` to drive the badge.
 *
 * Returns a `requestSpy` that fires on every GET, so a test can assert the query was — or, when a
 * permission gate should short-circuit it, was not — made.
 */
export function setupAutoSyncConfig(server: SetupServer, options: AutoSyncConfigOptions = {}) {
  const requestSpy = jest.fn();
  server.use(configHandler(options, requestSpy));
  return { requestSpy };
}

/**
 * Make the Config singleton 404 on both read and write, simulating an instance where the sync
 * worker has not seeded it yet. The UI must read this as "unconfigured" and refuse to save with a
 * transient "still initializing" message, because humans cannot create the singleton.
 */
export function setupAutoSyncConfigAbsent(server: SetupServer) {
  const requestSpy = jest.fn();
  server.use(
    http.get<{ namespace: string; name: string }>(CONFIG_URL, () => {
      requestSpy();
      return HttpResponse.json(notFoundStatus(), { status: 404 });
    }),
    http.patch<{ namespace: string; name: string }>(CONFIG_URL, () =>
      HttpResponse.json(notFoundStatus(), { status: 404 })
    )
  );
  return { requestSpy };
}

/** A single JSON Patch operation, as the UI sends them. */
interface PatchOperation {
  op: string;
  path: string;
  value?: unknown;
}

/**
 * Stateful Config handlers: PATCH applies to the stored object and GET serves it, so a refetch after
 * RTKQ tag invalidation observes the saved UID without a manual handler swap.
 *
 * Every write bumps `metadata.resourceVersion`, as a real apiserver does. The UI must not depend on
 * that value: the sync worker bumps it on every poll tick, so a write that pins resourceVersion is
 * rejected with a spurious 409 whenever a tick lands between page load and save.
 *
 * Returns `patchSpy` (called with each JSON Patch body) and `getStored` for asserting persisted state.
 */
export function setupStatefulAutoSyncConfig(server: SetupServer, options: AutoSyncConfigOptions = {}) {
  let stored = buildAutoSyncConfig(CONFIG_SINGLETON_NAME, options);
  const patchSpy = jest.fn();

  server.use(
    http.get<{ namespace: string; name: string }>(CONFIG_URL, () => HttpResponse.json(stored)),
    http.patch<{ namespace: string; name: string }, PatchOperation[]>(CONFIG_URL, async ({ request }) => {
      const operations = await request.json();
      patchSpy(operations);

      let spec = stored.spec;
      for (const operation of operations) {
        if (operation.path === '/spec/externalAlertmanagerSync') {
          spec = { ...spec, externalAlertmanagerSync: operation.value as Config['spec']['externalAlertmanagerSync'] };
        }
      }

      const nextVersion = String(Number(stored.metadata.resourceVersion ?? '1') + 1);
      stored = { ...stored, spec, metadata: { ...stored.metadata, resourceVersion: nextVersion } };
      return HttpResponse.json(stored);
    })
  );

  return { patchSpy, getStored: () => stored };
}

/**
 * Make the Config write fail with an apimachinery Status — e.g. the admission rejection an
 * operator-managed org returns when a UID write is attempted.
 */
export function setupAutoSyncConfigWriteError(
  server: SetupServer,
  { code, message }: { code: number; message: string }
) {
  server.use(
    http.patch<{ namespace: string; name: string }>(CONFIG_URL, () =>
      HttpResponse.json(
        { kind: 'Status', apiVersion: 'v1', metadata: {}, status: 'Failure', reason: 'Forbidden', code, message },
        { status: code }
      )
    )
  );
}

const handlers = [configHandler()];
export default handlers;
