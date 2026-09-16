// `sentinel-sourcemaps upload <build dir> <public prefix> [options]` (contract §4.8) as a function of its
// arguments and an `io` — `runCli(argv, io)` resolves to the exit code, so the C rows run in-process against a
// stubbed route with the retry sleeps injected, and src/cli/index.ts is the two lines that bind it to the
// process. Exit codes (step 5): 0 every batch 2xx; 1 any 4xx, or a 5xx / network failure after 4 attempts; 2 a
// usage error — a missing or empty option, a whitespace-padded release, a non-numeric `--batch-size`, an
// unreadable dir, zero maps — always before any request. The key is read from `--api-key` / `SENTINEL_API_KEY`
// and never written anywhere (C11).

import { parseArgs } from 'node:util';
import { isSourcemapUploadError, MAX_ATTEMPTS, type SendFn, type SleepFn, type UploadEvent, uploadSourcemaps } from '../upload/index.js';

export const USAGE = `sentinel-sourcemaps upload <build dir> <public prefix> [options]
  --release <string>        release_version, VERBATIM (env SENTINEL_RELEASE)      required
  --dashboard-url <url>     https://sentinel.daya.ai (env SENTINEL_DASHBOARD_URL) required
  --api-key <string>        (env SENTINEL_API_KEY)                                required
  --project-key <string>    X-Project-Key for an org-wide key (env SENTINEL_PROJECT_KEY)
  --delete                  delete each uploaded *.js.map / *.mjs.map after ITS batch succeeded
  --batch-size <n>          default 50 (the route's per-request file cap); outside [1, 50] → clamped, one warn;
                            non-numeric → exit 2; ≤ 100 MiB serialised per request regardless
  --dry-run                 print the pairs and exit 0 without posting
  --json                    one JSON object per file on stdout
  --help                    this text

Pairs every *.js / *.mjs under <build dir> to the map its trailing //# sourceMappingURL= comment names, under
<public prefix>/<relative path of the chunk> (Turbopack names a map by a different hash than its chunk); a map no
chunk references is paired by its own name, <public prefix>/<relative path without .map>. Posts them to
<dashboard url>/api/sourcemaps in batches of ≤ 50 files and ≤ 100 MiB. Exit 0 when every batch was
stored; 1 on any 4xx (the envelope is printed) or a 5xx / network failure after ${MAX_ATTEMPTS} attempts; 2 for a
usage error, an unreadable dir or zero maps.
`;

/** The reachability rule of docs/sdk-specification.md §8, printed on a network failure (C8). */
export const REACHABILITY_LINE =
  'the dashboard host resolves to the internal ELB: run this from the self-hosted runner pool or the VPN — a GitHub-hosted runner cannot reach it (docs/sdk-specification.md, Reachability)';

export interface CliIo {
  stdout(text: string): void;
  stderr(text: string): void;
  env: Readonly<Record<string, string | undefined>>;
  /** Injected for tests; `node:http(s)` otherwise. */
  send?: SendFn | undefined;
  /** Injected for tests; `setTimeout` otherwise. */
  sleep?: SleepFn | undefined;
}

const OPTIONS = {
  release: { type: 'string' },
  'dashboard-url': { type: 'string' },
  'api-key': { type: 'string' },
  'project-key': { type: 'string' },
  delete: { type: 'boolean' },
  'batch-size': { type: 'string' },
  'dry-run': { type: 'boolean' },
  json: { type: 'boolean' },
  help: { type: 'boolean', short: 'h' },
} as const;

function usageError(io: CliIo, line: string): 2 {
  io.stderr(`sentinel-sourcemaps: ${line}\n`);
  io.stderr(`run \`sentinel-sourcemaps upload --help\` for usage\n`);
  return 2;
}

function firstNonEmpty(...values: Array<string | undefined>): string | undefined {
  for (const v of values) if (v !== undefined) return v;
  return undefined;
}

