import { describe, expect, it } from 'vitest';
import {
	activityText,
	bindingActivity,
	formatBytes,
	groupByProvider,
	providerLabel,
	providerName,
	segmentMemory,
	shortRuntimeId,
	totalsMemory
} from './model';
import type { RuntimeInfo, RuntimeList } from '../api/types';

function rt(over: Partial<RuntimeInfo> = {}): RuntimeInfo {
	return {
		runtimeId: 'a1b2c3d4e5f6',
		harness: 'codex',
		shared: true,
		pid: 1234,
		startedAt: '2026-01-01T00:00:00Z',
		uptimeSeconds: 60,
		state: 'warm',
		sessionCount: 0,
		activeSessionCount: 0,
		waitingInputCount: 0,
		mutationCount: 0,
		sessions: [],
		resources: { available: false },
		...over
	};
}

function list(runtimes: RuntimeInfo[], totals?: Partial<RuntimeList['totals']>): RuntimeList {
	return {
		daemon: {
			instanceId: 'x', pid: 1, apiVersion: 1, uptimeSeconds: 1,
			sessionCount: 0, activeSessions: 0, coldSessions: 0
		},
		sampledAt: '2026-01-01T00:00:00Z',
		runtimes,
		totals: {
			runtimeCount: runtimes.length,
			sessionCount: 0,
			activeSessionCount: 0,
			waitingInputCount: 0,
			measuredRuntimeCount: runtimes.length,
			pssBytes: 0,
			rssBytes: 0,
			...totals
		}
	};
}

const MiB = 1024 * 1024;

describe('runtime view model', () => {
	it('groups in canonical provider order with empty slots', () => {
		const gs = groupByProvider(list([
			rt({ harness: 'devin', runtimeId: 'd1' }),
			rt({ harness: 'codex' }),
			rt({ harness: 'codex', runtimeId: 'c2' })
		]));
		expect(gs.map((g) => g.harness)).toEqual(['codex', 'devin', 'opencode']);
		expect(gs[0]?.runtimeCount).toBe(2);
		expect(gs[1]?.runtimeCount).toBe(1);
		expect(gs[2]?.runtimeCount).toBe(0);
	});

	it('keeps unknown harness kinds after canonical providers', () => {
		const gs = groupByProvider(list([rt({ harness: 'fixture' })]));
		expect(gs.map((g) => g.harness)).toEqual(['codex', 'devin', 'opencode', 'fixture']);
		expect(gs[3]?.runtimeCount).toBe(1);
		expect(gs[3]?.name).toBe('fixture');
	});

	it('sums measured PSS only', () => {
		const g = groupByProvider(list([
			rt({ resources: { available: true, pssBytes: 100 * MiB } }),
			rt({ runtimeId: 'b', resources: { available: false } })
		]))[0];
		if (!g) throw new Error('missing group');
		expect(g.pssBytes).toBe(100 * MiB);
		expect(g.measuredCount).toBe(1);
		expect(segmentMemory(g)).toBe('≥100 MiB');
	});

	it('formats exact and unavailable memory', () => {
		const exact = groupByProvider(list([
			rt({ resources: { available: true, pssBytes: 100 * MiB } })
		]))[0];
		if (!exact) throw new Error('missing group');
		expect(segmentMemory(exact)).toBe('100 MiB');
		const empty = groupByProvider(list([]))[0];
		expect(empty ? segmentMemory(empty) : '?').toBe('PSS —');
		expect(formatBytes(512)).toBe('512 B');
		expect(formatBytes(1.5 * 1024 * MiB)).toBe('1.50 GiB');
	});

	it('marks partial totals', () => {
		const l = list([rt()], { measuredRuntimeCount: 2, runtimeCount: 3, pssBytes: 241 * MiB });
		expect(totalsMemory(l)).toBe('≥241 MiB');
		expect(totalsMemory(list([], { measuredRuntimeCount: 0 }))).toBe('PSS —');
	});

	it('renders activity text preserving counts', () => {
		expect(activityText(rt({ sessionCount: 4, activeSessionCount: 1, waitingInputCount: 1 })))
			.toBe('1 active · 1 waiting input');
		expect(activityText(rt())).toBe('Idle');
		expect(bindingActivity('waiting_input')).toBe('Waiting input');
		expect(bindingActivity('idle')).toBe('Idle');
	});

	it('labels providers and shortens runtime IDs', () => {
		expect(providerLabel('codex')).toBe('Codex app-server');
		expect(providerLabel('devin')).toBe('Devin ACP');
		expect(providerName('devin')).toBe('Devin');
		expect(providerLabel('future')).toBe('future');
		expect(shortRuntimeId('a1b2c3d4e5f6')).toBe('#a1b2c3');
	});
});
