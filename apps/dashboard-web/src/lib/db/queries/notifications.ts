import { db } from '$lib/server/db';
import { notifications, issues, projects, organizations, users } from '$lib/db/schema';
import { and, desc, eq, isNull, sql } from 'drizzle-orm';
import type { NotificationListItem } from '$lib/notifications/types';

export type { NotificationListItem };

/**
 * Manual Issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8): the notification inbox's read side --
 * `GET /api/notifications` (list, and `?count=unread`) and `PATCH /api/notifications`
 * (mark-read). The WRITE side (`notifications` insert) lives in notify.ts's `notifyIssueEvent`,
 * inside the mutation's own transaction (D18) -- nothing here ever inserts a row.
 */

const DEFAULT_PAGE_SIZE = 25;
const MAX_PAGE_SIZE = 100;

export interface ListNotificationsOptions {
	userId: string;
	limit?: number;
	offset?: number;
}

// §8/§10 UI shape (see `$lib/notifications/types`): the bell dropdown and the `/notifications`
// list page both need the issue's title + type (to pick an icon and build the right detail-page
// link, per notify.ts's `buildIssueUrl` split on `issue_type`) and the acting user's display
// name -- none of which live on the `notifications` row itself. Joined here (left joins:
// `actorId` is only ever a real `users.id` when `actorType === 'user'`; for 'agent'/'system'
// actors the join simply misses and `actorName` comes back null, which the UI falls back to
// `actorId` for) rather than making every caller do a second round-trip per row.

/** Newest-first, paginated. `limit` is clamped to `MAX_PAGE_SIZE` -- same DoS-shape reasoning as MAX_BATCH_ISSUE_IDS (issues.ts). */
export async function listNotifications({
	userId,
	limit,
	offset,
}: ListNotificationsOptions): Promise<NotificationListItem[]> {
	const clampedLimit = Math.min(Math.max(limit ?? DEFAULT_PAGE_SIZE, 1), MAX_PAGE_SIZE);
	const clampedOffset = Math.max(offset ?? 0, 0);

	return await db
		.select({
			id: notifications.id,
			kind: notifications.kind,
			actorType: notifications.actorType,
			actorId: notifications.actorId,
			actorName: users.name,
			payload: notifications.payload,
			readAt: notifications.readAt,
			createdAt: notifications.createdAt,
			issueId: notifications.issueId,
			issueTitle: issues.message,
			issueType: issues.issueType,
			projectId: issues.projectId,
			orgSlug: organizations.slug,
		})
		.from(notifications)
		.innerJoin(issues, eq(issues.id, notifications.issueId))
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.innerJoin(organizations, eq(organizations.id, projects.organizationId))
		.leftJoin(users, eq(users.id, notifications.actorId))
		.where(eq(notifications.userId, userId))
		.orderBy(desc(notifications.createdAt))
		.limit(clampedLimit)
		.offset(clampedOffset);
}

/** Total (read + unread) notification count for `userId` -- backs `/notifications`'s pagination. */
export async function getNotificationCount(userId: string): Promise<number> {
	const rows = await db
		.select({ count: sql<number>`count(*)::int` })
		.from(notifications)
		.where(eq(notifications.userId, userId));

	return rows[0]?.count ?? 0;
}

/** Backs the bell badge -- `GET /api/notifications?count=unread`. */
export async function getUnreadNotificationCount(userId: string): Promise<number> {
	const rows = await db
		.select({ count: sql<number>`count(*)::int` })
		.from(notifications)
		.where(and(eq(notifications.userId, userId), isNull(notifications.readAt)));

	return rows[0]?.count ?? 0;
}

/**
 * Marks a single notification read. Scoped to `userId` in the WHERE clause -- not just `id` --
 * so a caller can never mark (or probe the existence of) another user's notification (B7-shaped:
 * ownership comes from the session, never trusted from the request alone). Returns whether a row
 * was actually updated, so the route can 404 on an id that doesn't belong to this user.
 */
export async function markNotificationRead(notificationId: string, userId: string): Promise<boolean> {
	const updated = await db
		.update(notifications)
		.set({ readAt: new Date() })
		.where(and(eq(notifications.id, notificationId), eq(notifications.userId, userId)))
		.returning({ id: notifications.id });

	return updated.length > 0;
}

/** Marks every unread notification for `userId` read. Returns the count actually updated. */
export async function markAllNotificationsRead(userId: string): Promise<number> {
	const updated = await db
		.update(notifications)
		.set({ readAt: new Date() })
		.where(and(eq(notifications.userId, userId), isNull(notifications.readAt)))
		.returning({ id: notifications.id });

	return updated.length;
}
