import { describe, it, expect } from 'vitest';
import { handoffRestatement, localTimeIn, ISO_WEEKDAY_NAMES } from './oncall-format';
import { oncallScheduleFixture } from '$lib/fixtures/oncall';

// sentinel-user-report-issue §4.4a, fe.md rows O3 and O5.

describe('handoffRestatement (O5)', () => {
	it('restates a weekly schedule as one sentence naming the day, the time and the zone', () => {
		expect(handoffRestatement(oncallScheduleFixture)).toBe(
			'Hands over every Monday at 09:00 Asia/Jakarta.'
		);
	});

	it('restates a daily schedule without inventing a day', () => {
		const sentence = handoffRestatement({ ...oncallScheduleFixture, kind: 'daily' });
		expect(sentence).toBe('Hands over every day at 09:00 Asia/Jakarta.');
		for (const day of Object.values(ISO_WEEKDAY_NAMES)) {
			expect(sentence).not.toContain(day);
		}
	});

	// The API rejects this combination with a 400 (§4.4a). The restatement says so rather than
	// guessing at a day, because a sentence that reads correctly is exactly what would hide it.
	it('says a weekly schedule with no weekday cannot be saved', () => {
		expect(
			handoffRestatement({ ...oncallScheduleFixture, handoffWeekday: null })
		).toContain('cannot be saved');
	});

	// SR-4's encoding, not `Date#getDay()`: 1 is Monday and 7 is Sunday. Off-by-one here moves the
	// whole rota by a day and nothing else in the UI would notice.
	it('uses ISO 8601 weekday numbering', () => {
		expect(ISO_WEEKDAY_NAMES[1]).toBe('Monday');
		expect(ISO_WEEKDAY_NAMES[7]).toBe('Sunday');
		expect(handoffRestatement({ ...oncallScheduleFixture, handoffWeekday: 7 })).toContain(
			'Sunday'
		);
	});
});

describe('localTimeIn (O3)', () => {
	it('formats the instant in the named IANA zone, not in the process zone', () => {
		const at = new Date('2026-09-04T02:00:00Z');
		expect(localTimeIn('Asia/Jakarta', at)).toContain('09:00');
		expect(localTimeIn('UTC', at)).toContain('02:00');
	});

	// §3 takes IANA names only, so a fixed offset must not render as a working choice: returning
	// null is what lets the editor say "the API will refuse this" before the user saves.
	it('returns null for a fixed offset and for a name Intl does not know', () => {
		expect(localTimeIn('+07:00')).toBeNull();
		expect(localTimeIn('Not/AZone')).toBeNull();
	});
});
