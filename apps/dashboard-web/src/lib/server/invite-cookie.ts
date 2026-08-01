/**
 * The short-lived, HttpOnly cookie that carries an invitation token across a sign-in round trip.
 *
 * This lives here rather than in a route file for two reasons:
 *
 * 1. SvelteKit route modules (`+page.server.ts`) may only export a fixed set of names — `load`,
 *    `actions`, `prerender`, … — and reject anything else at BUILD time. Exporting this constant
 *    from `routes/invitations/[token]/+page.server.ts` failed `pnpm build` with
 *    "Invalid export 'INVITE_TOKEN_COOKIE'", while `pnpm check` and `pnpm test` both passed, so
 *    nothing caught it until an image build.
 * 2. Before that, the value was duplicated in two route files and kept in sync by a comment.
 *    A cookie name that disagrees between the writer and the reader silently breaks invitation
 *    acceptance, with no error anywhere — precisely the cross-file coupling this repo's B5
 *    convention warns about.
 *
 * The token in this cookie is the RAW invite token (only its sha256 hash is ever persisted — D06),
 * which is why it must stay HttpOnly and short-lived.
 */
export const INVITE_TOKEN_COOKIE = 'sentinel_invite_token';

/** Long enough for an OAuth or magic-link round trip, short enough to limit exposure. */
export const INVITE_TOKEN_COOKIE_MAX_AGE_SECONDS = 10 * 60;
