import { describe, it, expect, vi, beforeEach } from 'vitest';

// email.ts is only ever mocked out by every OTHER test in this repo (members.test.ts,
// invitations tests, accept-invite tests, ...) -- this file is what actually exercises it.
// `EMAIL_SERVER` is read fresh from `$env/dynamic/private` on every call, so we mock the module
// with a mutable object and vary its `EMAIL_SERVER` per test rather than re-mocking per test.
const mockEnv: Record<string, string | undefined> = {};
vi.mock('$env/dynamic/private', () => ({ env: mockEnv }));

vi.mock('$lib/server/observability/log', () => ({
  log: { info: vi.fn(), error: vi.fn(), warn: vi.fn(), debug: vi.fn() },
}));

const sendMailMock = vi.fn();
const createTransportMock = vi.fn((..._args: any[]) => ({ sendMail: sendMailMock }));
vi.mock('nodemailer', () => ({
  default: { createTransport: (...args: any[]) => createTransportMock(...args) },
}));

const { log } = await import('$lib/server/observability/log');

// email.ts caches the nodemailer transporter in MODULE state, keyed by EMAIL_SERVER. Importing it
// once for the whole file makes these tests order-dependent: whichever test first sends with a
// given EMAIL_SERVER creates the transporter, and every later test sees a cache hit -- so the
// caching test passes or fails purely on execution order (confirmed with --sequence.shuffle).
// Re-import a FRESH module per test so each starts with an empty cache.
let sendInvitationEmail: typeof import('./email').sendInvitationEmail;

describe('sendInvitationEmail', () => {
  beforeEach(async () => {
    vi.clearAllMocks();
    for (const key of Object.keys(mockEnv)) delete mockEnv[key];
    sendMailMock.mockResolvedValue({ messageId: 'msg-1' });
    vi.resetModules();
    ({ sendInvitationEmail } = await import('./email'));
  });

  it('returns false and logs (without sending) when EMAIL_SERVER is not configured', async () => {
    // No EMAIL_SERVER set -- this is the "delivery was never attempted" case that P3-9/D41's
    // response-shape fix (out of this file's scope) needs to distinguish from a real send.
    const result = await sendInvitationEmail('user@example.com', 'https://x/invitations/abc', 'Acme');

    expect(result).toBe(false);
    expect(createTransportMock).not.toHaveBeenCalled();
    expect(sendMailMock).not.toHaveBeenCalled();
    expect(log.info).toHaveBeenCalledWith(
      'email.invitation_skipped',
      expect.objectContaining({ reason: 'EMAIL_SERVER not configured' })
    );
  });

  it('sends via the configured transport and returns true on success', async () => {
    mockEnv.EMAIL_SERVER = 'smtp://localhost:1025';
    mockEnv.EMAIL_FROM = 'invites@sentinel.local';

    const result = await sendInvitationEmail(
      'user@example.com',
      'https://sentinel.local/invitations/deadbeef',
      'Acme Corp'
    );

    expect(result).toBe(true);
    expect(createTransportMock).toHaveBeenCalledWith('smtp://localhost:1025');
    expect(sendMailMock).toHaveBeenCalledTimes(1);
    const mailOptions = sendMailMock.mock.calls[0][0];
    expect(mailOptions.to).toBe('user@example.com');
    expect(mailOptions.from).toBe('invites@sentinel.local');
    expect(mailOptions.subject).toContain('Acme Corp');
    expect(mailOptions.html).toContain('https://sentinel.local/invitations/deadbeef');
    expect(log.info).toHaveBeenCalledWith(
      'email.invitation_sent',
      expect.objectContaining({ toEmail: 'user@example.com', messageId: 'msg-1' })
    );
  });

  it('defaults the From address when EMAIL_FROM is unset', async () => {
    mockEnv.EMAIL_SERVER = 'smtp://localhost:1025';

    await sendInvitationEmail('user@example.com', 'https://x/invitations/abc', 'Acme');

    expect(sendMailMock.mock.calls[0][0].from).toBe('noreply@sentinel.local');
  });

  it('escapes HTML-significant characters in the organization name and invite URL', async () => {
    mockEnv.EMAIL_SERVER = 'smtp://localhost:1025';

    await sendInvitationEmail(
      'user@example.com',
      'https://x/invitations/abc?x=1&y=2"><script>alert(1)</script>',
      '<img src=x onerror=alert(1)>Acme & Co'
    );

    const html = sendMailMock.mock.calls[0][0].html as string;
    expect(html).not.toContain('<script>');
    expect(html).not.toContain('<img');
    expect(html).toContain('&lt;img');
    expect(html).toContain('Acme &amp; Co');
    expect(html).toContain('&lt;script&gt;');
  });

  it('strips CRLF from the organization name (header/HTML injection hardening)', async () => {
    mockEnv.EMAIL_SERVER = 'smtp://localhost:1025';

    await sendInvitationEmail('user@example.com', 'https://x/invitations/abc', 'Acme\r\nBcc: evil@example.com');

    const html = sendMailMock.mock.calls[0][0].html as string;
    expect(html).not.toContain('\r');
    expect(html).not.toContain('\nBcc:');
    expect(html).toMatch(/Acme\s+Bcc: evil@example\.com/);
  });

  it('returns false and logs when the transport throws (delivery failure is observable, not silent)', async () => {
    mockEnv.EMAIL_SERVER = 'smtp://localhost:1025';
    sendMailMock.mockRejectedValueOnce(new Error('connection refused'));

    const result = await sendInvitationEmail('user@example.com', 'https://x/invitations/abc', 'Acme');

    expect(result).toBe(false);
    expect(log.error).toHaveBeenCalledWith(
      'email.invitation_failed',
      expect.objectContaining({ toEmail: 'user@example.com' })
    );
  });

  it('uses the jsonTransport (no real network) for smtp://debug URLs, without requiring sendMailMock wiring changes', async () => {
    mockEnv.EMAIL_SERVER = 'smtp://debug';

    const result = await sendInvitationEmail('user@example.com', 'https://x/invitations/abc', 'Acme');

    expect(result).toBe(true);
    expect(createTransportMock).toHaveBeenCalledWith({ jsonTransport: true });
  });

  it('caches the transporter across calls with the same EMAIL_SERVER, and rebuilds it if EMAIL_SERVER changes', async () => {
    mockEnv.EMAIL_SERVER = 'smtp://localhost:1025';
    await sendInvitationEmail('a@example.com', 'https://x/invitations/1', 'Org A');
    await sendInvitationEmail('b@example.com', 'https://x/invitations/2', 'Org A');
    expect(createTransportMock).toHaveBeenCalledTimes(1);

    mockEnv.EMAIL_SERVER = 'smtp://otherhost:1025';
    await sendInvitationEmail('c@example.com', 'https://x/invitations/3', 'Org A');
    expect(createTransportMock).toHaveBeenCalledTimes(2);
  });
});
