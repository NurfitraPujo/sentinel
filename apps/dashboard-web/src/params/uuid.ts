import type { ParamMatcher } from '@sveltejs/kit';
import { isUuid } from '$lib/uuid';

/**
 * sentinel-issue-activity TASK-01: `[issueId=uuid]`. A path segment that is not a UUID does not match the route at all,
 * so the request is the router's ordinary 404 and no handler (and no query) ever runs with it. Every route whose
 * parameter is a `uuid` column carries this matcher; `tests/uuid-params.test.ts` walks src/routes and fails on one that
 * does not.
 */
export const match = ((param: string): param is string => isUuid(param)) satisfies ParamMatcher;
