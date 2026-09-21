// Live-controller recovery tests: reconnect after the last applied seq,
// CURSOR_TOO_OLD / CURSOR_AHEAD rehydration, 401 auth expiry, and abort
// lifecycle — all through injected fakes, no real network or sleeps.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { DecodeError } from '$lib/api/decode';
import { RelayError } from '$lib/api/errors';
import type { StreamFrame } from '$lib/api/stream';
import type { RelayEvent, TranscriptPage } from '$lib/api/types';
import { SessionLive, type LiveDeps } from './live.svelte';

const AT = '2026-01-01T00:00:00Z';

function event(seq: number, type = 'message.agent.delta'): RelayEvent {
	return {
		seq,
		sessionId: 's1',
		type,
		at: AT,
		durable: false,
		payload: { turnId: 't1', text: `chunk-${seq}` }
	};
}

function transcriptPage(throughSeq: number): TranscriptPage {
	return { throughSeq, records: [], hasMoreBefore: false };
}

/** One controllable fake NDJSON subscription. */
class FakeStream {
	readonly frames: StreamFrame[] = [];
	private waiters: ((f: StreamFrame | 'end') => void)[] = [];
	private rejecters: ((e: unknown) => void)[] = [];
	ended = false;

	constructor(
		readonly after: number,
		signal: AbortSignal
	) {
		signal.addEventListener('abort', () => this.end());
	}

	push(f: StreamFrame): void {
		const w = this.waiters.shift();
		if (w) w(f);
		else this.frames.push(f);
	}

	end(): void {
		if (this.ended) return;
		this.ended = true;
		for (const w of this.waiters.splice(0)) w('end');
	}

	fail(err: unknown): void {
		if (this.ended) return;
		this.ended = true;
		for (const r of this.rejecters.splice(0)) r(err);
		for (const w of this.waiters.splice(0)) w('end');
	}

	private next(): Promise<StreamFrame | 'end'> {
		if (this.frames.length > 0) return Promise.resolve(this.frames.shift() as StreamFrame);
		if (this.ended) return Promise.resolve('end');
		return new Promise((res, rej) => {
			this.waiters.push(res);
			this.rejecters.push(rej);
		});
	}

	iterable(): AsyncIterable<StreamFrame> {
		const self = this;
		return {
			[Symbol.asyncIterator]() {
				return {
					async next(): Promise<IteratorResult<StreamFrame>> {
						const v = await self.next();
						return v === 'end' ? { done: true, value: undefined } : { done: false, value: v };
					}
				};
			}
		};
	}
}

interface Fake {
	deps: LiveDeps;
	streams: FakeStream[];
	pages: TranscriptPage[];
	openErrors: unknown[];
	loads: number;
	unauthorized: number;
}

function fake(): Fake {
	const f: Fake = {
		streams: [],
		pages: [transcriptPage(5)],
		openErrors: [],
		loads: 0,
		unauthorized: 0,
		deps: undefined as unknown as LiveDeps
	};
	f.deps = {
		loadTranscript: () => {
			f.loads++;
			return Promise.resolve(f.pages.shift() ?? transcriptPage(50));
		},
		openStream: (after, signal) => {
			const err = f.openErrors.shift();
			if (err !== undefined) return Promise.reject(err);
			const s = new FakeStream(after, signal);
			f.streams.push(s);
			return Promise.resolve(s.iterable());
		},
		onUnauthorized: () => {
			f.unauthorized++;
		}
	};
	return f;
}

/** Flush pending microtasks (stream open, event application). */
async function tick(n = 5): Promise<void> {
	for (let i = 0; i < n; i++) await Promise.resolve();
}

