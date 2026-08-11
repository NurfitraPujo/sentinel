// Manual Issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8/§10): the notification shape shared
// between the server query (lib/db/queries/notifications.ts) and client components
// (NotificationBell.svelte, /[orgSlug]/notifications). Deliberately its own module, not exported
// from queries/notifications.ts directly -- that file imports `$lib/server/db`, which client
// components must never pull into their bundle even via a type-only import path.
export interface NotificationListItem {
	id: string;
	kind: string;
	actorType: string;
	actorId: string;
	actorName: string | null;
	payload: unknown;
	readAt: Date | string | null;
	createdAt: Date | string;
	issueId: string;
	issueTitle: string;
	issueType: string;
	projectId: string;
	orgSlug: string;
}
