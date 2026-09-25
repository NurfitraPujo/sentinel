// The one tag-link encoder, shared by sentinel-occurrence-detail-panels (spec §6.3) and sentinel-issue-tag-facets
// (spec §6.1). Contract decisions 6 and 7: each part is encoded before the join, the joined string is encoded again
// as one query value, and URLSearchParams is never used to build the href.
export function formatTagParam(key: string, value: string): string {
	return encodeURIComponent(key) + ':' + encodeURIComponent(value);
}

export function tagSearchHref(key: string, value: string): string {
	return '/search?tag=' + encodeURIComponent(formatTagParam(key, value));
}
