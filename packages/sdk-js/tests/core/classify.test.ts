// B3 of flows.md: `error_class` / `message` derivation (decisions 39 and 42 — the `Error` test is
// `Object.prototype.toString` OR `instanceof`, cross-realm, no duck typing; an empty name is `"Error"`), and the
// safe serialisation of contract §4.3 *Nothing here throws* — the four unserialisable inputs kept with their
// markers and the pinned `truncated_keys` path form. Shuffle-safe: every test starts from a deleted slot.

import { runInNewContext } from 'node:vm';
import { beforeEach, describe, expect, it } from 'vitest';
import { captureException, captureMessage } from '../../src/core/capture.js';
import { metadataWireBytes, sanitize } from '../../src/core/serialize.js';
import { resetPipeline, setup } from '../helpers/pipeline.js';

beforeEach(resetPipeline);

describe('B3: error_class and message for a string / an object / a stackless Error', () => {
  it('B3: a thrown string → error_class "Error", the string as message, stacktrace []', () => {
    const p = setup();
    captureException('nope');
    expect(p.events()[0]).toMatchObject({ error_class: 'Error', message: 'nope', stacktrace: [] });
  });

  it('B3: a thrown { code: 42 } → "NonError" with its JSON as the message, stacktrace []', () => {
    const p = setup();
    captureException({ code: 42 });
    expect(p.events()[0]).toMatchObject({ error_class: 'NonError', message: '{"code":42}', stacktrace: [] });
  });

  it('B3: a number, null, undefined and a symbol are "NonError" and never throw', () => {
    const p = setup();
    captureException(7);
    captureException(null);
    captureException(undefined);
    captureException(Symbol('s'));
    expect(p.events().map((e) => [e.error_class, e.message])).toEqual([
      ['NonError', '7'],
      ['NonError', 'null'],
      ['NonError', 'undefined'],
      ['NonError', '[unserializable]'],
    ]);
  });

  it('B3: an Error with no stack → error.name as the class, stacktrace []', () => {
    const p = setup();
    const err = new RangeError('out of range');
    delete err.stack;
    captureException(err);
    expect(p.events()[0]).toMatchObject({ error_class: 'RangeError', message: 'out of range', stacktrace: [] });
  });

  it('B3: a cross-realm Error (vm.runInNewContext) takes the Error branch — Object.prototype.toString, not instanceof', () => {
    const foreign = runInNewContext('new Error("x")') as Error;
    expect(foreign instanceof Error).toBe(false);
    const p = setup();
    captureException(foreign);
    expect(p.events()[0]).toMatchObject({ error_class: 'Error', message: 'x' });
    expect(p.events()[0]?.stacktrace.length).toBeGreaterThan(0);
  });

  it('B3: a duck { message, stack } is "NonError" — no duck typing', () => {
    const p = setup();
    captureException({ message: 'm', stack: 's' });
    expect(p.events()[0]).toMatchObject({ error_class: 'NonError', message: '{"message":"m","stack":"s"}', stacktrace: [] });
  });

  it('B3: an Error subclass with name = "" and an anonymous constructor → "Error", never ""', () => {
    const Anon = class extends Error {};
    Object.defineProperty(Anon, 'name', { value: '' });
    const err = new Anon('empty name');
    err.name = '';
    const p = setup();
    captureException(err);
    expect(p.events()[0]?.error_class).toBe('Error');
  });

  it('B3: a subclass keeps its constructor name when name is the inherited "Error"; a set name wins', () => {
    class CheckoutError extends Error {}
    class Named extends Error {
      override name = 'Custom';
    }
    const p = setup();
    captureException(new CheckoutError('a'));
    captureException(new Named('b'));
    expect(p.events().map((e) => e.error_class)).toEqual(['CheckoutError', 'Custom']);
  });

  it('B3: a DOMException takes the Error branch with its name as the class (decision 42)', () => {
    const p = setup();
    captureException(new DOMException('aborted', 'AbortError'));
    expect(p.events()[0]).toMatchObject({ error_class: 'AbortError', message: 'aborted' });
  });

  it('B3: a non-string message property is stringified', () => {
    const err = new Error('x');
    (err as { message: unknown }).message = 42;
    const p = setup();
    captureException(err);
    expect(p.events()[0]?.message).toBe('42');
  });
});

