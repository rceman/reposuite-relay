// Per-route session-detail loader: at most one in-flight getSession, and
// a superseded request can never write state for an older route.
//
// The detail route's effect owns one loader per key (and stops it on
// teardown). Each load() invalidates the previous attempt — aborting its
// fetch and marking its generation stale — so a slow response for key A
// that lands after navigation to key B resolves 'stale' and is dropped,
// never overwriting B's metadata. Refresh clicks and event-driven
// coalesced reloads go through the same loader, so two in-flight reads
// for the same key also collapse to the newest.
import type { SessionResponse } from '$lib/api/types';

export type DetailFetch = (key: string, signal: AbortSignal) => Promise<SessionResponse>;

export type DetailResult =
	| { kind: 'ok'; response: SessionResponse }
	| { kind: 'stale' }
	| { kind: 'error'; error: unknown };

export class DetailLoader {
	private generation = 0;
	private ctl: AbortController | null = null;

	/**
	 * Load key k. Supersedes any in-flight request; resolves 'stale'
	 * when this attempt was superseded or the loader was stopped.
	 */
	async load(k: string, fetcher: DetailFetch): Promise<DetailResult> {
		this.generation++;
		const gen = this.generation;
		this.ctl?.abort();
		const ctl = new AbortController();
		this.ctl = ctl;
		const current = () => gen === this.generation && !ctl.signal.aborted;
		try {
			const response = await fetcher(k, ctl.signal);
			if (!current()) return { kind: 'stale' };
			return { kind: 'ok', response };
		} catch (err) {
			if (!current()) return { kind: 'stale' };
			return { kind: 'error', error: err };
		}
	}

	/** Invalidate everything — route teardown, key change, logout. */
	stop(): void {
		this.generation++;
		this.ctl?.abort();
		this.ctl = null;
	}
}
