// The browser's automatic contexts and request (spec §6.1, TASK-04): read fresh at every capture, each
// reader guarded — a missing API is skipped, never thrown. No `contexts.browser` / `.os` (D2): the ingestor
// derives both from `request.headers["User-Agent"]`.

import { filterQuery, filterUrl } from '../core/url.js';
import type { AutoContexts } from '../core/types.js';

function codePoints(s: string): string[] {
  return Array.from(s);
}

function cutCodePoints(s: string, max: number): string {
  const cps = codePoints(s);
  return cps.length > max ? cps.slice(0, max).join('') : s;
}

function readLocation(): { origin: string; pathname: string; search: string } | undefined {
  try {
    const loc = (globalThis as { location?: unknown }).location as { origin?: unknown; pathname?: unknown; search?: unknown } | undefined;
    if (loc === null || typeof loc !== 'object') return undefined;
    const origin = typeof loc.origin === 'string' ? loc.origin : '';
    const pathname = typeof loc.pathname === 'string' ? loc.pathname : '';
    const search = typeof loc.search === 'string' ? loc.search : '';
    return { origin, pathname, search };
  } catch {
    return undefined;
  }
}

function readNavigatorString(key: 'userAgent' | 'language'): string | undefined {
  try {
    const nav = (globalThis as { navigator?: unknown }).navigator as Record<string, unknown> | undefined;
    const value = nav?.[key];
    return typeof value === 'string' && value !== '' ? value : undefined;
  } catch {
    return undefined;
  }
}

function readTimezone(): string | undefined {
  try {
    const Intl = (globalThis as { Intl?: unknown }).Intl as { DateTimeFormat?: new () => { resolvedOptions(): { timeZone?: unknown } } } | undefined;
    if (typeof Intl?.DateTimeFormat !== 'function') return undefined;
    const tz = new Intl.DateTimeFormat().resolvedOptions().timeZone;
    return typeof tz === 'string' && tz !== '' ? tz : undefined;
  } catch {
    return undefined;
  }
}

function readIntegerProp(obj: unknown, key: string): number | undefined {
  try {
    const v = (obj as Record<string, unknown> | undefined)?.[key];
    return typeof v === 'number' && Number.isFinite(v) ? Math.trunc(v) : undefined;
  } catch {
    return undefined;
  }
}

function readDevice(): Record<string, unknown> | undefined {
  const device: Record<string, unknown> = {};
  try {
    const screen = (globalThis as { screen?: unknown }).screen;
    const width = readIntegerProp(screen, 'width');
    const height = readIntegerProp(screen, 'height');
    if (width !== undefined) device.screen_width_pixels = width;
    if (height !== undefined) device.screen_height_pixels = height;
  } catch {
    // Skipped.
  }
  try {
    const ratio = (globalThis as { devicePixelRatio?: unknown }).devicePixelRatio;
    if (typeof ratio === 'number' && Number.isFinite(ratio)) device.screen_density = ratio;
  } catch {
    // Skipped.
  }
  try {
    const nav = (globalThis as { navigator?: unknown }).navigator as Record<string, unknown> | undefined;
    const memory = nav?.deviceMemory;
    if (typeof memory === 'number' && Number.isFinite(memory)) device.memory_size = Math.trunc(memory * 1_073_741_824);
    const cpus = nav?.hardwareConcurrency;
    if (typeof cpus === 'number' && Number.isFinite(cpus)) device.processor_count = Math.trunc(cpus);
  } catch {
    // Skipped.
  }
  return Object.keys(device).length > 0 ? device : undefined;
}

/** The browser's automatic `request` and `contexts` (spec §6.1). Never throws. */
export function browserAutoContexts(): AutoContexts {
  const out: AutoContexts = {};
  const contexts: Record<string, Record<string, unknown>> = {};

  const locale = readNavigatorString('language');
  const timezone = readTimezone();
  const culture: Record<string, unknown> = {};
  if (locale !== undefined) culture.locale = locale;
  if (timezone !== undefined) culture.timezone = timezone;
  if (Object.keys(culture).length > 0) contexts.culture = culture;

  const device = readDevice();
  if (device !== undefined) contexts.device = device;

  if (Object.keys(contexts).length > 0) out.contexts = contexts;

  const loc = readLocation();
  if (loc !== undefined) {
    const request: NonNullable<AutoContexts['request']> = {};
    const url = cutCodePoints(filterUrl(loc.origin + loc.pathname), 2048);
    request.url = url;
    const rawQuery = loc.search.startsWith('?') ? loc.search.slice(1) : loc.search;
    if (rawQuery !== '') {
      const query = cutCodePoints(filterQuery(rawQuery), 4096);
      if (query !== '') request.query_string = query;
    }
    const ua = readNavigatorString('userAgent');
    if (ua !== undefined) request.headers = { 'User-Agent': cutCodePoints(ua, 4096) };
    if (Object.keys(request).length > 0) out.request = request;
  }

  return out;
}
