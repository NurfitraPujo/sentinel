// The walker and the pairing of contract §4.8 step 1, as amended by BUG-01 (the seat's decision 43): the map a
// chunk's OWN `//# sourceMappingURL=` comment names is uploaded under the CHUNK's path. Turbopack 15.5.21 names a
// client chunk's map by a different hash than the chunk (`d69eb7d85499027c.js` → `8f15d5d03090a0be.js.map`, no
// `file` field, no `<chunk>.js.map` anywhere), so `docs/sdk-specification.md` §8's name-based loop stored paths no
// browser frame ever carries and B7 never symbolicated on a Turbopack build. Webpack writes `<chunk>.js.map`
// beside `<chunk>.js` with a comment saying so, so for a webpack tree the two rules agree byte for byte.
//
// The rule, in walk order (directory entries sorted by name, depth-first — so C2's "three requests (50, 50, 20),
// in order" is deterministic and a re-run packs the same batches):
//   - every `*.js` / `*.mjs` under `<build dir>` whose trailing `sourceMappingURL` comment (percent-decoded —
//     Turbopack writes `%40` / `%5B` for `@` / `[` in server comments; `data:` and `http(s):` URLs ignored)
//     resolves, relative to the chunk's directory, to a `.map` that exists → one pair under
//     `<public prefix, trailing / stripped> + '/' + <chunk's relative path>`; a chunk whose comment names a
//     file that does not exist is skipped with one warn each; chunks with no comment at all (Next's manifest
//     `.js` files, the 3 of 19 Turbopack client chunks with no map) are skipped with ONE warn per walk naming
//     them, not one per file;
//   - every `*.js.map` / `*.mjs.map` no chunk referenced keeps the name-based pairing (`<relative path without
//     .map>`), uploaded even when its sibling `.js` is missing — `siblingExists: false` is the caller's warn,
//     never a skip (the original step 1);
//   - a map two chunks reference is one pair per chunk (the route is keyed by `minified_path`); the uploader
//     unlinks it once, after the last batch that carried it;
//   - an EMPTY INDEX MAP — no string `mappings` and a `sections` that is absent or `[]` — is left out with one
//     warn per file (BUG-02, the seat's decision 44). Turbopack 15.5.21 writes `{"version":3,"sources":[],
//     "sections":[]}` for every one-line server entry stub (`app/*/page.js`, `pages/_app.js`, `instrumentation.js`,
//     `app/api/*/route.js` — 10 of the scratch app's 62 server maps), the route answers `422 invalid_sourcemap
//     no_mappings` for the whole batch, and decision 29 turns that into a red build with nothing to show for it:
//     there is nothing in such a map to symbolicate. A NON-empty `sections` map is still uploaded — the route's
//     422 is then the loud signal that index-map support is missing on the sentinel side, not something to hide.
// The prefix need not be a URL path: §4.9's server walk passes `serverRoot` (`/app/.next/server`, decision 18)
// and gets the absolute filesystem path the node runtime prints. Only regular files are walked (`find -type
// f`); a `.css.map` is ignored here — sweeping every `*.map` out of a Next output is the wrapper's job (§4.9
// step 4, `listMaps`).

import { existsSync } from 'node:fs';
import { open, readdir, readFile, stat } from 'node:fs/promises';
import { dirname, join, relative, resolve, sep } from 'node:path';
import { SourcemapUploadError } from './errors.js';

export interface MapPair {
  /** The map on disk, absolute or as given. */
  readonly path: string;
  /** The `minified_path` the route stores (after its own scheme/host strip). */
  readonly minifiedPath: string;
  /** `stat().size` — what the packer budgets with (contract §4.8 step 3). */
  readonly sizeBytes: number;
  /**
   * `comment`: the chunk's `sourceMappingURL` named this map and `minifiedPath` is the chunk's path;
   * `name`: no chunk referenced the map, so it is paired by its own name (`<path without .map>`).
   */
  readonly pairedBy: 'comment' | 'name';
  /** The chunk on disk for a `comment` pair; for a `name` pair, `<path without .map>`, which may not exist. */
  readonly chunkPath: string;
  /** Whether `chunkPath` exists; `false` (only ever for a `name` pair) is a `warn`, never a skip. */
  readonly siblingExists: boolean;
}

export interface WalkOptions {
  /** Receives the skip lines — a comment naming a missing file, the chunks with no comment. */
  readonly onWarn?: ((line: string) => void) | undefined;
}

/** `.js.map` / `.mjs.map` — the two suffixes the walk pairs (contract §4.8 step 1). */
export function isSourcemapOfScript(name: string): boolean {
  return name.endsWith('.js.map') || name.endsWith('.mjs.map');
}