export async function runCli(argv: readonly string[], io: CliIo): Promise<number> {
  let parsed: ReturnType<typeof parseArgs<{ options: typeof OPTIONS; allowPositionals: true }>>;
  try {
    parsed = parseArgs({ args: [...argv], options: OPTIONS, allowPositionals: true, strict: true });
  } catch (err) {
    return usageError(io, err instanceof Error ? err.message : String(err));
  }
  const { values, positionals } = parsed;
  if (values.help) {
    io.stdout(USAGE);
    return 0;
  }
  const [command, buildDir, publicPrefix, ...extra] = positionals;
  if (command !== 'upload') return usageError(io, command === undefined ? 'missing command: expected `upload`' : `unknown command ${JSON.stringify(command)}: expected \`upload\``);
  if (buildDir === undefined || publicPrefix === undefined) return usageError(io, 'expected two arguments: <build dir> <public prefix>');
  if (extra.length > 0) return usageError(io, `unexpected argument ${JSON.stringify(extra[0])}`);

  // The flag wins over the env; a flag given as '' is an empty release, refused below, not "unset".
  const release = firstNonEmpty(values.release, io.env.SENTINEL_RELEASE);
  const dashboardUrl = firstNonEmpty(values['dashboard-url'], io.env.SENTINEL_DASHBOARD_URL);
  const apiKey = firstNonEmpty(values['api-key'], io.env.SENTINEL_API_KEY);
  const projectKey = firstNonEmpty(values['project-key'], io.env.SENTINEL_PROJECT_KEY);
  if (release === undefined) return usageError(io, 'missing --release (or SENTINEL_RELEASE)');
  if (dashboardUrl === undefined || dashboardUrl === '') return usageError(io, 'missing --dashboard-url (or SENTINEL_DASHBOARD_URL)');
  if (apiKey === undefined || apiKey === '') return usageError(io, 'missing --api-key (or SENTINEL_API_KEY)');

  let batchSize: number | undefined;
  if (values['batch-size'] !== undefined) {
    const raw = values['batch-size'];
    batchSize = raw.trim() === '' ? Number.NaN : Number(raw);
    if (!Number.isFinite(batchSize)) return usageError(io, `--batch-size must be a number, got ${JSON.stringify(raw)}`);
  }

  const json = values.json === true;
  const onEvent = (event: UploadEvent): void => {
    switch (event.type) {
      case 'warn':
        io.stderr(`warning: ${event.line}\n`);
        return;
      case 'retry':
        io.stderr(`attempt ${event.attempt}/${MAX_ATTEMPTS} failed: ${event.line}; retrying in ${event.delayMs / 1000} s\n`);
        return;
      case 'batch': {
        const { batch } = event;
        for (const file of batch.files) {
          if (json) io.stdout(`${JSON.stringify({ minified_path: file.minified_path, outcome: file.outcome, sha256: file.sha256, size_bytes: file.size_bytes })}\n`);
          else io.stdout(`${file.outcome}  ${file.minified_path}  ${file.sha256.slice(0, 12)}  ${file.size_bytes}\n`);
        }
        const retried = batch.attempts > 1 ? ` after ${batch.attempts} attempts` : '';
        const deleted = batch.deleted.length > 0 ? `, ${batch.deleted.length} map(s) deleted` : '';
        io.stderr(`batch ${batch.index + 1}: ${batch.status}, ${batch.sent.length} file(s)${retried}${deleted}\n`);
        return;
      }
    }
  };

  try {
    const report = await uploadSourcemaps({
      buildDir,
      publicPrefix,
      release,
      dashboardUrl,
      apiKey,
      projectKey,
      batchSize,
      deleteAfterUpload: values.delete === true,
      dryRun: values['dry-run'] === true,
      onEvent,
      send: io.send,
      sleep: io.sleep,
    });
    if (report.dryRun) {
      for (const pair of report.pairs) {
        if (json) io.stdout(`${JSON.stringify({ minified_path: pair.minifiedPath, path: pair.path, size_bytes: pair.sizeBytes })}\n`);
        else io.stdout(`${pair.minifiedPath}  ${pair.path}  ${pair.sizeBytes}\n`);
      }
      const summary = `${report.requests} request(s) would be sent for ${report.pairs.length} map(s), release ${report.release}\n`;
      (json ? io.stderr : io.stdout)(summary);
      return 0;
    }
    io.stderr(`${report.files.length} file(s) stored in ${report.requests} request(s), release ${report.release}\n`);
    return 0;
  } catch (err) {
    if (isSourcemapUploadError(err)) {
      io.stderr(`sentinel-sourcemaps: ${err.message}\n`);
      if (err.kind === 'unavailable' && err.status === undefined) io.stderr(`sentinel-sourcemaps: ${REACHABILITY_LINE}\n`);
      if (err.kind === 'rejected') io.stderr(`sentinel-sourcemaps: stopped at the first failing batch; later batches were not sent and nothing of it was deleted\n`);
      return err.exitCode;
    }
    io.stderr(`sentinel-sourcemaps: ${err instanceof Error ? err.message : String(err)}\n`);
    return 1;
  }
}
