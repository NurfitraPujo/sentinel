import nodemailer from 'nodemailer';
import { env } from '$env/dynamic/private';
import { log } from '$lib/server/observability/log';

let cachedTransporter: nodemailer.Transporter | null = null;
let cachedServerConfig: string | null = null;

function escapeHtml(str: string): string {
  return str
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#039;');
}

function getTransporter(emailServer: string): nodemailer.Transporter {
  if (!cachedTransporter || cachedServerConfig !== emailServer) {
    if (emailServer.startsWith('smtp://debug')) {
      cachedTransporter = nodemailer.createTransport({
        jsonTransport: true,
      });
    } else {
      cachedTransporter = nodemailer.createTransport(emailServer);
    }
    cachedServerConfig = emailServer;
  }
  return cachedTransporter;
}

export async function sendInvitationEmail(
  toEmail: string,
  inviteUrl: string,
  organizationName: string
): Promise<boolean> {
  const emailServer = env.EMAIL_SERVER;
  const emailFrom = env.EMAIL_FROM ?? 'noreply@sentinel.local';

  if (!emailServer) {
    log.info('email.invitation_skipped', { reason: 'EMAIL_SERVER not configured', toEmail });
    return false;
  }

  try {
    const transporter = getTransporter(emailServer);

    const safeOrgName = escapeHtml(organizationName.replace(/[\r\n]/g, ' '));
    const safeInviteUrl = escapeHtml(inviteUrl);

    const mailOptions = {
      from: emailFrom,
      to: toEmail,
      subject: `Invitation to join ${safeOrgName} on Sentinel`,
      html: `
        <div style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; background-color: #0f172a; color: #f8fafc; padding: 24px; border-radius: 8px;">
          <h2 style="color: #3b82f6; margin-top: 0;">You've been invited to Sentinel</h2>
          <p>You have been invited to join <strong>${safeOrgName}</strong> on Sentinel.</p>
          <p>Click the link below to accept your invitation:</p>
          <p style="margin: 24px 0;">
            <a href="${safeInviteUrl}" style="background-color: #3b82f6; color: #ffffff; padding: 10px 18px; border-radius: 4px; text-decoration: none; font-weight: 600; display: inline-block;">Accept Invitation</a>
          </p>
          <p style="color: #94a3b8; font-size: 13px;">Or copy and paste this URL into your browser:</p>
          <p style="color: #94a3b8; font-size: 13px; font-family: monospace; word-break: break-all;">${safeInviteUrl}</p>
        </div>
      `,
    };

    const info = await transporter.sendMail(mailOptions);
    log.info('email.invitation_sent', { toEmail, messageId: info.messageId });
    return true;
  } catch (err) {
    log.error('email.invitation_failed', { toEmail, error: err });
    return false;
  }
}

// Manual Issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8): notification kinds this can be
// called with. Deliberately a SUBSET of notifications.kind's DB CHECK -- notify.ts's email
// policy (Q7/Q11) only ever calls this for 'commented'|'claimed'|'status_changed'|'resolved'|
// 'question_asked'; 'linked' and 'progress_update' are in-app only and never reach this function.
export type IssueNotificationKind =
  | 'commented'
  | 'claimed'
  | 'status_changed'
  | 'resolved'
  | 'question_asked';

const NOTIFICATION_SUBJECTS: Record<IssueNotificationKind, string> = {
  commented: 'New comment on',
  claimed: 'Claimed:',
  status_changed: 'Status changed:',
  resolved: 'Resolved:',
  question_asked: 'Question needs your input:',
};

/**
 * §8: the single email side-effect of notify.ts's fan-out, following the invitation pattern --
 * called AFTER the mutation's transaction has committed, best-effort, returning a `delivered`
 * boolean rather than throwing (a failed email must never roll back or fail the mutation it
 * describes). The email is never a reply channel (§8) -- it only ever links to the thread; body
 * content is not echoed into the email beyond the issue title, so nothing from `bodyMd` needs
 * escaping here beyond the title itself.
 */
export async function sendIssueNotificationEmail(
  toEmail: string,
  issueUrl: string,
  kind: IssueNotificationKind,
  issueTitle: string
): Promise<boolean> {
  const emailServer = env.EMAIL_SERVER;
  const emailFrom = env.EMAIL_FROM ?? 'noreply@sentinel.local';

  if (!emailServer) {
    log.info('email.issue_notification_skipped', { reason: 'EMAIL_SERVER not configured', toEmail, kind });
    return false;
  }

  try {
    const transporter = getTransporter(emailServer);

    const safeTitle = escapeHtml(issueTitle.replace(/[\r\n]/g, ' '));
    const safeIssueUrl = escapeHtml(issueUrl);
    const subjectPrefix = NOTIFICATION_SUBJECTS[kind];

    const mailOptions = {
      from: emailFrom,
      to: toEmail,
      subject: `${subjectPrefix} ${safeTitle}`,
      html: `
        <div style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; background-color: #0f172a; color: #f8fafc; padding: 24px; border-radius: 8px;">
          <h2 style="color: #3b82f6; margin-top: 0;">${subjectPrefix} ${safeTitle}</h2>
          <p style="margin: 24px 0;">
            <a href="${safeIssueUrl}" style="background-color: #3b82f6; color: #ffffff; padding: 10px 18px; border-radius: 4px; text-decoration: none; font-weight: 600; display: inline-block;">View on Sentinel</a>
          </p>
          <p style="color: #94a3b8; font-size: 13px;">Or copy and paste this URL into your browser:</p>
          <p style="color: #94a3b8; font-size: 13px; font-family: monospace; word-break: break-all;">${safeIssueUrl}</p>
        </div>
      `,
    };

    const info = await transporter.sendMail(mailOptions);
    log.info('email.issue_notification_sent', { toEmail, kind, messageId: info.messageId });
    return true;
  } catch (err) {
    log.error('email.issue_notification_failed', { toEmail, kind, error: err });
    return false;
  }
}
