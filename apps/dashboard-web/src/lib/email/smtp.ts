import { env } from '$env/dynamic/private';

export interface SmtpConfig {
	server: string;
	from: string;
}

export function loadSmtpConfig(): SmtpConfig {
	return {
		server: env.EMAIL_SERVER || '',
		from: env.EMAIL_FROM || 'noreply@sentinel.local',
	};
}

export function isSmtpConfigured(): boolean {
	const config = loadSmtpConfig();
	return !!config.server && config.server.startsWith('smtp://');
}