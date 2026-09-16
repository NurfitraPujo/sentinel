// The failure classes of contract §4.8 step 5, as one error the CLI maps to an exit code and the wrapper (§4.9
// step 5) maps to fail-or-warn:
//
//   usage        exit 2 — a missing / empty / whitespace-padded release, an unreadable dir, a bad option
//   no_maps      exit 2 — zero `*.js.map` under the dir (the wrapper's W13 loud case, decision 32)
//   rejected     exit 1 — any 4xx: the envelope printed, stop at the first failing batch (decision 29)
//   unavailable  exit 1 — every 5xx and every network failure, whatever the body, after 4 attempts (decision 34)
//
// `message` is the printable line: `<status> <code>: <message> <details as JSON>` for an envelope, `<status>
// <text ≤ 4 KiB>` for any other body, `connection failed (<code>)` for a network failure. The envelope `code`
// rides along as a field for the reader — it is printed, never branched on (decision 34).

export type UploadFailureKind = 'usage' | 'no_maps' | 'rejected' | 'unavailable';

export interface UploadFailureInit {
  status?: number | undefined;
  attempts?: number | undefined;
  code?: string | undefined;
  details?: unknown;
  text?: string | undefined;
  cause?: unknown;
}

export class SourcemapUploadError extends Error {
  override readonly name = 'SourcemapUploadError';
  readonly kind: UploadFailureKind;
  /** `2` for usage / no_maps, `1` for rejected / unavailable (contract §4.8 step 5). */
  readonly exitCode: 1 | 2;
  readonly status: number | undefined;
  readonly attempts: number | undefined;
  /** The envelope's `code`, when the body was one. */
  readonly code: string | undefined;
  readonly details: unknown;
  /** The body as text (≤ 4 KiB) when it was not an envelope. */
  readonly text: string | undefined;
  override readonly cause: unknown;

  constructor(kind: UploadFailureKind, message: string, init: UploadFailureInit = {}) {
    super(message);
    this.kind = kind;
    this.exitCode = kind === 'usage' || kind === 'no_maps' ? 2 : 1;
    this.status = init.status;
    this.attempts = init.attempts;
    this.code = init.code;
    this.details = init.details;
    this.text = init.text;
    this.cause = init.cause;
  }
}

/** Duck-typed, so a `vi.mock`ed uploader or a second copy of the bundle still classifies (the wrapper's step 5). */
export function isSourcemapUploadError(err: unknown): err is SourcemapUploadError {
  return (
    typeof err === 'object' &&
    err !== null &&
    (err as { name?: unknown }).name === 'SourcemapUploadError' &&
    typeof (err as { kind?: unknown }).kind === 'string'
  );
}

/** How much of a non-envelope body is printed (contract §4.8 step 5: "capped at 4 KiB"). */
export const MAX_PRINTED_BODY_BYTES = 4096;

export interface DescribedResponse {
  readonly status: number;
  readonly code: string | undefined;
  readonly message: string | undefined;
  readonly details: unknown;
  /** The body as text when it was not the envelope, capped. */
  readonly text: string | undefined;
  /** The printable line (§7: `<status> <code>: <message> <details as JSON>`, or `<status> <text>`). */
  readonly line: string;
}

function capBytes(text: string, max: number): string {
  const buf = Buffer.from(text, 'utf8');
  return buf.byteLength <= max ? text : buf.subarray(0, max).toString('utf8');
}

/** Reads a failing response the way §4.8 step 5 prints it: the envelope's fields when the body is one, the text otherwise. */
export function describeResponse(status: number, body: string): DescribedResponse {
  try {
    const parsed: unknown = JSON.parse(body);
    const error = (parsed as { error?: unknown } | null)?.error;
    if (typeof error === 'object' && error !== null) {
      const { code, message, details } = error as { code?: unknown; message?: unknown; details?: unknown };
      if (typeof code === 'string') {
        const detailsText = details === undefined ? '' : ` ${JSON.stringify(details)}`;
        return { status, code, message: typeof message === 'string' ? message : undefined, details, text: undefined, line: `${status} ${code}: ${typeof message === 'string' ? message : ''}${detailsText}` };
      }
    }
  } catch {
    // not JSON — printed as text below
  }
  const text = capBytes(body, MAX_PRINTED_BODY_BYTES);
  return { status, code: undefined, message: undefined, details: undefined, text, line: `${status} ${text}`.trimEnd() };
}