describe('B3: nothing throws — the four unserialisable inputs are kept with markers', () => {
  it('B3: a cyclic req (a Node IncomingMessage shape) → metadata.req.socket "[Circular]", truncated_keys ["req.socket"]', () => {
    const req: Record<string, unknown> = { method: 'GET', url: '/checkout', headers: { host: 'app' } };
    req.socket = req;
    const p = setup();
    expect(() => captureException(new Error('x'), { metadata: { req } })).not.toThrow();
    const [event] = p.events();
    expect(event?.metadata.req).toEqual({ method: 'GET', url: '/checkout', headers: { host: 'app' }, socket: '[Circular]' });
    expect(event?.metadata.truncated_keys).toEqual(['req.socket']);
  });

  it('B3: { metadata: { n: 10n } } → metadata.n "10", truncated_keys ["n"]', () => {
    const p = setup();
    expect(() => captureException(new Error('x'), { metadata: { n: 10n } })).not.toThrow();
    expect(p.events()[0]?.metadata.n).toBe('10');
    expect(p.events()[0]?.metadata.truncated_keys).toEqual(['n']);
  });

  it('B3: a throwing toJSON → "[unserializable]" at its path; a throwing getter likewise', () => {
    const p = setup();
    const bad = {
      toJSON() {
        throw new Error('x');
      },
    };
    const withGetter = {
      get flaky(): string {
        throw new Error('no');
      },
    };
    expect(() => captureException(new Error('x'), { metadata: { bad, g: withGetter } })).not.toThrow();
    const [event] = p.events();
    expect(event?.metadata.bad).toBe('[unserializable]');
    expect(event?.metadata.g).toEqual({ flaky: '[unserializable]' });
    expect(event?.metadata.truncated_keys).toEqual(['bad', 'g.flaky']);
  });

  it('B3: a thrown object whose toString throws → message "[unserializable]", and captureMessage of one likewise', () => {
    const p = setup();
    const thrower = {
      toString() {
        throw new Error('no string for you');
      },
    };
    expect(() => captureException(thrower)).not.toThrow();
    expect(() => captureMessage(thrower as unknown as string)).not.toThrow();
    expect(p.events()[0]).toMatchObject({ error_class: 'NonError', message: '[unserializable]' });
    expect(p.events()[1]).toMatchObject({ error_class: 'Message', message: '[unserializable]' });
  });

  it('B3: the three substitutions in one call → truncated_keys lists req.socket, n, bad', () => {
    const req: Record<string, unknown> = { url: '/x' };
    req.socket = req;
    const p = setup();
    captureException(new Error('x'), {
      metadata: {
        req,
        n: 10n,
        bad: {
          toJSON() {
            throw new Error('x');
          },
        },
      },
    });
    expect(p.events()[0]?.metadata.truncated_keys).toEqual(['req.socket', 'n', 'bad']);
  });

  it('B3: the path form — an array element by numeric index: items.0.socket', () => {
    const item: Record<string, unknown> = { id: 1 };
    item.socket = item;
    const p = setup();
    captureException(new Error('x'), { metadata: { items: [item] } });
    const [event] = p.events();
    expect(event?.metadata.items).toEqual([{ id: 1, socket: '[Circular]' }]);
    expect(event?.metadata.truncated_keys).toEqual(['items.0.socket']);
  });

  it('B3: a removed top-level key subsumes its substituted sub-paths — a 70 KiB cyclic req beside request_id → ["req"]', () => {
    const req: Record<string, unknown> = { body: 'x'.repeat(70 * 1024) };
    req.socket = req;
    const p = setup();
    captureException(new Error('x'), { metadata: { req, request_id: 'req_1' } });
    const [event] = p.events();
    expect(event?.metadata.req).toBeUndefined();
    expect(event?.metadata.request_id).toBe('req_1');
    expect(event?.metadata.truncated_keys).toEqual(['req']);
  });

  it('B3: functions and undefined are omitted from objects and null in arrays, exactly as JSON.stringify does', () => {
    const { value, substituted } = sanitize({ f() {}, u: undefined, arr: [() => 1, undefined, 1], nan: NaN, d: new Date(0) });
    expect(value).toEqual({ arr: [null, null, 1], nan: null, d: '1970-01-01T00:00:00.000Z' });
    expect(substituted).toEqual([]);
  });

  it('B3: a shared (non-cyclic) reference is not a cycle', () => {
    const shared = { k: 1 };
    expect(sanitize({ a: shared, b: shared })).toEqual({ value: { a: { k: 1 }, b: { k: 1 } }, substituted: [] });
  });

  it('metadataWireBytes: +5 per < > & and +3 per U+2028 / U+2029 over the UTF-8 length', () => {
    expect(metadataWireBytes('{"a":"<>&"}')).toBe(11 + 15);
    expect(metadataWireBytes('{"a":"\u2028\u2029"}')).toBe(14 + 6);
    expect(metadataWireBytes('{"a":"é"}')).toBe(10);
  });
});
