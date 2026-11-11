// sentinel-issue-tag-facets spec §10.2: the check query, verbatim (the same text as
// tests/integration/tag_facets_db_test.go `tagFacetsCheckSQL`). Every exactness proof asserts all five
// columns are 0. It is SCOPED to the issues a test created ($1, a uuid[]) and its oracle counts only
// COUNTED occurrences (issue_tag_counted), so it asserts on the state the test made and nothing else,
// even against a database holding rows the backfill never counted. Shared by the two real-Postgres
// dashboard suites; never a route file.
export const TAG_FACETS_CHECK_SQL = `
WITH bf AS (SELECT cursor_id, done FROM issue_tag_values_backfill),
oracle AS (SELECT o.issue_id, p.key, p.value, count(*) AS n
             FROM error_occurrences o CROSS JOIN bf
             CROSS JOIN LATERAL issue_tag_pairs(o.tags, o.contexts, o.event_user, o.release_version, o.environment) p
            WHERE o.issue_id = ANY($1::uuid[]) AND issue_tag_counted(o.tag_rollup_applied, o.id, bf.cursor_id, bf.done)
            GROUP BY 1, 2, 3),
r AS (SELECT * FROM issue_tag_values WHERE issue_id = ANY($1::uuid[]))
SELECT
 (SELECT count(*) FROM r t LEFT JOIN oracle o USING (issue_id, key, value)
   WHERE t.value <> '' AND o.n IS DISTINCT FROM t.count)                                     AS named_mismatch,
 (SELECT count(*) FROM (SELECT issue_id, key, sum(n) s FROM oracle GROUP BY 1, 2) o
   FULL JOIN (SELECT issue_id, key, sum(count) s FROM r GROUP BY 1, 2) x USING (issue_id, key)
   WHERE o.s IS DISTINCT FROM x.s)                                                            AS key_total_mismatch,
 (SELECT count(*) FROM oracle o
   WHERE NOT EXISTS (SELECT 1 FROM r t WHERE (t.issue_id, t.key, t.value) = (o.issue_id, o.key, o.value))
     AND NOT EXISTS (SELECT 1 FROM r t WHERE (t.issue_id, t.key, t.value) = (o.issue_id, o.key, ''))) AS value_nowhere,
 (SELECT count(*) FROM r WHERE count <= 0)                                                    AS zero_rows,
 (SELECT count(*) FROM (SELECT 1 FROM r WHERE value <> '' GROUP BY issue_id, key HAVING count(*) > 1000) x) AS over_cap
`;

export interface TagFacetsCheckRow {
	named_mismatch: number | string;
	key_total_mismatch: number | string;
	value_nowhere: number | string;
	zero_rows: number | string;
	over_cap: number | string;
}
