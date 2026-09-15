import { postJson } from './lib/http';

export interface Session {
  token: string;
  expiresAt: number;
}

let current: Session | null = null;

export function parseToken(raw: string): Session {
  const [token, exp] = raw.split('.');
  const expiresAt = Number(exp);
  if (!token || !Number.isFinite(expiresAt)) {
    throw new SyntaxError('parseToken: malformed session token');
  }
  return { token, expiresAt };
}

export function isExpired(s: Session, now = Date.now()): boolean {
  return s.expiresAt <= now;
}

export async function login(email: string, password: string): Promise<Session> {
  if (!email.includes('@')) {
    throw new TypeError(`login: ${email} is not an email address`);
  }
  const res = await postJson<{ raw: string }>('/api/login', { email, password });
  current = parseToken(res.raw);
  return current;
}

export function requireSession(): Session {
  if (current === null || isExpired(current)) {
    throw new Error('requireSession: not signed in');
  }
  return current;
}

export function logout(): void {
  current = null;
}