/** `.js` / `.mjs` — the chunks whose trailing comment the walk reads. */
export function isScript(name: string): boolean {
  return name.endsWith('.js') || name.endsWith('.mjs');
}

/** `<prefix, trailing / stripped>/<relative path>` — the spec doc's `$prefix/${rel%.map}` with the `.map` already off. */
export function pairMinifiedPath(publicPrefix: string, relativeChunkPath: string): string {
  return `${publicPrefix.replace(/\/+$/, '')}/${relativeChunkPath}`;
}

/** How much of a chunk's tail is searched for the comment; a map URL is far shorter, an inline `data:` map is ignored either way. */
export const SOURCEMAPPING_URL_TAIL_BYTES = 4096;

const COMMENT = /\/[/*][@#]\s*sourceMappingURL=([^\s*]+)/g;
const HAS_SCHEME = /^[a-z][a-z0-9+.-]*:/i;

/**
 * The last `//# sourceMappingURL=<url>` (or `/*# … *\/`) in the chunk's final 4 KiB, percent-decoded, or
 * `undefined` when there is none or it is a `data:` / `http(s):` / other URL with a scheme (nothing on disk to
 * upload). A malformed percent-escape is left as written.
 */
export function parseSourceMappingUrl(tail: string): string | undefined {
  let last: string | undefined;
  for (const m of tail.matchAll(COMMENT)) last = m[1];
  if (last === undefined) return undefined;
  const bare = last.replace(/[?#].*$/, '');
  if (bare === '' || HAS_SCHEME.test(bare)) return undefined;
  try {
    return decodeURIComponent(bare);
  } catch {
    return bare;
  }
}

async function readTail(path: string, size: number): Promise<string> {
  const length = Math.min(size, SOURCEMAPPING_URL_TAIL_BYTES);
  if (length === 0) return '';
  const fh = await open(path, 'r');
  try {
    const buf = Buffer.alloc(length);
    const { bytesRead } = await fh.read(buf, 0, length, size - length);
    return buf.subarray(0, bytesRead).toString('utf8');
  } finally {
    await fh.close();
  }
}

/** Every regular file under `dir` (depth-first, entries sorted by name), as `[absolute path, relative posix path]`. */
async function walkFiles(dir: string): Promise<Array<[string, string]>> {
  const out: Array<[string, string]> = [];
  const visit = async (current: string): Promise<void> => {
    const entries = await readdir(current, { withFileTypes: true });
    entries.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
    for (const entry of entries) {
      const full = join(current, entry.name);
      if (entry.isDirectory()) await visit(full);
      else if (entry.isFile()) out.push([full, relative(dir, full).split(sep).join('/')]);
    }
  };
  await visit(dir);
  return out;
}

/**
 * The CLI's exit-2 classes for the directory (contract §4.8 steps 2, 5): one that does not exist holds no map
 * (`no_maps` — the wrapper's W13 names the flag that should have produced it); one that exists but cannot be
 * read, or is not a directory, is "unreadable dir" (`usage`).
 */
async function assertDirectory(dir: string): Promise<void> {
  let info;
  try {
    info = await stat(dir);
  } catch (err) {
    const code = (err as { code?: string }).code;
    if (code === 'ENOENT' || code === 'ENOTDIR') throw new SourcemapUploadError('no_maps', `no *.js.map under ${dir} (it does not exist)`, { cause: err });
    throw new SourcemapUploadError('usage', `cannot read ${dir}: ${code ?? String(err)}`, { cause: err });
  }
  if (!info.isDirectory()) throw new SourcemapUploadError('usage', `cannot read ${dir}: not a directory`);
}

/** The most names one skip line spells out; the rest is a count. */
const NAMED_SKIPS = 10;

/**
 * The largest map the empty-index-map check reads and parses. Turbopack's stubs are ≈ 40 bytes; a real server map
 * runs 1–7 MB and parsing every one of `web`'s ≈ 800 in the build hook would cost more than the upload. A map over
 * this size is uploaded unread and the route judges it.
 */
export const EMPTY_INDEX_MAP_MAX_BYTES = 64 * 1024;

/**
 * `true` for a version-3 index map with nothing to symbolicate: no string `mappings` and a `sections` that is
 * absent or an empty array (`{"version":3,"sources":[],"sections":[]}`). A map that is not JSON, not an object,
 * larger than `EMPTY_INDEX_MAP_MAX_BYTES`, has a `mappings` string (even `""`, which the route accepts) or a
 * non-empty `sections` is `false` — it is uploaded as written and the route's structural check judges it.
 */
export async function isEmptyIndexMap(path: string, sizeBytes: number): Promise<boolean> {
  if (sizeBytes > EMPTY_INDEX_MAP_MAX_BYTES) return false;
  let parsed: unknown;
  try {
    parsed = JSON.parse(await readFile(path, 'utf8'));
  } catch {
    return false;
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) return false;
  const map = parsed as Record<string, unknown>;
  if (typeof map.mappings === 'string') return false;
  return map.sections === undefined || (Array.isArray(map.sections) && map.sections.length === 0);
}

/** The pairs of contract §4.8 step 1 (as amended by BUG-01), in walk order. `usage` when the directory cannot be read; `[]` when it holds no map. */
export async function walkSourcemaps(buildDir: string, publicPrefix: string, options: WalkOptions = {}): Promise<MapPair[]> {
  await assertDirectory(buildDir);
  const warn = options.onWarn ?? (() => {});
  const files = await walkFiles(buildDir);
  // Keyed by the resolved path, so a comment's target (resolved against the chunk's directory) meets the walk's
  // own entry whether `buildDir` was given relative or absolute.
  const sizes = new Map<string, number>();
  for (const [path] of files) sizes.set(resolve(path), (await stat(path)).size);

  // Pass 1: every chunk's comment, resolved against the chunk's directory.
  const referenced = new Set<string>();
  const chunkMap = new Map<string, string>();
  const withoutComment: string[] = [];
  for (const [path, rel] of files) {
    if (!isScript(rel)) continue;
    const abs = resolve(path);
    const url = parseSourceMappingUrl(await readTail(path, sizes.get(abs) ?? 0));
    if (url === undefined) {
      withoutComment.push(rel);
      continue;
    }
    const mapPath = resolve(dirname(abs), url);
    if (!sizes.has(mapPath) && !existsSync(mapPath)) {
      warn(`${path}: its sourceMappingURL names ${url}, which does not exist; skipping the chunk`);
      continue;
    }
    chunkMap.set(abs, mapPath);
    referenced.add(mapPath);
  }
  if (withoutComment.length > 0) {
    const named = withoutComment.slice(0, NAMED_SKIPS).join(', ');
    const more = withoutComment.length > NAMED_SKIPS ? `, +${withoutComment.length - NAMED_SKIPS} more` : '';
    warn(`${withoutComment.length} script(s) under ${buildDir} carry no sourceMappingURL comment and have no map to upload: ${named}${more}`);
  }

  // Pass 2: the pairs, in walk order of the file that produced each. An empty index map is checked once per file
  // (a map two chunks reference is one read) and left out with one warn naming it.
  const emptyChecked = new Map<string, boolean>();
  const skipIfEmpty = async (key: string, mapPath: string, sizeBytes: number): Promise<boolean> => {
    let empty = emptyChecked.get(key);
    if (empty === undefined) {
      empty = await isEmptyIndexMap(mapPath, sizeBytes);
      emptyChecked.set(key, empty);
      if (empty) warn(`${mapPath}: empty index map (no mappings, no sections) — nothing to symbolicate; skipping it`);
    }
    return empty;
  };
  const pairs: MapPair[] = [];
  for (const [path, rel] of files) {
    const abs = resolve(path);
    const mapPath = chunkMap.get(abs);
    if (mapPath !== undefined) {
      const sizeBytes = sizes.get(mapPath) ?? (await stat(mapPath)).size;
      if (await skipIfEmpty(mapPath, mapPath, sizeBytes)) continue;
      pairs.push({ path: mapPath, minifiedPath: pairMinifiedPath(publicPrefix, rel), sizeBytes, pairedBy: 'comment', chunkPath: path, siblingExists: true });
    } else if (isSourcemapOfScript(rel) && !referenced.has(abs)) {
      const sizeBytes = sizes.get(abs) ?? 0;
      if (await skipIfEmpty(abs, path, sizeBytes)) continue;
      const chunkPath = path.slice(0, -'.map'.length);
      pairs.push({
        path,
        minifiedPath: pairMinifiedPath(publicPrefix, rel.slice(0, -'.map'.length)),
        sizeBytes,
        pairedBy: 'name',
        chunkPath,
        siblingExists: existsSync(chunkPath),
      });
    }
  }
  return pairs;
}

/** Every `*.map` of any kind under `dir` — the wrapper's sweep (contract §4.9 step 4). A missing directory is empty. */
export async function listMaps(dir: string): Promise<string[]> {
  if (!existsSync(dir)) return [];
  const files = await walkFiles(dir);
  return files.filter(([, rel]) => rel.endsWith('.map')).map(([path]) => path);
}
