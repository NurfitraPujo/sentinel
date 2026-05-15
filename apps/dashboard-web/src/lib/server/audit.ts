export type AuditEventType = 
	| 'magic_link_requested'
	| 'magic_link_clicked'
	| 'magic_link_failed'
	| 'magic_link_success';

export interface AuditEvent {
	type: AuditEventType;
	timestamp: string;
	email?: string;
	ip?: string;
	userAgent?: string;
	error?: string;
}

const auditLog: AuditEvent[] = [];

export function logAuditEvent(event: Omit<AuditEvent, 'timestamp'>): void {
	auditLog.push({
		...event,
		timestamp: new Date().toISOString(),
	});
	console.log('[AUDIT]', JSON.stringify(event));
}

export function getAuditLog(): AuditEvent[] {
	return [...auditLog];
}