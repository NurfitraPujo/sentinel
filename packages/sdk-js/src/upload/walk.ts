// The walker and the positional pairing of contract §4.8 step 1 — `docs/sdk-specification.md` §8's shell loop
// made a function: every `*.js.map` / `*.mjs.map` under `<build dir>`, recursively, paired to
// `<public prefix, trailing / stripped> + '/' + <relative path without .map>`. The prefix need not be a URL
// path: §4.9's server walk passes `serverRoot` (`/app/.next/server`, decision 18) and gets the absolute
// filesystem path the node runtime prints. Only regular files are walked (`find -type f`); a `.css.map` is
// ignored here — sweeping every `*.map` out of a Next output is the wrapper's job (§4.9 step 4, `listMaps`).
//
// The order is the walk's: directory entries sorted by name, depth-first — so C2's "three requests (50, 50,
// 20), in order" is deterministic and a re-run packs the same batches.

import { existsSync } from 'node:fs';
import { readdir, stat } from 'node:fs/promises';
import { join, relative, sep } from 'node:path';
import { SourcemapUploadError } from './errors.js';

export interface MapPair {
  /** The map on disk, absolute or as given. */
  readonly path: string;
  /** The `minified_path` the route stores (after its own scheme/host strip). */
  readonly minifiedPath: string;
  /** `stat().size` — what the packer budgets with (contract §4.8 step 3). */
  readonly sizeBytes: number;
  /** Whether `<path without .map>` exists beside the map; `false` is a `warn`, never a skip (step 1). */
  readonly siblingExists: boolean;
}

/** `.js.map` / `.mjs.map` — the two suffixes the walk pairs (contract §4.8 step 1). */
export function isSourcemapOfScript(name: string): boolean {
  return name.endsWith('.js.map') || name.endsWith('.mjs.map');
}

/** `<prefix, trailing / stripped>/<relative path without .map>` — the spec doc's `$prefix/${rel%.map}`. */
export function pairMinifiedPath(publicPrefix: string, relativeMapPath: string): string {
  return `${publicPrefix.replace(/\/+$/, '')}/${relativeMapPath.slice(0, -'.map'.length)}`;
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

/** The pairs of contract §4.8 step 1, in walk order. `usage` when the directory cannot be read; `[]` when it holds no map. */
export async function walkSourcemaps(buildDir: string, publicPrefix: string): Promise<MapPair[]> {
  await assertDirectory(buildDir);
  const pairs: MapPair[] = [];
  for (const [path, rel] of await walkFiles(buildDir)) {
    if (!isSourcemapOfScript(rel)) continue;
    const info = await stat(path);
    pairs.push({
      path,
      minifiedPath: pairMinifiedPath(publicPrefix, rel),
      sizeBytes: info.size,
      siblingExists: existsSync(path.slice(0, -'.map'.length)),
    });
  }
  return pairs;
}

/** Every `*.map` of any kind under `dir` — the wrapper's sweep (contract §4.9 step 4). A missing directory is empty. */
export async function listMaps(dir: string): Promise<string[]> {
  if (!existsSync(dir)) return [];
  const files = await walkFiles(dir);
  return files.filter(([, rel]) => rel.endsWith('.map')).map(([path]) => path);
}
