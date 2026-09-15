import { buildQuery, getJson } from './lib/http';

export interface Hit {
  sku: string;
  title: string;
  score: number;
}

export function tokenize(query: string): string[] {
  const tokens = query
    .toLowerCase()
    .split(/\s+/)
    .filter((t) => t.length > 0);
  if (tokens.length === 0) {
    throw new RangeError('tokenize: the query is empty');
  }
  return tokens;
}

export function rankHits(hits: Hit[]): Hit[] {
  return [...hits].sort((a, b) => b.score - a.score || a.sku.localeCompare(b.sku));
}

export async function runSearch(query: string, page = 1): Promise<Hit[]> {
  const tokens = tokenize(query);
  const hits = await getJson<Hit[]>(`/api/search${buildQuery({ q: tokens.join(' '), page })}`);
  return rankHits(hits);
}

export function highlight(title: string, tokens: string[]): string {
  let out = title;
  for (const t of tokens) {
    out = out.replaceAll(t, `<mark>${t}</mark>`);
  }
  return out;
}
