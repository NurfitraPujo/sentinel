// node's automatic contexts and server_name (spec §6.2, TASK-04).

import os from 'node:os';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { captureException } from '../../src/core/capture.js';
import { init as initNode } from '../../src/node.js';
import { nodeAutoContexts } from '../../src/node/contexts.js';
import { getInternalClient } from '../../src/core/state.js';
import { mockFetch } from '../helpers/fetch-mock.js';
import { resetPipeline, setup } from '../helpers/pipeline.js';

function queued() {
  return (getInternalClient()?.queue ?? []).map((json) => JSON.parse(json));
}

beforeEach(resetPipeline);

describe('nodeAutoContexts (spec §6.2)', () => {
  it('contexts.runtime is { name: "node", version: process.version }', () => {
    const auto = nodeAutoContexts(undefined);
    expect(auto.contexts?.runtime).toEqual({ name: 'node', version: process.version });
  });

  it('contexts.os.name maps linux/darwin/win32 to Linux/macOS/Windows; kernel_version is os.release()', () => {
    vi.spyOn(os, 'platform').mockReturnValue('linux');
    vi.spyOn(os, 'release').mockReturnValue('6.0.0');
    expect(nodeAutoContexts(undefined).contexts?.os).toEqual({ name: 'Linux', kernel_version: '6.0.0' });
    vi.spyOn(os, 'platform').mockReturnValue('darwin');
    expect(nodeAutoContexts(undefined).contexts?.os).toMatchObject({ name: 'macOS' });
    vi.spyOn(os, 'platform').mockReturnValue('win32');
    expect(nodeAutoContexts(undefined).contexts?.os).toMatchObject({ name: 'Windows' });
  });

  it('server_name: the option when a non-empty string, else os.hostname(), cut to 256 code points', () => {
    vi.spyOn(os, 'hostname').mockReturnValue('fixture-host');
    expect(nodeAutoContexts(undefined).server_name).toBe('fixture-host');
    expect(nodeAutoContexts('my-app-server').server_name).toBe('my-app-server');
    expect(nodeAutoContexts('x'.repeat(300)).server_name).toBe('x'.repeat(256));
  });
});

describe('wired through init (spec §6.2, §3.9)', () => {
  it('serverName option sets server_name', () => {
    mockFetch();
    initNode({ endpoint: 'http://127.0.0.1:1/ingest', apiKey: 'k', serverName: 'my-app-server' });
    captureException(new Error('x'));
    expect(queued()[0]?.server_name).toBe('my-app-server');
  });

  it('a non-string serverName degrades to os.hostname() with invalid_option', () => {
    mockFetch();
    vi.spyOn(os, 'hostname').mockReturnValue('fixture-host');
    const errors: string[] = [];
    initNode({ endpoint: 'http://127.0.0.1:1/ingest', apiKey: 'k', serverName: 42 as unknown as string, onError: (e) => errors.push(e.code) });
    captureException(new Error('x'));
    expect(queued()[0]?.server_name).toBe('fixture-host');
    expect(errors).toContain('invalid_option');
  });

  it('captureException on node carries contexts.runtime / os and server_name from the option', () => {
    const p = setup({}, 'node', () => ({ contexts: { runtime: { name: 'node', version: 'v20.0.0' }, os: { name: 'Linux', kernel_version: '6.0.0' } }, server_name: 'fixture-host' }));
    captureException(new Error('x'));
    const [event] = p.events();
    expect(event?.contexts?.runtime).toEqual({ name: 'node', version: 'v20.0.0' });
    expect(event?.contexts?.os).toEqual({ name: 'Linux', kernel_version: '6.0.0' });
    expect(event?.server_name).toBe('fixture-host');
  });
});
