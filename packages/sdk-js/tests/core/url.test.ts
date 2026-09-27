// The URL privacy filter (spec §5.1, TASK-03): `filterUrl`, `filterQuery`, `filterUrlsInText`, `stripUserinfo`
// — every clause, the pinned residuals kept on purpose, and the linear-time case.

import { describe, expect, it } from 'vitest';
import { filterQuery, filterUrl, filterUrlsInText, stripUserinfo, URL_IN_TEXT } from '../../src/core/url.js';

describe('stripUserinfo (rule 0)', () => {
  it('removes userinfo through the LAST @ in the authority', () => {
    expect(stripUserinfo('https://user:pass@h/p?a=1')).toBe('https://h/p?a=1');
    expect(stripUserinfo('https://u:p@h/x')).toBe('https://h/x');
    expect(stripUserinfo('//u@h/x')).toBe('//h/x');
    expect(stripUserinfo('https://a@b:p@h/')).toBe('https://h/');
  });

  it('leaves an @ after the authority, and a string with no // scheme, untouched', () => {
    expect(stripUserinfo('https://h/users/@me')).toBe('https://h/users/@me');
    expect(stripUserinfo('mailto:a@b.test')).toBe('mailto:a@b.test');
  });
});

describe('filterUrl: rule 3 — substring and segment keys', () => {
  it.each([
    ['password', '/p?password=x', '/p?password=[Filtered]'],
    ['token substring', '/p?access_token=x', '/p?access_token=[Filtered]'],
    ['auth substring', '/p?authz=x', '/p?authz=[Filtered]'],
    ['segment: code', '/p?auth_code=x', '/p?auth_code=[Filtered]'],
    ['segment: state', '/p?session_state=x', '/p?session_state=[Filtered]'],
    ['segment: key', '/p?Key-Pair-Id=x', '/p?Key-Pair-Id=[Filtered]'],
    ['segment: signature', '/p?X-Amz-Signature=x', '/p?X-Amz-Signature=[Filtered]'],
    ['segment: credential', '/p?X-Goog-Credential=x', '/p?X-Goog-Credential=[Filtered]'],
    ['segment: sig', '/p?sig=x', '/p?sig=[Filtered]'],
    ['segment: jwt', '/p?jwt=x', '/p?jwt=[Filtered]'],
    ['segment: otp', '/p?otp=x', '/p?otp=[Filtered]'],
  ])('%s', (_label, input, expected) => {
    expect(filterUrl(input)).toBe(expected);
  });

  it('non-matches (substrings of ordinary keys, not whole segments): zipcode, monkey, statement', () => {
    expect(filterUrl('/p?zipcode=90210')).toBe('/p?zipcode=90210');
    expect(filterUrl('/p?monkey=1')).toBe('/p?monkey=1');
    expect(filterUrl('/p?statement=1')).toBe('/p?statement=1');
  });

  it('every other part is kept byte for byte; fragments are removed; a keyless part is kept', () => {
    expect(filterUrl('/p?a=1&b=2#frag')).toBe('/p?a=1&b=2');
    expect(filterUrl('/verify?t0k')).toBe('/verify?t0k');
    expect(filterUrl('/p?color=%23fff')).toBe('/p?color=%23fff');
    expect(filterUrl('/p?ids=1,2,3')).toBe('/p?ids=1,2,3');
  });

  it('a malformed % escape stays as written', () => {
    expect(filterUrl('/p?%zz=1&token=%zz')).toBe('/p?%zz=1&token=[Filtered]');
  });

  it('the ; separator (spec review round 7)', () => {
    expect(filterUrl('/p?a=1;reset_token=X')).toBe('/p?a=1;reset_token=[Filtered]');
    expect(filterQuery('a=1;token=t&b=2')).toBe('a=1;token=[Filtered]&b=2');
  });

  it('a key readable only after rule 2\'s second decode', () => {
    expect(filterUrl('/p?co%2564e=X&page=2')).toBe('/p?co%2564e=[Filtered]&page=2');
  });

  it('a string with no ? and no # comes back unchanged except for rule 0 (rule 6)', () => {
    expect(filterUrl('/plain/path')).toBe('/plain/path');
  });
});

