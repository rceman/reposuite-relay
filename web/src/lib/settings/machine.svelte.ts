// Settings → Machine API access controller. The persistent machine
// credential is never readable — only rotation returns a value, and
// exactly once. The revealed token and its presentation state (masked,
// copied) live ONLY in this component-scoped controller: never in a
// shared store, never in browser persistence, and cleared on
// dismiss/navigation/logout — and BEFORE every new rotation attempt,
// so a stale or superseded credential can never remain displayed.
//
// Rotation is never retried automatically: a transport failure after
// the POST was sent is ambiguous — the rename commit point may already
// have passed — and a second rotate would invalidate a credential the
// caller might already hold. The caller must rotate again explicitly.
import { RelayError } from '$lib/api/errors';
import type { MachineTokenRotateResponse, MachineTokenStatusResponse } from '$lib/api/types';

function describe(err: unknown): string {
	if (err instanceof RelayError) {
		// A Relay API error is classified server-side — its concrete
		// message (e.g. the pre-commit failure) is safe to show.
		return err.message;
	}
	// Transport/decode failure after the POST was sent is ambiguous —
	// the rotation may already have committed.
	return (
		'Rotation outcome may be unknown. Existing machine clients may ' +
		'have been invalidated — rotate again explicitly to obtain a ' +
		'fresh token.'
	);
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
	/** Masked by default — explicit user action reveals plaintext. */
	shown = $state(false);
	/** Copy feedback — never implies persistence of the credential. */
	copyState = $state<'idle' | 'copied' | 'failed'>('idle');
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
			this.statusError =
				err instanceof RelayError
					? err.message
					: 'Failed to load machine-token status';
		}
	}

	/**
	 * One explicit user action → exactly one rotate call — never a
	 * retry. The previous revealed credential is discarded BEFORE the
	 * request is issued: a second rotation may commit a different token
	 * or leave the outcome ambiguous, so the old reveal must never
	 * survive into it. Success reveals the new token masked; ANY
	 * failure leaves `token` null.
	 */
	async rotate(): Promise<boolean> {
		if (this.busy) return false;
		this.clearReveal();
		this.busy = true;
		this.error = null;
		try {
			const r = await this.reads.rotateMachineToken();
			this.token = r.token;
			this.durabilityConfirmed = r.durabilityConfirmed;
			return true;
		} catch (err) {
			this.error = describe(err);
			return false;
		} finally {
			this.busy = false;
		}
	}

	/**
	 * Drop ALL reveal state — the credential plus its presentation:
	 * raw token, durability flag, mask, copy feedback. Called on
	 * explicit dismiss, navigation, logout, and before each rotation.
	 */
	clearReveal(): void {
		this.token = null;
		this.durabilityConfirmed = true;
		this.shown = false;
		this.copyState = 'idle';
	}

	/** Explicit user copy only — never automatic. */
	async copyToken(): Promise<void> {
		const tok = this.token;
		if (tok === null) return;
		try {
			await navigator.clipboard.writeText(tok);
			this.copyState = 'copied';
		} catch {
			// Clipboard denied — token stays available for manual copy.
			this.copyState = 'failed';
		}
	}
}
