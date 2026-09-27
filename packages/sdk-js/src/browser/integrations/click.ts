// The click breadcrumb integration (spec §5.6, TASK-03).

import type { InternalClient } from '../../core/state.js';
import { isRecording, record } from './shared.js';

const MAX_DEPTH = 5;
const MAX_LENGTH = 80;
const DEBOUNCE_MS = 1000;

interface ElementLike {
  tagName?: unknown;
  id?: unknown;
  classList?: unknown;
  parentElement?: unknown;
  nodeType?: unknown;
  parentNode?: unknown;
}

function asElement(target: unknown): ElementLike | undefined {
  let node = target as ElementLike | null | undefined;
  // A text node's parent is the element (spec §5.6).
  if (node !== null && node !== undefined && node.nodeType === 3) node = node.parentNode as ElementLike | undefined;
  if (node === null || node === undefined || typeof node !== 'object') return undefined;
  if (typeof node.tagName !== 'string') return undefined;
  return node;
}

function renderOne(el: ElementLike): string {
  let out = '';
  try {
    out += String(el.tagName).toLowerCase();
  } catch {
    return out;
  }
  try {
    if (typeof el.id === 'string' && el.id !== '') out += `#${el.id}`;
  } catch {
    // Skipped.
  }
  try {
    const classList = el.classList as { [Symbol.iterator]?: unknown } | undefined;
    if (classList !== null && classList !== undefined && typeof classList[Symbol.iterator] === 'function') {
      for (const cls of classList as unknown as Iterable<string>) out += `.${cls}`;
    }
  } catch {
    // Skipped.
  }
  return out;
}

function isBoundary(el: ElementLike): boolean {
  try {
    const tag = String(el.tagName).toLowerCase();
    return tag === 'html' || tag === 'body';
  } catch {
    return true;
  }
}

/** The selector text for `event.target`, spec §5.6's shape. */
export function selectorOf(target: unknown): string {
  const start = asElement(target);
  if (start === undefined) return '';
  const chain: string[] = [];
  let current: ElementLike | undefined = start;
  let depth = 0;
  while (current !== undefined && depth < MAX_DEPTH && !(depth > 0 && isBoundary(current))) {
    chain.unshift(renderOne(current));
    depth++;
    let parent: ElementLike | undefined;
    try {
      parent = current.parentElement as ElementLike | undefined;
    } catch {
      parent = undefined;
    }
    if (parent === undefined || parent === null || isBoundary(parent)) break;
    current = parent;
  }
  let selector = chain.join(' > ');
  // Growing selector: if it exceeds 80, stop growing at the last ancestor that still fits, keeping the target.
  while (chain.length > 1 && Array.from(selector).length > MAX_LENGTH) {
    chain.shift();
    selector = chain.join(' > ');
  }
  return Array.from(selector).length > MAX_LENGTH ? Array.from(selector).slice(0, MAX_LENGTH).join('') : selector;
}

/** Installs the click integration; returns its uninstall function. */
export function installClickIntegration(client: InternalClient): () => void {
  const doc = (globalThis as { document?: unknown }).document;
  if (doc === null || typeof doc !== 'object' || typeof (doc as { addEventListener?: unknown }).addEventListener !== 'function') return () => undefined;
  const target = doc as unknown as EventTarget;

  let active = true;
  let lastTarget: unknown;
  let lastAt = 0;

  const onClick = (ev: Event): void => {
    if (!active || isRecording()) return;
    try {
      const evTarget = (ev as { target?: unknown }).target;
      const now = Date.now();
      if (evTarget === lastTarget && now - lastAt < DEBOUNCE_MS) return;
      lastTarget = evTarget;
      lastAt = now;
      const selector = selectorOf(evTarget);
      if (selector === '') return;
      record(client, { type: 'default', category: 'ui.click', level: 'info', message: selector }, { event: ev });
    } catch {
      // A recording failure never reaches the page.
    }
  };

  try {
    target.addEventListener('click', onClick, { capture: true, passive: true });
  } catch {
    return () => undefined;
  }
  return () => {
    active = false;
    try {
      target.removeEventListener('click', onClick, { capture: true } as EventListenerOptions);
    } catch {
      // Nothing more to undo.
    }
  };
}
