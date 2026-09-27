// Installs the five automatic breadcrumb integrations (spec §5, TASK-03) after the client exists, unless the
// client is no-op or `maxBreadcrumbs` is 0. Each installed patch is uninstalled by the client's `teardown`.

import { reportError } from '../../core/report.js';
import type { InternalClient } from '../../core/state.js';
import type { BreadcrumbSwitches } from '../../core/types.js';
import { installClickIntegration } from './click.js';
import { installConsoleIntegration } from './console.js';
import { installFetchIntegration } from './fetch.js';
import { installNavigationIntegration } from './navigation.js';
import { originAndPath } from './shared.js';
import { installXhrIntegration } from './xhr.js';

const NAMES = ['console', 'fetch', 'xhr', 'navigation', 'click'] as const;

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** `false` → every switch off; an object → each member, a non-boolean degrading to `true` (spec §5). */
function resolveSwitches(client: InternalClient, given: unknown): BreadcrumbSwitches | false {
  if (given === false) return false;
  if (given === undefined) return { console: true, fetch: true, xhr: true, navigation: true, click: true };
  if (!isPlainObject(given)) {
    reportError(client, 'invalid_option', `breadcrumbs ${JSON.stringify(given)} is not false or an object; using every integration on`, { option: 'breadcrumbs' });
    return { console: true, fetch: true, xhr: true, navigation: true, click: true };
  }
  const out: BreadcrumbSwitches = {};
  for (const name of NAMES) {
    const value = given[name];
    if (value === undefined) {
      out[name] = true;
    } else if (typeof value !== 'boolean') {
      reportError(client, 'invalid_option', `breadcrumbs.${name} ${JSON.stringify(value)} is not a boolean; using true`, { option: `breadcrumbs.${name}` });
      out[name] = true;
    } else {
      out[name] = value;
    }
  }
  return out;
}

/**
 * Install every integration `switches` (or the default all-on) enables. `selfTargetUrl` is the plan's send
 * target (browser tunnel) — the fetch/XHR integrations never record a request to it. Returns one combined
 * uninstall.
 */
export function installBrowserIntegrations(client: InternalClient, given: unknown, selfTargetUrl: string | undefined): () => void {
  if (client.options.maxBreadcrumbs === 0) return () => undefined;
  const switches = resolveSwitches(client, given);
  if (switches === false) return () => undefined;
  const selfTarget = selfTargetUrl === undefined ? undefined : originAndPath(selfTargetUrl);
  const uninstalls: Array<() => void> = [];
  if (switches.console !== false) uninstalls.push(installConsoleIntegration(client));
  if (switches.fetch !== false) uninstalls.push(installFetchIntegration(client, selfTarget));
  if (switches.xhr !== false) uninstalls.push(installXhrIntegration(client, selfTarget));
  if (switches.navigation !== false) uninstalls.push(installNavigationIntegration(client));
  if (switches.click !== false) uninstalls.push(installClickIntegration(client));
  return () => {
    for (const u of uninstalls) u();
  };
}
