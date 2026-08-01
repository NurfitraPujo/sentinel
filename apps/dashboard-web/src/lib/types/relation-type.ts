// Single source of truth for the set of issue relation types the DB actually permits.
//
// This mirrors the CHECK constraint in
// packages/db-migrations/migrations/1721900000_add_issue_lifecycle_and_relations.sql:72 and the
// VALID_RELATION_TYPES constant that src/routes/api/issues/[issueId]/relations/+server.ts
// validates incoming writes against. Both the query layer (src/lib/db/queries/issues.ts) and any
// UI component that renders a relationType must import RelationType from here rather than
// re-declaring the union, so the three call sites cannot drift out of sync again.
//
// The DB column itself is a plain varchar with no DB-level enum type, so any value read back out
// of the database is only ever known to TypeScript as `string`. Narrow it to RelationType with
// isRelationType/filterKnownRelationTypes at the boundary where DB data enters typed UI code —
// never with an `as` cast, since a cast would silently accept a row a future migration or a bad
// manual UPDATE puts outside the three known values.

export const RELATION_TYPES = ['linked_to', 'caused_by', 'duplicate_of'] as const;

export type RelationType = (typeof RELATION_TYPES)[number];

export function isRelationType(value: unknown): value is RelationType {
	return typeof value === 'string' && (RELATION_TYPES as readonly string[]).includes(value);
}

/**
 * Narrows a list of DB-shaped items (relationType: string) down to items whose relationType is
 * one of the known RelationType values. Anything else is dropped and logged rather than rendered
 * or cast, since it represents data the UI's type system does not model and cannot safely trust.
 */
export function filterKnownRelationTypes<T extends { relationType: string }>(
	items: readonly T[]
): (T & { relationType: RelationType })[] {
	const kept: (T & { relationType: RelationType })[] = [];
	for (const item of items) {
		if (isRelationType(item.relationType)) {
			kept.push(item as T & { relationType: RelationType });
		} else {
			console.warn(
				`Dropping issue relation ${('id' in item && item.id) || '<unknown>'} with unrecognized relationType: ${item.relationType}`
			);
		}
	}
	return kept;
}