describe('filterUrl: rule 5 — a URL nested in a value', () => {
  it('a nested path-with-query is filtered', () => {
    expect(filterUrl('/login?next=/reset-password?reset_token=X')).toBe('/login?next=/reset-password?reset_token=[Filtered]');
  });

  it('an encoded nested URL is filtered and re-encoded n times', () => {
    expect(filterUrl('/login?next=%2Freset%3Freset_token%3DX')).toBe('/login?next=%2Freset%3Freset_token%3D%5BFiltered%5D');
  });

  it('a doubly encoded nested URL decodes twice and re-encodes twice', () => {
    expect(filterUrl('/login?next=%252Freset%253Freset_token%253DX')).toBe('/login?next=%252Freset%253Freset_token%253D%255BFiltered%255D');
  });

  it('a nested fragment-only URL: the fragment is dropped, nothing left to filter', () => {
    expect(filterUrl('/login?next=%2Fcb%23access_token%3DX')).toBe('/login?next=%2Fcb');
  });

  it('a nested fragment with no sensitive key, and an encoded #, are untouched', () => {
    expect(filterUrl('/p?color=%23fff')).toBe('/p?color=%23fff');
    expect(filterUrl('/p?next=%2Fp%23page%3D2')).toBe('/p?next=%2Fp%23page%3D2');
  });

  it('userinfo inside a nested value is stripped', () => {
    expect(filterUrl('/login?next=https%3A%2F%2Fu%3Ap%40h%2Fx')).toBe('/login?next=https%3A%2F%2Fh%2Fx');
  });

  it('a later parameter of a nested URL is a separate outer part, filtered there', () => {
    expect(filterUrl('/login?next=/r?page=2&reset_token=X')).toBe('/login?next=/r?page=2&reset_token=[Filtered]');
  });

  it('a value nested 3 levels deep is filtered', () => {
    const inner = encodeURIComponent('/p?token=X');
    const level2 = encodeURIComponent('/p?b=' + inner);
    const three = '/p?a=' + level2;
    // The key name stays visible (only the value is filtered, rule 4); "X" — the value — is gone.
    expect(filterUrl(three)).toContain('5BFiltered%25');
    expect(filterUrl(three)).not.toContain('%3DX');
  });

  it('a value nested past the 3-level cap returns without throwing (no recursion past depth 3)', () => {
    let value = 'token=X';
    for (let i = 0; i < 6; i++) value = encodeURIComponent('/p?nested=' + value);
    expect(() => filterUrl('/p?a=' + value)).not.toThrow();
  });

  it('lenient decode: a non-UTF-8 escape and a trailing % do not hide the token', () => {
    expect(filterUrl('/login?next=%E9%2Freset%3Freset_token%3DX')).toBe('/login?next=%25E9%2Freset%3Freset_token%3D%5BFiltered%5D');
    expect(filterUrl('/login?next=%2Freset%3Freset_token%3DX%')).toBe('/login?next=%2Freset%3Freset_token%3D%5BFiltered%5D');
  });

  it('a valid multi-byte escape decodes and re-encodes to the same bytes', () => {
    expect(filterUrl('/login?next=%2Fcaf%C3%A9%3Ftoken%3DX')).toBe('/login?next=%2Fcaf%C3%A9%3Ftoken%3D%5BFiltered%5D');
  });

  it('two pinned residuals kept on purpose (§12.2(3))', () => {
    expect(filterUrl('/login?next=reset_token%3DX')).toBe('/login?next=reset_token%3DX');
  });
});

describe('filterQuery on a bare query string', () => {
  it('filters a sensitive key, keeping the rest', () => {
    expect(filterQuery('reset_token=abc&page=2')).toBe('reset_token=[Filtered]&page=2');
  });

  it('filters a nested URL value the same way filterUrl does', () => {
    expect(filterQuery('next=/reset-password?reset_token=X')).toBe('next=/reset-password?reset_token=[Filtered]');
  });

  it('a comma-joined value carrying a nested URL is filtered', () => {
    expect(filterQuery('a=1,https://h/q?reset_token=S')).toBe('a=1,https://h/q?reset_token=[Filtered]');
  });
});

