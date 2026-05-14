import { db } from "$lib/server/db";
import { issues, errorOccurrences, projects } from "$lib/db/schema";
import { eq, desc, sql, and, type SQL } from "drizzle-orm";

export interface IssueFilters {
    status?: string;
    projectId?: string;
}

export const issueQueries = {
    async getIssueById(id: string) {
        const [issue] = await db
            .select()
            .from(issues)
            .where(eq(issues.id, id))
            .limit(1);
        return issue || null;
    },

    async getOccurrencesByIssueId(issueId: string, limit = 100) {
        return await db
            .select()
            .from(errorOccurrences)
            .where(eq(errorOccurrences.issueId, issueId))
            .orderBy(desc(errorOccurrences.createdAt))
            .limit(limit);
    },

    async listIssues(filters: IssueFilters, page = 1, limit = 20) {
        const offset = (page - 1) * limit;
        const conditions: SQL[] = [];

        if (filters.status && filters.status !== "all") {
            conditions.push(eq(issues.status, filters.status));
        }

        if (filters.projectId && filters.projectId !== "all") {
            conditions.push(eq(issues.projectId, filters.projectId));
        }

        const results = await db
            .select({
                id: issues.id,
                projectId: issues.projectId,
                fingerprint: issues.fingerprint,
                message: issues.message,
                errorClass: issues.errorClass,
                status: issues.status,
                firstSeen: issues.firstSeen,
                lastSeen: issues.lastSeen,
                count: issues.count,
            })
            .from(issues)
            .where(conditions.length > 0 ? and(...conditions) : undefined)
            .orderBy(desc(issues.lastSeen))
            .limit(limit)
            .offset(offset);

        return results;
    },

    async countIssues(filters: IssueFilters) {
        const conditions: SQL[] = [];

        if (filters.status && filters.status !== "all") {
            conditions.push(eq(issues.status, filters.status));
        }

        if (filters.projectId && filters.projectId !== "all") {
            conditions.push(eq(issues.projectId, filters.projectId));
        }

        const totalResult = await db
            .select({ count: sql<number>`cast(count(*) as integer)` })
            .from(issues)
            .where(conditions.length > 0 ? and(...conditions) : undefined);

        return Number(totalResult[0]?.count ?? 0);
    },

    async listProjects() {
        return await db
            .select({
                id: projects.id,
                name: projects.name,
            })
            .from(projects)
            .limit(100);
    },

    async getProjectById(id: string) {
        const [project] = await db
            .select()
            .from(projects)
            .where(eq(projects.id, id))
            .limit(1);
        return project || null;
    },
};
