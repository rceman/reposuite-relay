// DetailLoader: a superseded detail request resolves 'stale' and its
// fetch is aborted — a slow response for an old route key can never
// overwrite a newer route's session state.
import { describe, expect, it } from 'vitest';
import { DetailLoader } from './detail';
import type { SessionResponse } from '$lib/api/types';

function resp(key: string): SessionResponse {
	return {
		daemon: {
			instanceId: 'i',
			pid: 1,
			apiVersion: 1,
			uptimeSeconds: 0,
			sessionCount: 1,
			activeSessions: 0,
			coldSessions: 1
		},
		session: {
			key,
			sessionId: `id-${key}`,
			runtimeId: '',
			runtimeState: 'cold',
			harness: 'codex',
			cwd: '/tmp',
			state: 'idle',
			activity: 'idle',
			generation: 0,
			pid: 0,
			createdAt: '2026-01-01T00:00:00Z',
			generationStartedAt: '2026-01-01T00:00:00Z'
		}
	};
}

interface Deferred {
	promise: Promise<SessionResponse>;
	resolve: (v: SessionResponse) => void;
	reject: (e: unknown) => void;
}

function deferred(): Deferred {
	let resolve: (v: SessionResponse) => void = () => undefined;
	let reject: (e: unknown) => void = () => undefined;
	const promise = new Promise<SessionResponse>((res, rej) => {
		resolve = res;
		reject = rej;
	});
	return { promise, resolve, reject };
}

function fetcherFor(
	pending: Record<string, Deferred>,
	signals: Record<string, AbortSignal>
) {
	return (k: string, signal: AbortSignal): Promise<SessionResponse> => {
		signals[k] = signal;
		const d = deferred();
		pending[k] = d;
		return d.promise;
	};
}

describe('DetailLoader', () => {
	it('drops a stale response: A in flight, route moves to B, A lands last', async () => {
		const loader = new DetailLoader();
		const pending: Record<string, Deferred> = {};
		const signals: Record<string, AbortSignal> = {};
		const fetcher = fetcherFor(pending, signals);

		const ra = loader.load('a', fetcher); // GET /sessions/A in flight
		const rb = loader.load('b', fetcher); // route switches to B

		// The superseded fetch is aborted immediately.
		expect(signals.a?.aborted).toBe(true);
		expect(signals.b?.aborted).toBe(false);

		pending.b?.resolve(resp('b'));
		const outB = await rb;
		expect(outB).toEqual({ kind: 'ok', response: resp('b') });

		// A completes LAST — it must resolve stale, never overwrite B.
		pending.a?.resolve(resp('a'));
		await expect(ra).resolves.toEqual({ kind: 'stale' });
	});

	it('supersedes an in-flight refresh for the same key', async () => {
		const loader = new DetailLoader();
		const pending: Record<string, Deferred[]> = {};
		const fetcher = (k: string, _signal: AbortSignal): Promise<SessionResponse> => {
			const d = deferred();
			(pending[k] ??= []).push(d);
			return d.promise;
		};
		const r1 = loader.load('a', fetcher);
		const r2 = loader.load('a', fetcher); // coalesced refresh supersedes
		pending.a?.[1]?.resolve(resp('a'));
		await expect(r2).resolves.toMatchObject({ kind: 'ok' });
		pending.a?.[0]?.resolve(resp('a'));
		await expect(r1).resolves.toEqual({ kind: 'stale' });
	});

	it('stop() invalidates an in-flight request and aborts its fetch', async () => {
		const loader = new DetailLoader();
		const pending: Record<string, Deferred> = {};
		const signals: Record<string, AbortSignal> = {};
		const r = loader.load('a', fetcherFor(pending, signals));
		loader.stop(); // route teardown / logout
		expect(signals.a?.aborted).toBe(true);
		pending.a?.resolve(resp('a'));
		await expect(r).resolves.toEqual({ kind: 'stale' });
	});

	it('a load after stop() works normally (new route effect)', async () => {
		const loader = new DetailLoader();
		loader.stop();
		const out = await loader.load('a', (k) => Promise.resolve(resp(k)));
		expect(out).toMatchObject({ kind: 'ok' });
	});

	it('propagates a live error for the current request only', async () => {
		const loader = new DetailLoader();
		const pending: Record<string, Deferred> = {};
		const r = loader.load('a', fetcherFor(pending, {}));
		const err = new Error('boom');
		pending.a?.reject(err);
		await expect(r).resolves.toEqual({ kind: 'error', error: err });
	});
});
