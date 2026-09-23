// Settings → Machine API access controller. The persistent machine
// credential is never readable — only rotation returns a value, and
// exactly once. The revealed token lives ONLY in this component-scoped
// state: never in a shared store, never in browser persistence, and
// cleared on dismiss/navigation/logout.
//
// Rotation is never retried automatically: a transport failure after
// the POST was sent is ambiguous — the rename commit point may already
// have passed — and a second rotate would invalidate a credential the
// caller might already hold. The caller must rotate again explicitly.
import { RelayError } from '$lib/api/errors';
import type { MachineTokenRotateResponse, MachineTokenStatusResponse } from '$lib/api/types';

function describe(err: unknown, fallback: string): string {
	return err instanceof RelayError ? err.message : fallback;
}

/** The two machine-token API calls the panel depends on (injectable). */
export interface MachineTokenReads {
	getMachineTokenStatus: () => Promise<MachineTokenStatusResponse>;
	rotateMachineToken: () => Promise<MachineTokenRotateResponse>;
}

export class MachineTokenPanel {
	/** Status metadata — never secret material. */
	configured = $state<boolean | null>(null);
	/** The one-time revealed credential — component memory only. */
	token = $state<string | null>(null);
	/** Rename committed but dirsync failed → visible warning, not an error. */
	durabilityConfirmed = $state(true);
	busy = $state(false);
	error = $state<string | null>(null);
	statusError = $state<string | null>(null);

	constructor(private readonly reads: MachineTokenReads) {}

	async status(): Promise<void> {
		try {
			const r = await this.reads.getMachineTokenStatus();
			this.configured = r.configured;
			this.statusError = null;
		} catch (err) {
			this.statusError = describe(err, 'Failed to load machine-token status');
		}
	}

	/**
	 * One explicit user action → exactly one rotate call — never a
	 * retry. Success reveals the new token; ANY failure leaves
	 * `token` untouched and surfaces an error telling the user the
	 * outcome may be unknown.
	 */
	async rotate(): Promise<boolean> {
		if (this.busy) return false;
		this.busy = true;
		this.error = null;
		try {
			const r = await this.reads.rotateMachineToken();
			this.token = r.token;
			this.durabilityConfirmed = r.durabilityConfirmed;
			return true;
		} catch (err) {
			this.error = describe(
				err,
				'Rotation outcome may be unknown. Existing machine clients may ' +
					'have been invalidated — rotate again explicitly to obtain a ' +
					'fresh token.'
			);
			return false;
		} finally {
			this.busy = false;
		}
	}

	/** Drop the revealed credential — dismiss, navigation, logout. */
	clear(): void {
		this.token = null;
		this.durabilityConfirmed = true;
	}
}
