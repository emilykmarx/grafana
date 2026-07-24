import { t } from '@grafana/i18n';
import { Alert } from '@grafana/ui';

import { type PromoteStatsSummary } from './types';

/**
 * Summary of how many resources a promote will merge into the live config, shown on the
 * review screen and the settings promote-confirmation modal. Lists only the resource types
 * that are actually present in the import.
 */
export function PromoteMergeSummary({ stats }: { stats: PromoteStatsSummary }) {
  const items = [
    stats.receivers > 0 &&
      t('alerting.import-to-gma.review.merge-receivers', '', {
        count: stats.receivers,
        defaultValue_one: '{{count}} contact point',
        defaultValue_other: '{{count}} contact points',
      }),
    stats.templates > 0 &&
      t('alerting.import-to-gma.review.merge-templates', '', {
        count: stats.templates,
        defaultValue_one: '{{count}} template',
        defaultValue_other: '{{count}} templates',
      }),
    stats.timeIntervals > 0 &&
      t('alerting.import-to-gma.review.merge-time-intervals', '', {
        count: stats.timeIntervals,
        defaultValue_one: '{{count}} mute timing',
        defaultValue_other: '{{count}} mute timings',
      }),
    stats.inhibitionRules > 0 &&
      t('alerting.import-to-gma.review.merge-inhibition-rules', '', {
        count: stats.inhibitionRules,
        defaultValue_one: '{{count}} inhibition rule',
        defaultValue_other: '{{count}} inhibition rules',
      }),
    stats.route && t('alerting.import-to-gma.review.merge-route', 'a notification route'),
  ].filter((item): item is string => Boolean(item));

  if (items.length === 0) {
    return null;
  }

  return (
    <Alert
      severity="warning"
      title={t('alerting.import-to-gma.review.merge-summary', 'Will merge into your live config: {{summary}}', {
        summary: items.join(', '),
      })}
    />
  );
}
