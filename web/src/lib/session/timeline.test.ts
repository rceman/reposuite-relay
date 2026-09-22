// Timeline reducer: durable hydration, live delta accumulation,
// draft→completion handoff, pending-input reconstruction, dedupe.
import { describe, expect, it } from 'vitest';
import { Timeline } from './timeline';
import type { RelayEvent, TranscriptRecord } from '$lib/api/types';

const AT = '2026-01-01T00:00:00Z';

function rec(seq: number, type: string, payload: unknown): TranscriptRecord {
	return { version: 1, seq, type, at: AT, payload };
}

function event(seq: number, type: string, payload: unknown, durable = false): RelayEvent {
	return { seq, sessionId: 's1', type, at: AT, payload, durable };
}

function page(seqs: { seq: number; type: string; payload: unknown }[], throughSeq: number) {
	return { throughSeq, records: seqs.map((s) => rec(s.seq, s.type, s.payload)), hasMoreBefore: false };
}

describe('Timeline', () => {
	it('renders durable message.user as a user row', () => {
		const tl = new Timeline();
		tl.hydrate(page([{ seq: 1, type: 'message.user', payload: { text: 'hello' } }], 1));
		expect(tl.state.rows).toHaveLength(1);
		expect(tl.state.rows[0]).toMatchObject({ kind: 'user', text: 'hello' });
		expect(tl.state.lastSeq).toBe(1);
	});

	it('accumulates agent deltas into one live draft', () => {
		const tl = new Timeline();
		tl.hydrate(page([], 5));
		expect(tl.applyEvent(event(6, 'message.agent.delta', { turnId: 't1', itemId: 'i1', text: 'Hel' }))).toBe('applied');
		expect(tl.applyEvent(event(7, 'message.agent.delta', { turnId: 't1', itemId: 'i1', text: 'lo' }))).toBe('applied');
		expect(tl.state.rows).toHaveLength(1);
		expect(tl.state.rows[0]).toMatchObject({ kind: 'agent', live: true, text: 'Hello' });
	});

	it('keeps drafts for different turns separate', () => {
		const tl = new Timeline();
		tl.hydrate(page([], 0));
		tl.applyEvent(event(1, 'message.agent.delta', { turnId: 't1', text: 'a' }));
		tl.applyEvent(event(2, 'message.agent.delta', { turnId: 't2', text: 'b' }));
		expect(tl.state.rows).toHaveLength(2);
	});

	it('replaces a live draft with the durable completion in place', () => {
		const tl = new Timeline();
		tl.hydrate(page([], 0));
		tl.applyEvent(event(1, 'message.agent.delta', { turnId: 't1', itemId: 'i1', text: 'partial' }));
		tl.applyEvent(event(2, 'message.agent.completed', { turnId: 't1', itemId: 'i1', text: 'final' }));
		expect(tl.state.rows).toHaveLength(1);
		expect(tl.state.rows[0]).toMatchObject({ kind: 'agent', live: false, text: 'final', seq: 2 });
	});

	it('appends a durable completion that never had a draft', () => {
		const tl = new Timeline();
		tl.hydrate(page([], 0));
		tl.applyEvent(event(1, 'message.agent.completed', { turnId: 't1', text: 'done' }, true));
		expect(tl.state.rows[0]).toMatchObject({ kind: 'agent', live: false, text: 'done' });
	});

	it('skips duplicate seq (replay overlap)', () => {
		const tl = new Timeline();
		tl.hydrate(page([{ seq: 3, type: 'message.user', payload: { text: 'a' } }], 5));
		expect(tl.applyEvent(event(5, 'message.user', { text: 'x' }))).toBe('duplicate');
		expect(tl.applyEvent(event(4, 'message.user', { text: 'x' }))).toBe('duplicate');
		expect(tl.state.rows).toHaveLength(1);
	});

	it('reconstructs pending input from durable history', () => {
		const tl = new Timeline();
		tl.hydrate(
			page(
				[
					{
						seq: 1,
						type: 'input.requested',
						payload: {
							inputId: 'in_1',
							turnId: 't',
							itemId: 'i',
							isBlocking: true,
							questions: [{ id: 'q1', header: 'H', question: 'Q?' }]
						}
					}
				],
				1
			)
		);
		expect(tl.state.pending).toHaveLength(1);
		expect(tl.state.pending[0]?.inputId).toBe('in_1');
	});

	it('resolved removes pending input', () => {
		const tl = new Timeline();
		tl.hydrate(
			page(
				[
					{ seq: 1, type: 'input.requested', payload: { inputId: 'in_1', turnId: 't', itemId: 'i', isBlocking: true, questions: [] } },
					{ seq: 2, type: 'input.resolved', payload: { inputId: 'in_1', answers: { q1: ['a'] } } }
				],
				2
			)
		);
		expect(tl.state.pending).toHaveLength(0);
	});

	it('aborted removes pending input', () => {
		const tl = new Timeline();
		tl.hydrate(
			page(
				[
					{ seq: 1, type: 'input.requested', payload: { inputId: 'in_1', turnId: 't', itemId: 'i', isBlocking: true, questions: [] } },
					{ seq: 2, type: 'input.aborted', payload: { inputId: 'in_1', reason: 'runtime died' } }
				],
				2
			)
		);
		expect(tl.state.pending).toHaveLength(0);
	});

	it('tracks multiple pending inputs by exact inputId', () => {
		const tl = new Timeline();
		const req = (id: string) => ({
			seq: 0, type: 'input.requested',
			payload: { inputId: id, turnId: 't', itemId: 'i', isBlocking: false, questions: [] }
		});
		tl.hydrate(page([], 0));
		tl.applyEvent(event(1, 'input.requested', req('in_a').payload));
		tl.applyEvent(event(2, 'input.requested', req('in_b').payload));
		tl.applyEvent(event(3, 'input.resolved', { inputId: 'in_a', answers: {} }));
		expect(tl.state.pending.map((p) => p.inputId)).toEqual(['in_b']);
	});

	it('renders system/control events quietly', () => {
		const tl = new Timeline();
		tl.hydrate(
			page(
				[
					{ seq: 1, type: 'harness.started', payload: { runtimeId: 'r', nativeSessionId: 'n', resumed: false } },
					{ seq: 2, type: 'runtime.exited', payload: { runtimeId: 'r', reason: 'stopped' } },
					{ seq: 3, type: 'session.config', payload: { model: 'm', mode: 'x' } }
				],
				3
			)
		);
		const kinds = tl.state.rows.map((r) => r.kind);
		expect(kinds).toEqual(['system', 'system', 'system']);
	});

	it('updates metrics from metrics.updated without a row', () => {
		const tl = new Timeline();
		tl.hydrate(page([], 0));
		tl.applyEvent(event(1, 'metrics.updated', { kind: 'tokens', inputTokens: 42, model: 'gpt-5' }));
		expect(tl.state.rows).toHaveLength(0);
		expect(tl.state.metrics?.inputTokens).toBe(42);
		expect(tl.state.metrics?.model).toBe('gpt-5');
	});

	it('renders an unknown event type as a safe diagnostic row', () => {
		const tl = new Timeline();
		tl.hydrate(page([], 0));
		tl.applyEvent(event(1, 'vendor.future.thing', { weird: true }));
		expect(tl.state.rows[0]).toMatchObject({ kind: 'unknown', eventType: 'vendor.future.thing' });
	});

	it('renders a malformed payload as a diagnostic row, never a crash', () => {
		const tl = new Timeline();
		tl.hydrate(page([], 0));
		tl.applyEvent(event(1, 'message.user', { text: 42 }));
		expect(tl.state.rows[0]).toMatchObject({ kind: 'system', label: 'Malformed message.user event' });
	});

	it('keeps drafts distinct for aliasing turnId/itemId pairs', () => {
		// "ab"+"c" and "a"+"bc" alias under naive concatenation — the
		// draft key must encode the pair unambiguously.
		const tl = new Timeline();
		tl.hydrate(page([], 0));
		tl.applyEvent(event(1, 'message.agent.delta', { turnId: 'ab', itemId: 'c', text: 'one' }));
		tl.applyEvent(event(2, 'message.agent.delta', { turnId: 'a', itemId: 'bc', text: 'two' }));
		expect(tl.state.rows).toHaveLength(2);
		// Each completion replaces only its own draft, in place.
		tl.applyEvent(event(3, 'message.agent.completed', { turnId: 'ab', itemId: 'c', text: 'done-1' }));
		expect(tl.state.rows).toHaveLength(2);
		expect(tl.state.rows[0]).toMatchObject({ kind: 'agent', live: false, text: 'done-1' });
		expect(tl.state.rows[1]).toMatchObject({ kind: 'agent', live: true, text: 'two' });
	});

	it('hydrate resets prior state (rehydration after cursor recovery)', () => {
		const tl = new Timeline();
		tl.hydrate(page([{ seq: 1, type: 'message.user', payload: { text: 'a' } }], 1));
		tl.applyEvent(event(2, 'message.agent.delta', { turnId: 't', text: 'live' }));
		tl.hydrate(page([{ seq: 1, type: 'message.user', payload: { text: 'a' } }], 9));
		expect(tl.state.rows).toHaveLength(1);
		expect(tl.state.rows[0]?.kind).toBe('user');
		expect(tl.cursor).toBe(9);
	});
});
