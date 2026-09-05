// sentinel-user-report-issue §4.4a — the `GET /api/organizations/{orgId}/oncall` response, which
// is the shape the on-call editor is built around.
//
// Separate from `$lib/db/queries/oncall`'s `OncallScheduleView` on purpose: that one is the
// server's row-shaped view (Date objects, an internal rotation anchor the response deliberately
// does not expose). This is the wire shape a browser actually receives.

export const ROTATION_KINDS = ['daily', 'weekly'] as const;

export type RotationKind = (typeof ROTATION_KINDS)[number];

export interface OncallMember {
	userId: string;
	name: string | null;
	position: number;
	/** `false` marks a member whose org role has since dropped below `ISSUE_WRITE_ROLES`. Shown,
	 *  skipped by the rotation, never silently deleted — the API returns them for that purpose. */
	eligible: boolean;
}

export interface OncallOverride {
	id: string;
	userId: string;
	startsAt: string;
	endsAt: string;
}

export interface OncallSchedule {
	id: string;
	name: string;
	/** An IANA zone name. A fixed offset is a `400` from the API and must not be offerable (§3). */
	timezone: string;
	kind: RotationKind;
	/** `"HH:MM"`, wall-clock in `timezone`. */
	handoffTime: string;
	/** ISO 8601 weekday, 1 = Monday … 7 = Sunday. `null` for a `daily` rotation. */
	handoffWeekday: number | null;
	members: OncallMember[];
	overrides: OncallOverride[];
}

export interface CurrentlyOnCall {
	userId: string;
	name: string | null;
	via: 'rotation' | 'override';
}

export interface OncallResponse {
	/** `null` for an organization that has never created one, or whose schedule was deactivated —
	 *  not a 404. That `null` is exactly what the editor's empty state (O1) renders. */
	schedule: OncallSchedule | null;
	/** `null` when nobody resolves. */
	currentlyOnCall: CurrentlyOnCall | null;
}

/** An org member the editor may offer for the rotation or for an override (§4.4a's `400` bar). */
export interface OncallCandidate {
	userId: string;
	name: string;
	role: string;
}
