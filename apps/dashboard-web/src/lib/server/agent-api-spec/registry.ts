import type { z } from 'zod';
import * as S from './schemas';

/**
 * N6: the typed table both the OpenAPI generator (`scripts/generate-agent-openapi.ts`) and
 * `completeness.test.ts` walk. `path`/`method`/`routeFile` are the bidirectional-completeness
 * anchor: `completeness.test.ts` derives the SAME (path, method) set by scanning
 * `src/routes/api/agent/**\/+server.ts` and asserts it against this table's keys.
 */

export interface RegistryResponses {
	[status: string]: z.ZodTypeAny;
}

export interface RegistryEntry {
	path: string;
	method: 'get' | 'post' | 'patch' | 'delete';
	routeFile: string;
	operationId: string;
	summary: string;
	request?: {
		querySchema?: z.ZodTypeAny;
		bodySchema?: z.ZodTypeAny;
	};
	responses: RegistryResponses;
}

export const agentApiRegistry: RegistryEntry[] = [
	{
		path: '/api/agent/issues',
		method: 'get',
		routeFile: 'src/routes/api/agent/issues/+server.ts',
		operationId: 'listIssues',
		summary: "List issues in the calling key's organization",
		request: { querySchema: S.ListIssuesQuerySchema },
		responses: {
			'200': S.ListIssuesResponseSchema,
			'400': S.ErrorFieldErrorSchema,
			'401': S.UnauthorizedErrorSchema,
		},
	},
	{
		path: '/api/agent/issues/{issueId}',
		method: 'get',
		routeFile: 'src/routes/api/agent/issues/[issueId]/+server.ts',
		operationId: 'getIssue',
		summary: 'Full issue detail (spans user_report and system_error)',
		responses: {
			'200': S.IssueDetailResponseSchema,
			'401': S.UnauthorizedErrorSchema,
			'404': S.MessageErrorSchema,
		},
	},
	{
		path: '/api/agent/issues/{issueId}/occurrences',
		method: 'get',
		routeFile: 'src/routes/api/agent/issues/[issueId]/occurrences/+server.ts',
		operationId: 'listIssueOccurrences',
		summary: 'Newest-first page of occurrences (system_error issues)',
		request: { querySchema: S.OccurrencesQuerySchema },
		responses: {
			'200': S.OccurrencesResponseSchema,
			'400': S.MessageErrorSchema,
			'401': S.UnauthorizedErrorSchema,
			'404': S.MessageErrorSchema,
		},
	},
	{
		path: '/api/agent/issues/{issueId}/claim',
		method: 'post',
		routeFile: 'src/routes/api/agent/issues/[issueId]/claim/+server.ts',
		operationId: 'claimIssue',
		summary:
			'Atomically claim an issue (idempotent self-reclaim: re-claiming an issue you already hold returns 200 with alreadyClaimed:true, not 409; 409 only if held by someone else)',
		responses: {
			'200': S.ClaimResponseSchema,
			'401': S.UnauthorizedErrorSchema,
			'404': S.MessageErrorSchema,
			'409': S.ClaimConflictErrorSchema,
		},
	},
	{
		path: '/api/agent/issues/{issueId}/claim',
		method: 'delete',
		routeFile: 'src/routes/api/agent/issues/[issueId]/claim/+server.ts',
		operationId: 'releaseClaim',
		summary:
			"Release your own claim (idempotent: releasing an already-unclaimed issue returns 200, not 409; cannot release another agent's claim)",
		responses: {
			'200': S.ReleaseResponseSchema,
			'401': S.UnauthorizedErrorSchema,
			'404': S.MessageErrorSchema,
			'409': S.ClaimConflictErrorSchema,
		},
	},
	{
		path: '/api/agent/issues/{issueId}/status',
		method: 'patch',
		routeFile: 'src/routes/api/agent/issues/[issueId]/status/+server.ts',
		operationId: 'updateIssueStatus',
		summary:
			'Change issue status (idempotent: retrying the same status+resolved_in_version returns changed:false, no duplicate activity/notification)',
		request: { bodySchema: S.StatusBodySchema },
		responses: {
			'200': S.StatusResponseSchema,
			'400': S.MessageErrorSchema,
			'401': S.UnauthorizedErrorSchema,
			'404': S.MessageErrorSchema,
		},
	},
	{
		path: '/api/agent/issues/{issueId}/comments',
		method: 'get',
		routeFile: 'src/routes/api/agent/issues/[issueId]/comments/+server.ts',
		operationId: 'listComments',
		summary: 'Poll comments (e.g. to see answers to a blocking question)',
		request: { querySchema: S.CommentsQuerySchema },
		responses: {
			'200': S.ListCommentsResponseSchema,
			'400': S.MessageErrorSchema,
			'401': S.UnauthorizedErrorSchema,
			'404': S.MessageErrorSchema,
		},
	},
	{
		path: '/api/agent/issues/{issueId}/comments',
		method: 'post',
		routeFile: 'src/routes/api/agent/issues/[issueId]/comments/+server.ts',
		operationId: 'postComment',
		summary:
			'Post a non-blocking comment (emails subscribers). Optional idempotency_key: a repeat replays the original comment with deduplicated:true, no second email',
		request: { bodySchema: S.PostCommentBodySchema },
		responses: {
			'201': S.PostCommentResponseSchema,
			'400': S.MessageErrorSchema,
			'401': S.UnauthorizedErrorSchema,
			'404': S.MessageErrorSchema,
			'409': S.MessageErrorSchema,
		},
	},
	{
		path: '/api/agent/issues/{issueId}/comments/{commentId}',
		method: 'patch',
		routeFile: 'src/routes/api/agent/issues/[issueId]/comments/[commentId]/+server.ts',
		operationId: 'editComment',
		summary: 'Edit your own comment (403 for another author, agent or human)',
		request: { bodySchema: S.EditCommentBodySchema },
		responses: {
			'200': S.EditCommentResponseSchema,
			'400': S.MessageErrorSchema,
			'401': S.UnauthorizedErrorSchema,
			'403': S.MessageErrorSchema,
			'404': S.MessageErrorSchema,
		},
	},
	{
		path: '/api/agent/issues/{issueId}/comments/{commentId}',
		method: 'delete',
		routeFile: 'src/routes/api/agent/issues/[issueId]/comments/[commentId]/+server.ts',
		operationId: 'deleteComment',
		summary: 'Delete your own comment (403 for another author, agent or human)',
		responses: {
			'200': S.DeleteCommentResponseSchema,
			'401': S.UnauthorizedErrorSchema,
			'403': S.MessageErrorSchema,
			'404': S.MessageErrorSchema,
		},
	},
	{
		path: '/api/agent/issues/{issueId}/report/severity',
		method: 'patch',
		routeFile: 'src/routes/api/agent/issues/[issueId]/report/severity/+server.ts',
		operationId: 'updateReportSeverity',
		summary: 'Set the severity of a user_report issue (400 for a system_error issue)',
		request: { bodySchema: S.SeverityBodySchema },
		responses: {
			'200': S.SeverityResponseSchema,
			'400': S.MessageErrorSchema,
			'401': S.UnauthorizedErrorSchema,
			'404': S.MessageErrorSchema,
		},
	},
	{
		path: '/api/agent/issues/{issueId}/questions',
		method: 'post',
		routeFile: 'src/routes/api/agent/issues/[issueId]/questions/+server.ts',
		operationId: 'askQuestion',
		summary:
			'Ask a blocking question (sets waiting_on, forces an immediate email). Optional idempotency_key: a repeat replays the original question comment id with deduplicated:true and no second email',
		request: { bodySchema: S.QuestionBodySchema },
		responses: {
			'201': S.QuestionResponseSchema,
			'400': S.MessageErrorSchema,
			'401': S.UnauthorizedErrorSchema,
			'404': S.MessageErrorSchema,
			'409': S.MessageErrorSchema,
		},
	},
	{
		path: '/api/agent/issues/{issueId}/progress',
		method: 'post',
		routeFile: 'src/routes/api/agent/issues/[issueId]/progress/+server.ts',
		operationId: 'postProgress',
		summary:
			'Post an in-app-only progress update (no email). Optional idempotency_key: a repeat replays a deduplicated success with no second progress row',
		request: { bodySchema: S.ProgressBodySchema },
		responses: {
			'201': S.ProgressResponseSchema,
			'400': S.MessageErrorSchema,
			'401': S.UnauthorizedErrorSchema,
			'404': S.MessageErrorSchema,
			'409': S.MessageErrorSchema,
		},
	},
	{
		path: '/api/agent/issues/{issueId}/relations',
		method: 'post',
		routeFile: 'src/routes/api/agent/issues/[issueId]/relations/+server.ts',
		operationId: 'addRelation',
		summary:
			"Add a relation from this issue to another (409 for an exact duplicate; also 409 for caused_by if the REVERSE pair already exists -- would create a 2-cycle)",
		request: { bodySchema: S.RelationBodySchema },
		responses: {
			'201': S.RelationRowSchema,
			'400': S.MessageErrorSchema,
			'401': S.UnauthorizedErrorSchema,
			'404': S.MessageErrorSchema,
			'409': S.MessageErrorSchema,
		},
	},
	{
		path: '/api/agent/issues/{issueId}/relations',
		method: 'delete',
		routeFile: 'src/routes/api/agent/issues/[issueId]/relations/+server.ts',
		operationId: 'removeRelation',
		summary: 'Remove a relation, identified by (target_issue_id, relation_type)',
		request: { bodySchema: S.RelationBodySchema },
		responses: {
			'200': S.RelationRemoveResponseSchema,
			'400': S.MessageErrorSchema,
			'401': S.UnauthorizedErrorSchema,
			'404': S.MessageErrorSchema,
		},
	},
	{
		path: '/api/agent/projects',
		method: 'get',
		routeFile: 'src/routes/api/agent/projects/+server.ts',
		operationId: 'listProjects',
		summary: "List the calling key's organization's projects",
		responses: {
			'200': S.ListProjectsResponseSchema,
			'401': S.UnauthorizedErrorSchema,
		},
	},
	{
		path: '/api/agent/events',
		method: 'get',
		routeFile: 'src/routes/api/agent/events/+server.ts',
		operationId: 'listEvents',
		summary: 'Seq-cursored org activity feed (at-least-once, ~2s lag guard)',
		request: { querySchema: S.EventsQuerySchema },
		responses: {
			'200': S.EventsResponseSchema,
			'400': S.ErrorFieldErrorSchema,
			'401': S.UnauthorizedErrorSchema,
		},
	},
	{
		path: '/api/agent/uploads',
		method: 'post',
		routeFile: 'src/routes/api/agent/uploads/+server.ts',
		operationId: 'uploadAttachment',
		summary: 'Upload an attachment (multipart; not associated with an issue until referenced in a comment)',
		responses: {
			'201': S.UploadResponseSchema,
			'400': S.MessageErrorSchema,
			'401': S.UnauthorizedErrorSchema,
			'413': S.MessageErrorSchema,
			'415': S.MessageErrorSchema,
			'503': S.MessageErrorSchema,
		},
	},
	{
		path: '/api/agent/self',
		method: 'get',
		routeFile: 'src/routes/api/agent/self/+server.ts',
		operationId: 'getSelf',
		summary: 'Identity of the calling key: agent, org, and the key row itself',
		responses: {
			'200': S.SelfResponseSchema,
			'401': S.UnauthorizedErrorSchema,
		},
	},
	{
		path: '/api/agent/key/rotate',
		method: 'post',
		routeFile: 'src/routes/api/agent/key/rotate/+server.ts',
		operationId: 'rotateKey',
		summary:
			'Rotate the calling key: mints a new secret, keeps the old key valid for AGENT_KEY_ROTATION_GRACE_HOURS (default 24, 0 = immediate)',
		responses: {
			'200': S.KeyRotateResponseSchema,
			'400': S.MessageErrorSchema,
			'401': S.UnauthorizedErrorSchema,
		},
	},
	{
		path: '/api/agent/batch',
		method: 'post',
		routeFile: 'src/routes/api/agent/batch/+server.ts',
		operationId: 'runBatch',
		summary: 'Run up to 20 mutations sequentially in one HTTP round trip',
		request: { bodySchema: S.BatchBodySchema },
		responses: {
			'200': S.BatchResponseSchema,
			'400': S.BatchValidationErrorSchema,
			'401': S.UnauthorizedErrorSchema,
		},
	},
];
