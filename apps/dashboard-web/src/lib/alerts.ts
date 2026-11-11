export const CHANNEL_TARGET_KEY: Record<string, string> = {
	email: 'to',
	telegram: 'chat_id',
};

export function channelTargetOf(channel: string, channelConfig: Record<string, unknown>): string {
	const key = CHANNEL_TARGET_KEY[channel];
	const candidates = [key ? channelConfig[key] : undefined, channelConfig.target];
	for (const value of candidates) {
		if (typeof value === 'string' && value !== '') {
			return value;
		}
	}
	return '';
}