describe('filterUrlsInText', () => {
  it('filters an absolute and a relative URL, and two on one line (captured prefix put back)', () => {
    expect(filterUrlsInText('see /login?next=/reset-password?reset_token=X done')).toBe('see /login?next=/reset-password?reset_token=[Filtered] done');
    expect(filterUrlsInText('fetch https://u:p@h/x failed')).toBe('fetch https://h/x failed');
    expect(filterUrlsInText('see //u:p@h/x ok')).toBe('see //h/x ok');
    expect(filterUrlsInText('a http://h/x?token=t b http://h/y?token=t2')).toBe('a http://h/x?token=[Filtered] b http://h/y?token=[Filtered]');
  });

  it('a comma-separated URL list: one match, the second URL filtered inside the value', () => {
    expect(filterUrlsInText('see /p?a=1,https://h/q?reset_token=S')).toBe('see /p?a=1,https://h/q?reset_token=[Filtered]');
    expect(filterUrlsInText('see /p?next=x,https://h/q&reset_token=S')).toBe('see /p?next=x,https://h/q&reset_token=[Filtered]');
  });

  it('a nested fragment-only URL and a bare fragment (no base) — the base rule drops it, the bare one is a pinned residual', () => {
    expect(filterUrlsInText('go https://h/cb#access_token=S')).toBe('go https://h/cb');
    expect(filterUrlsInText('hash is #access_token=X')).toBe('hash is #access_token=X');
  });

  it('a fragment with a query-like tail is dropped by rule 1', () => {
    expect(filterUrlsInText('see /p?a=1#frag and more')).toBe('see /p?a=1 and more');
  });

  it("not a path start: a/b?token=x and x//c?token=x are unchanged (no lookbehind false match)", () => {
    expect(filterUrlsInText('a/b?token=x')).toBe('a/b?token=x');
    expect(filterUrlsInText('x//c?token=x')).toBe('x//c?token=x');
  });

  it('text with a ? that is not a URL is unchanged', () => {
    expect(filterUrlsInText('is this ok?')).toBe('is this ok?');
  });

  it("round 7's residual: the match ends at the space, so 'GET /api/search?q=John Doe&token=abc' is unchanged", () => {
    expect(filterUrlsInText('GET /api/search?q=John Doe&token=abc')).toBe('GET /api/search?q=John Doe&token=abc');
  });

  it('lenient decode inside free text', () => {
    expect(filterUrlsInText('see /login?next=%2Freset%3Freset_token%3DX%E9 ok')).toBe('see /login?next=%2Freset%3Freset_token%3D%5BFiltered%5D ok');
  });

  it('linear time: a long slash-heavy string with no ? returns within 1s', () => {
    const start = performance.now();
    filterUrlsInText('-/'.repeat(500_000));
    expect(performance.now() - start).toBeLessThan(1000);
  });
});

describe('URL_IN_TEXT match shape', () => {
  it('a comma-joined URL list is one match; base and query split correctly', () => {
    URL_IN_TEXT.lastIndex = 0;
    const matches = [...'see /p?a=1,https://h/q?reset_token=S'.matchAll(URL_IN_TEXT)];
    expect(matches).toHaveLength(1);
    expect(matches[0]?.[2]).toBe('/p');
    expect(matches[0]?.[3]).toBe('a=1,https://h/q?reset_token=S');
  });

  it('one match over a path with a comma-joined ids query', () => {
    URL_IN_TEXT.lastIndex = 0;
    const matches = [...'/p?ids=1,2,3 ok'.matchAll(URL_IN_TEXT)];
    expect(matches).toHaveLength(1);
    expect(matches[0]?.[3]).toBe('ids=1,2,3');
  });
});
