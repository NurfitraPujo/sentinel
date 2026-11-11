import { describe, it, expect } from 'vitest';
import { resolveEffectiveProjectRole } from '../src/lib/db/queries/organizations';

describe('Multi-Tenant Isolation & Effective Role Resolution', () => {
  it('should inherit organization role when no project override is defined', () => {
    const orgRole = 'engineer';
    const effectiveRole = resolveEffectiveProjectRole(orgRole, null);
    expect(effectiveRole).toBe('engineer');
  });

  it('should prioritize project override role over organization role', () => {
    const orgRole = 'engineer';
    const projectOverride = 'viewer';
    const effectiveRole = resolveEffectiveProjectRole(orgRole, projectOverride);
    expect(effectiveRole).toBe('viewer');
  });

  it('should enforce owner role precedence if project override is undefined', () => {
    const orgRole = 'owner';
    const effectiveRole = resolveEffectiveProjectRole(orgRole, undefined);
    expect(effectiveRole).toBe('owner');
  });
});

import { RESERVED_SLUGS } from '../src/lib/db/queries/organizations';

describe('Reserved Slugs Validation', () => {
  it('should include key system routes in RESERVED_SLUGS', () => {
    expect(RESERVED_SLUGS).toContain('admin');
    expect(RESERVED_SLUGS).toContain('settings');
    expect(RESERVED_SLUGS).toContain('api');
    expect(RESERVED_SLUGS).toContain('auth');
    expect(RESERVED_SLUGS).toContain('docs');
    expect(RESERVED_SLUGS).toContain('billing');
    expect(RESERVED_SLUGS).toContain('support');
  });
});
