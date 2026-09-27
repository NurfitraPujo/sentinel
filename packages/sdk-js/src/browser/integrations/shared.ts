// Shared plumbing for the five automatic breadcrumb integrations (spec §5, TASK-03): the re-entrancy flag
// ("nothing is recorded while it is set"), the wrap/uninstall pattern ("wrap, never replace behaviour"; a
// second `init` never stacks two SDK wrappers; uninstall restores the original only when the global still
// holds the SDK's own wrapper), and the self-fetch/XHR exclusion (§5.3/§5.4: the SDK's own sends are never
// recorded).

import { addBreadcrumbToScope } from '../../core/breadcrumbs.js';
import { getSlot, type InternalClient } from '../../core/state.js';
import type { BreadcrumbHint } from '../../core/types.js';

let recording = false;

/** True while the SDK itself is recording a crumb or writing a debug line — nothing is recorded during it. */
export function isRecording(): boolean {
  return recording;
}

/** Record one crumb through the same buffer `addBreadcrumb` uses, with the re-entrancy flag held. */
export function record(client: InternalClient, crumb: unknown, hint: BreadcrumbHint): void {
  if (recording) return;
  recording = true;
  try {
    addBreadcrumbToScope(client, getSlot().scope, crumb, hint);
  } catch {
    // A recording failure never reaches the page (spec §5).
  } finally {
    recording = false;
  }
}

/**
 * Wrap `obj[name]` (a function), returning an uninstall function. `makeWrapper` receives the original and an
 * `active()` check the wrapper must consult before recording (never before calling the original — the original
 * always runs). Uninstall restores the original only when the global still holds this wrapper; otherwise
 * (someone wrapped after the SDK) the wrapper is left in place and `active()` starts returning `false`, so it
 * becomes a pass-through.
 */
export function wrapMethod<T extends (...args: never[]) => unknown>(obj: Record<string, unknown>, name: string, makeWrapper: (original: T, active: () => boolean) => T): () => void {
  const original = obj[name];
  if (typeof original !== 'function') return () => undefined;
  let active = true;
  const wrapper = makeWrapper(original as T, () => active);
  obj[name] = wrapper;
  return () => {
    active = false;
    if (obj[name] === wrapper) obj[name] = original;
  };
}

/** `url`'s origin + pathname, or `undefined` when it does not parse — for the fetch/XHR self-exclusion check. */
export function originAndPath(url: string): string | undefined {
  try {
    const u = new URL(url);
    return u.origin + u.pathname;
  } catch {
    return undefined;
  }
}
