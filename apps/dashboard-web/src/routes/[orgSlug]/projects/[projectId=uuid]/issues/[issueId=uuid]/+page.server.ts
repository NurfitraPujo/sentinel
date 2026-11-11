import { redirect } from '@sveltejs/kit';
import type { PageServerLoad } from './$types';

// Issue detail existed at two URLs — this one and `/issues/[id]` — and only the latter was linked
// from the UI. `/issues/[id]` is now the single canonical address (owner's call, 2026-09-08), so
// this route no longer renders a second copy of the page.
//
// It is a REDIRECT rather than a deletion because `lib/server/notify.ts` put this path into
// notification emails that have already been delivered. Deleting the route would 404 every
// historical email link, which is not something a later change can undo. Emails minted from now on
// carry `/issues/[id]`; this can be dropped once the old ones have aged out.
//
// Authorisation is deliberately NOT repeated here: the redirect target performs the real check
// (routes/issues/[id=uuid]/+page.server.ts resolves the issue, then checkProjectAccess against the
// project it actually belongs to). Answering identically for every id — rather than 403ing on one
// and redirecting on another — also keeps this from reporting whether an id exists.
//
// sentinel-project-environments §4.5a called this loader the SECOND door to one issue and gave it
// its own copy of both guard layers — decision 46's `requireProjectAccess` swap on the project
// dimension and decision 20's `404` (never `403`) on the environment dimension. Once the route
// stops rendering there is only one door, and both layers live at the redirect target:
// `routes/issues/[id=uuid]/+page.server.ts:41` (project) and `:46-49` (environment). Re-guarding here
// would ALSO be a regression against the anti-probe property above: an environment-dimension `404`
// on this path answers differently for an id inside the caller's grant than for one outside it,
// which is exactly the existence signal decision 20 exists to withhold. If this route is ever made
// to render again, it must take both layers back.
export const load: PageServerLoad = async ({ params }) => {
	redirect(308, `/issues/${params.issueId}`);
};