describe('SessionLive', () => {
	beforeEach(() => {
		vi.useFakeTimers();
	});
	afterEach(() => {
		vi.useRealTimers();
	});

	it('bootstraps: snapshot → subscribe after throughSeq → live', async () => {
		const f = fake();
		const live = new SessionLive('s1', f.deps);
		await live.start();
		await tick();
		expect(f.streams).toHaveLength(1);
		expect(f.streams[0]?.after).toBe(5); // the snapshot cursor, not record seq
		expect(live.state).toBe('live');
		live.stop();
	});

	it('applies live events into the timeline', async () => {
		const f = fake();
		const live = new SessionLive('s1', f.deps);
		await live.start();
		await tick();
		f.streams[0]?.push({ kind: 'event', event: event(6) });
		f.streams[0]?.push({ kind: 'event', event: event(7) });
		await tick();
		expect(live.view.rows).toHaveLength(1);
		expect(live.view.rows[0]).toMatchObject({ kind: 'agent', live: true, text: 'chunk-6chunk-7' });
		live.stop();
	});

	it('reconnects after the last applied seq on stream end', async () => {
		const f = fake();
		const live = new SessionLive('s1', f.deps);
		await live.start();
		await tick();
		f.streams[0]?.push({ kind: 'event', event: event(6) });
		f.streams[0]?.end();
		await tick();
		expect(live.state).toBe('reconnecting');
		await vi.advanceTimersByTimeAsync(250);
		await tick();
		expect(f.streams).toHaveLength(2);
		expect(f.streams[1]?.after).toBe(6); // last applied seq, not throughSeq
		live.stop();
	});

	it('reconnects on a stream error frame', async () => {
		const f = fake();
		const live = new SessionLive('s1', f.deps);
		await live.start();
		await tick();
		f.streams[0]?.push({
			kind: 'error',
			code: 'SUBSCRIBER_EVICTED',
			message: 'subscriber evicted: queue full'
		});
		await tick();
		await vi.advanceTimersByTimeAsync(250);
		await tick();
		expect(f.streams).toHaveLength(2);
		live.stop();
	});

	it('CURSOR_TOO_OLD rehydrates and re-subscribes from the new throughSeq', async () => {
		const f = fake();
		f.openErrors.push(new RelayError('too old', { code: 'CURSOR_TOO_OLD', status: 409 }));
		f.pages.push(transcriptPage(77)); // rehydration page
		const live = new SessionLive('s1', f.deps);
		await live.start();
		await tick(10);
		expect(f.loads).toBe(2); // initial + rehydrate
		// The rejected openStream never registered a stream — the next
		// successful subscribe uses the NEW throughSeq.
		expect(f.streams).toHaveLength(1);
		expect(f.streams[0]?.after).toBe(77);
		expect(live.state).toBe('live');
		live.stop();
	});

	it('CURSOR_AHEAD rehydrates once then surfaces a persistent failure', async () => {
		const f = fake();
		f.openErrors.push(
			new RelayError('ahead', { code: 'CURSOR_AHEAD', status: 409 }),
			new RelayError('ahead', { code: 'CURSOR_AHEAD', status: 409 }),
			new RelayError('ahead', { code: 'CURSOR_AHEAD', status: 409 }),
			new RelayError('ahead', { code: 'CURSOR_AHEAD', status: 409 })
		);
		const live = new SessionLive('s1', f.deps);
		await live.start();
		await tick(15);
		expect(live.state).toBe('offline');
		expect(live.error).toContain('CURSOR_AHEAD');
		live.stop();
	});

	it('401 goes offline and refreshes auth — no reconnect loop', async () => {
		const f = fake();
		f.openErrors.push(new RelayError('unauthorized', { code: 'UNAUTHORIZED', status: 401 }));
		const live = new SessionLive('s1', f.deps);
		await live.start();
		await tick(10);
		expect(f.unauthorized).toBe(1);
		expect(live.state).toBe('offline');
		await vi.advanceTimersByTimeAsync(10_000);
		expect(f.streams).toHaveLength(0); // never retried
		live.stop();
	});

	it('stop() aborts the stream and cancels pending reconnects', async () => {
		const f = fake();
		const live = new SessionLive('s1', f.deps);
		await live.start();
		await tick();
		f.streams[0]?.end();
		await tick();
		live.stop();
		await vi.advanceTimersByTimeAsync(10_000);
		expect(f.streams).toHaveLength(1); // no reconnect after unmount
	});

	it('a protocol/decode error goes offline without reconnecting', async () => {
		const f = fake();
		// Malformed wire data (unsafe seq, bad JSON) surfaces as DecodeError
		// at the parse boundary — reconnecting would decode the same frame.
		f.openErrors.push(new DecodeError('event frame exceeds the 4 MiB bound'));
		const live = new SessionLive('s1', f.deps);
		await live.start();
		await tick(10);
		expect(live.state).toBe('offline');
		expect(live.error).toContain('protocol error');
		await vi.advanceTimersByTimeAsync(10_000);
		expect(f.streams).toHaveLength(0);
		live.stop();
	});
});
