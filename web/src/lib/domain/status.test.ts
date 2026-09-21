import { describe, expect, it } from 'vitest';
import type { SessionInfo } from '$lib/api/types';
import { activityStatus, providerName, runtimeStatus } from './status';

function sess(over: Partial<SessionInfo>): SessionInfo {
	return {
		key: 'k',
		sessionId: 'id',
		runtimeId: '',
		runtimeState: 'cold',
		harness: 'codex',
		cwd: '/x',
		state: 'idle',
		activity: 'idle',
		generation: 0,
		pid: 0,
		createdAt: '',
		generationStartedAt: '',
		...over
	};
}

describe('status presentation', () => {
	it('maps every known state to label + icon — never color alone', () => {
		for (const state of ['cold', 'warm', 'starting', 'stopping', 'active']) {
			const p = runtimeStatus(sess({ runtimeState: state }));
			expect(p.label.length).toBeGreaterThan(0);
			expect(p.icon).toBeDefined();
		}
		for (const a of ['idle', 'active', 'waiting_input']) {
			const p = activityStatus(sess({ activity: a }));
			expect(p.label.length).toBeGreaterThan(0);
			expect(p.icon).toBeDefined();
		}
	});

	it('renders cold as an explicit snowflake + label', () => {
		const p = runtimeStatus(sess({ runtimeState: 'cold' }));
		expect(p.label).toBe('Cold');
	});

	it('renders waiting_input distinctly from generic active', () => {
		const p = activityStatus(sess({ activity: 'waiting_input' }));
		expect(p.label).toBe('Waiting input');
		expect(p.variant).toBe('destructive');
	});

	it('keeps unknown states legible with the raw value as label', () => {
		const p = runtimeStatus(sess({ runtimeState: 'mystery' }));
		expect(p.label).toBe('mystery');
	});
});

describe('providerName', () => {
	it('presents harness keys as product names', () => {
		expect(providerName('codex')).toBe('Codex');
		expect(providerName('devin')).toBe('Devin');
		expect(providerName('opencode')).toBe('OpenCode');
	});

	it('passes through unknown harnesses', () => {
		expect(providerName('fixture')).toBe('fixture');
	});
});
