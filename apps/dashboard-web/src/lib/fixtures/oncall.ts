// sentinel-user-report-issue §4.4a's `GET` response, transcribed as a typed fixture — the same
// role `report-detail.ts` plays for §4.2, and for the same reason (fe.md task 1).
//
// The member list is deliberately mixed: `eligible: true` and `eligible: false` in one schedule,
// because O4's requirement is that an ineligible member is rendered greyed with a reason rather
// than silently dropped, and a fixture with only eligible members cannot catch a component that
// drops them.

import type { OncallCandidate, OncallResponse, OncallSchedule } from '$lib/types/oncall';

export const oncallScheduleFixture: OncallSchedule = {
	id: 'a4d2c8e1-77b3-4f0a-9c62-1e5d3b8a7f40',
	name: 'Primary',
	timezone: 'Asia/Jakarta',
	kind: 'weekly',
	handoffTime: '09:00',
	handoffWeekday: 1,
	members: [
		{ userId: 'usr_2Nk4', name: 'Devni K.', position: 0, eligible: true },
		{ userId: 'usr_7Qm1', name: 'Andrew C.', position: 1, eligible: false },
	],
	overrides: [
		{
			id: '0f2c4d6e-8a1b-4c3d-9e5f-7a8b9c0d1e2f',
			userId: 'usr_7Qm1',
			startsAt: '2026-09-10T00:00:00Z',
			endsAt: '2026-09-14T00:00:00Z',
		},
	],
};

/** The populated case, resolved by the rotation. */
export const oncallResponseFixture: OncallResponse = {
	schedule: oncallScheduleFixture,
	currentlyOnCall: { userId: 'usr_2Nk4', name: 'Devni K.', via: 'rotation' },
};

/** The same schedule while an override is in force — O6's second `via` value. */
export const oncallResponseViaOverrideFixture: OncallResponse = {
	schedule: oncallScheduleFixture,
	currentlyOnCall: { userId: 'usr_7Qm1', name: 'Andrew C.', via: 'override' },
};

/** O1: an organization that has never created a schedule, or whose schedule was deactivated. */
export const oncallEmptyResponseFixture: OncallResponse = {
	schedule: null,
	currentlyOnCall: null,
};

/** A schedule with members but nobody resolvable — the "nobody" O6 has to say out loud. */
export const oncallNobodyOnCallFixture: OncallResponse = {
	schedule: oncallScheduleFixture,
	currentlyOnCall: null,
};

/**
 * The org members the editor may offer for the rotation or an override. The loader has already
 * narrowed these to `ISSUE_WRITE_ROLES`, which is why `usr_7Qm1` is absent from the list even
 * though the schedule above still carries him: his org role dropped to `viewer`, so he is
 * `eligible: false` on the rotation and no longer addable to it. That asymmetry is the point.
 */
export const oncallCandidatesFixture: OncallCandidate[] = [
	{ userId: 'usr_2Nk4', name: 'Devni K.', role: 'engineer' },
	{ userId: 'usr_9Zt8', name: 'Rina P.', role: 'admin' },
];
