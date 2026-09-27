// The edge's automatic contexts (spec §6.3, TASK-04): contexts.runtime only, no version, no os, no server_name.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { captureException } from '../../src/core/capture.js';
import { init as initEdge } from '../../src/edge.js';
import { getInternalClient } from '../../src/core/state.js';
import { mockFetch } from '../helpers/fetch-mock.js';
import { resetPipeline } from '../helpers/pipeline.js';

function queued() {
  return (getInternalClient()?.queue ?? []).map((json) => JSON.parse(json));
}

beforeEach(resetPipeline);

afterEach(() => {
  delete (globalThis as { EdgeRuntime?: unknown }).EdgeRuntime;
});

describe('edge automatic contexts (spec §6.3)', () => {
  it('contexts.runtime.name is "edge" when EdgeRuntime is undefined; no version, no os, no server_name', () => {
    mockFetch();
    initEdge({ endpoint: 'http://127.0.0.1:1/ingest', apiKey: 'k' });
    captureException(new Error('x'));
    const [event] = queued();
    expect(event.contexts).toEqual({ runtime: { name: 'edge' } });
    expect(event.server_name).toBeUndefined();
  });

  it("contexts.runtime.name is Next's global EdgeRuntime string when defined", () => {
    (globalThis as { EdgeRuntime?: unknown }).EdgeRuntime = 'edge-runtime.vercel.sh';
    mockFetch();
    initEdge({ endpoint: 'http://127.0.0.1:1/ingest', apiKey: 'k' });
    captureException(new Error('x'));
    const [event] = queued();
    expect(event.contexts).toEqual({ runtime: { name: 'edge-runtime.vercel.sh' } });
  });
});
