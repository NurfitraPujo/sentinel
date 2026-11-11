import type { Session } from '@auth/core/types';

declare global {
	namespace App {
		// A11 (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md N7f): `claimedBy`/`claimedAt` are
		// optional so every OTHER `error(status, message)` call site in the app (which passes a
		// bare string, coerced to `{message}`) keeps compiling unchanged. Only agent-ops.ts's
		// `throwClaimConflict` sets them, on claim/release 409s.
		interface Error {
			claimedBy?: string | null;
			claimedAt?: string | null;
		}
		interface Locals {
			auth(): Promise<Session | null>;
			getSession(): Promise<Session | null>;
			currentOrg?: { id: string; name: string; slug: string };
			orgRole?: string;
		}
		interface PageData {
			session: Session | null;
		}
		// interface PageState {}
		// interface Platform {}
	}
}

export {};
