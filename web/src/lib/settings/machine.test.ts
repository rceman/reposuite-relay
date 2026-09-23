// Machine-token panel: the one-time credential reveal — success shows
// the token exactly once, durability=false shows the warning state (not
// an error), any failure reveals nothing, rotation is never retried,
// and dismiss/logout clear the reference.
import { describe, expect, it } from 'vitest';
import { MachineTokenPanel } from './machine.svelte';
import { RelayError } from '$lib/api/errors';
import type {
	DaemonInfo,
	MachineTokenRotateResponse,
	MachineTokenStatusResponse
} from '$lib/api/types';

const daemon = (): DaemonInfo => ({
	instanceId: 'i',
	pid: 1,
	apiVersion: 2,
	uptimeSeconds: 0,
	sessionCount: 0,
	activeSessions: 0,
	coldSessions: 0
});

const TOK = 'a'.repeat(64);

function reads(over: Partial<{
	status: () => Promise<MachineTokenStatusResponse>;
	rotate: () => Promise<MachineTokenRotateResponse>;
}> = {}) {
	let rotateCalls = 0;
	return {
		rotateCalls: () => rotateCalls,
		reads: {
			getMachineTokenStatus:
				over.status ??
				(async () => ({ daemon: daemon(), configured: true })),
			rotateMachineToken:
				over.rotate ??
				(async () => {
					rotateCalls++;
					return { daemon: daemon(), token: TOK, durabilityConfirmed: true };
				})
		}
	};
}

describe('MachineTokenPanel', () => {
	it('status loads configured metadata only', async () => {
		const { reads: r } = reads();
		const p = new MachineTokenPanel(r);
		await p.status();
		expect(p.configured).toBe(true);
		expect(p.statusError).toBeNull();
	});

	it('status failure reports error without touching token', async () => {
		const { reads: r } = reads({
			status: async () => {
				throw new RelayError('denied', { code: 'ADMIN_REQUIRED', status: 403 });
			}
		});
		const p = new MachineTokenPanel(r);
		p.token = TOK;
		await p.status();
		expect(p.statusError).toBe('denied');
		expect(p.token).toBe(TOK); // reveal state is rotation-scoped
	});

	it('clean rotation reveals the new token', async () => {
		const { reads: r } = reads();
		const p = new MachineTokenPanel(r);
		expect(await p.rotate()).toBe(true);
		expect(p.token).toBe(TOK);
		expect(p.durabilityConfirmed).toBe(true);
		expect(p.error).toBeNull();
	});

	it('durability=false reveals the token WITH the warning flag', async () => {
		const { reads: r } = reads({
			rotate: async () => ({ daemon: daemon(), token: TOK, durabilityConfirmed: false })
		});
		const p = new MachineTokenPanel(r);
		expect(await p.rotate()).toBe(true);
		expect(p.token).toBe(TOK);
		expect(p.durabilityConfirmed).toBe(false);
	});

	it('API failure reveals nothing and is never retried', async () => {
		let calls = 0;
		const { reads: r } = reads({
			rotate: async () => {
				calls++;
				throw new RelayError('boom', { code: 'INTERNAL', status: 500 });
			}
		});
		const p = new MachineTokenPanel(r);
		expect(await p.rotate()).toBe(false);
		expect(p.token).toBeNull();
		expect(p.error).toBe('boom');
		expect(calls).toBe(1); // exactly one rotate per user action
	});

	it('transport failure reveals nothing and is never retried', async () => {
		let calls = 0;
		const { reads: r } = reads({
			rotate: async () => {
				calls++;
				throw new Error('network down');
			}
		});
		const p = new MachineTokenPanel(r);
		expect(await p.rotate()).toBe(false);
		expect(p.token).toBeNull();
		expect(p.error).toContain('unknown'); // ambiguous-outcome message
		expect(calls).toBe(1);
	});

	it('decoder failure reveals nothing and is never retried', async () => {
		let calls = 0;
		const { reads: r } = reads({
			rotate: async () => {
				calls++;
				throw new RelayError('decode failed', {
					code: 'INVALID_RESPONSE',
					status: 200
				});
			}
		});
		const p = new MachineTokenPanel(r);
		expect(await p.rotate()).toBe(false);
		expect(p.token).toBeNull();
		expect(calls).toBe(1);
	});

	it('dismiss clears the complete reveal state', async () => {
		const { reads: r } = reads();
		const p = new MachineTokenPanel(r);
		await p.rotate();
		p.shown = true;
		p.copyState = 'copied';
		p.clearReveal(); // dismiss / navigation / logout path
		expect(p.token).toBeNull();
		expect(p.durabilityConfirmed).toBe(true);
		expect(p.shown).toBe(false);
		expect(p.copyState).toBe('idle');
	});

	it('a second rotation clears the previous token BEFORE settling', async () => {
		const A = 'a'.repeat(64);
		let resolveB!: (v: MachineTokenRotateResponse) => void;
		const gate = new Promise<MachineTokenRotateResponse>((res) => {
			resolveB = res;
		});
		let calls = 0;
		const { reads: r } = reads({
			rotate: async () => {
				calls++;
				if (calls === 1) {
					return { daemon: daemon(), token: A, durabilityConfirmed: true };
				}
				return gate;
			}
		});
		const p = new MachineTokenPanel(r);
		await p.rotate();
		expect(p.token).toBe(A);
		p.shown = true;
		p.copyState = 'copied';

		const pending = p.rotate(); // second rotation in flight
		// The prior credential and ALL reveal state are already gone —
		// before the ambiguous second request resolves.
		expect(p.token).toBeNull();
		expect(p.shown).toBe(false);
		expect(p.copyState).toBe('idle');
		resolveB({ daemon: daemon(), token: 'b'.repeat(64), durabilityConfirmed: true });
		expect(await pending).toBe(true);
		expect(p.token).toBe('b'.repeat(64));
		expect(p.shown).toBe(false); // fresh credential arrives masked
		expect(p.copyState).toBe('idle');
	});

	it('ambiguous second rotation leaves no stale credential', async () => {
		const A = 'a'.repeat(64);
		let calls = 0;
		const { reads: r } = reads({
			rotate: async () => {
				calls++;
				if (calls === 1) {
					return { daemon: daemon(), token: A, durabilityConfirmed: true };
				}
				throw new Error('network down'); // transport ambiguity
			}
		});
		const p = new MachineTokenPanel(r);
		await p.rotate();
		expect(p.token).toBe(A);
		expect(await p.rotate()).toBe(false);
		expect(p.token).toBeNull(); // never the superseded token
		expect(p.error).toContain('unknown');
		expect(calls).toBe(2); // one explicit action each, no retry
	});

	it('clipboard copy is explicit and failure keeps the token', async () => {
		const { reads: r } = reads();
		const p = new MachineTokenPanel(r);
		await p.rotate();
		// Node test env has no clipboard — failure path.
		await p.copyToken();
		expect(p.copyState).toBe('failed');
		expect(p.token).toBe(TOK);
	});
});
