// Session live controller: owns the history→live cutover, the NDJSON
// stream lifecycle, reconnect backoff, and cursor recovery for ONE
// viewed session. Transport parsing lives in api/stream.ts, reduction in
// session/timeline.ts — this file only orchestrates.
//
//   1. load bounded transcript snapshot → hydrate timeline
//   2. subscribe events?after=throughSeq (the canonical cursor)
//   3. replay prefix + live events fold into the timeline
//   4. on interruption: reconnect after the last applied seq
//   5. CURSOR_TOO_OLD / CURSOR_AHEAD → rehydrate durable history, then
//      re-subscribe from the NEW throughSeq (never guess at gaps)
//   6. 401 → auth refresh + offline (no protected retries)
//   7. stop() aborts everything — no stream survives navigation
import { api, openEventStream } from '$lib/api/client';
import { DecodeError } from '$lib/api/decode';
import { RelayError } from '$lib/api/errors';
import { parseNDJSON, type StreamFrame } from '$lib/api/stream';
import { Timeline, type TimelineView } from './timeline';
import type { TranscriptPage } from '$lib/api/types';

export type LiveState = 'connecting' | 'live' | 'reconnecting' | 'offline';

/** Injectable seams — tests substitute fakes, production uses the API. */
export interface LiveDeps {
	loadTranscript(limit: number): Promise<TranscriptPage>;
	openStream(after: number, signal: AbortSignal): Promise<AsyncIterable<StreamFrame>>;
	/** 401 during streaming → refresh /auth/session (session may be dead). */
	onUnauthorized(): void;
}

/** Reconnect backoff schedule: bounded, capped at 2 s, no tight loop. */
const BACKOFF = [250, 500, 1000, 2000] as const;
/** Cursor-error recovery attempts before the stream is declared dead. */
const MAX_CURSOR_RECOVERY = 3;

const EMPTY_VIEW: TimelineView = {
	rows: [],
	pending: [],
	metrics: undefined,
	lastSeq: 0,
	hasMoreBefore: false
};

export class SessionLive {
	/** Render state — reassigned on every timeline mutation. */
	view = $state<TimelineView>(EMPTY_VIEW);
	state = $state<LiveState>('connecting');
	error = $state<string | null>(null);

	private readonly timeline = new Timeline();
	private abortCtl: AbortController | null = null;
	private timer: ReturnType<typeof setTimeout> | null = null;
	private stopped = false;
	private attempt = 0;
	private cursorErrors = 0;
	private transcriptLimit: number;

	constructor(
		readonly key: string,
		private readonly deps: LiveDeps,
		/** Called after state-affecting events so the page can coalesce a
		 *  session-status refresh (never one fetch per token delta). */
		private readonly onStateEvent?: (type: string) => void,
		transcriptLimit = 200
	) {
		this.transcriptLimit = transcriptLimit;
	}

	/** Bootstrap: snapshot → hydrate → subscribe. Errors surface via state. */
	async start(): Promise<void> {
		try {
			const page = await this.deps.loadTranscript(this.transcriptLimit);
			if (this.stopped) return;
			this.timeline.hydrate(page);
			this.sync();
			this.connect();
		} catch (err) {
			if (this.stopped) return;
			this.state = 'offline';
			this.error = err instanceof Error ? err.message : 'failed to load session';
		}
	}

	/** Rehydrate replaces the timeline with a fresh durable snapshot. */
	private async rehydrate(): Promise<void> {
		const page = await this.deps.loadTranscript(this.transcriptLimit);
		if (this.stopped) return;
		this.timeline.hydrate(page);
		this.sync();
	}

	/**
	 * setLimit grows the durable tail window (load-more-history) and
	 * rehydrates. The stream is re-established from the new throughSeq so
	 * events published between the old and new snapshots replay exactly.
	 * connect() cancels any pending reconnect timer and aborts the old
	 * stream AFTER the snapshot — the ≤1 stream / ≤1 timer invariant
	 * holds at every instant.
	 */
	async setLimit(limit: number): Promise<void> {
		this.transcriptLimit = limit;
		try {
			await this.rehydrate();
		} catch (err) {
			if (this.stopped) return;
			this.error = err instanceof Error ? err.message : 'history reload failed';
			return;
		}
		// User-initiated restart: fresh attempt counter, fresh cursor.
		this.attempt = 0;
		this.connect();
	}

	/**
	 * connect is the ONLY stream-establishment path: it first cancels a
	 * pending reconnect timer and aborts the previous stream (if any),
	 * so active streams ≤ 1 and pending timers ≤ 1 always.
	 */
	private connect(): void {
		if (this.stopped) return;
		this.cancelReconnectTimer();
		this.abortCtl?.abort();
		const ctl = new AbortController();
		this.abortCtl = ctl;
		this.state = this.attempt === 0 ? 'connecting' : 'reconnecting';
		void this.run(ctl);
	}

	private async run(ctl: AbortController): Promise<void> {
		const after = this.timeline.cursor;
		try {
			const frames = await this.deps.openStream(after, ctl.signal);
			this.cursorErrors = 0;
			this.state = 'live';
			this.error = null;
			for await (const frame of frames) {
				if (this.stopped || ctl.signal.aborted) return;
				if (frame.kind === 'error') {
					// STREAM_CLOSED / SUBSCRIBER_EVICTED — reconnect from the
					// last applied seq; nothing is lost while the ring holds.
					break;
				}
				if (this.timeline.applyEvent(frame.event) === 'applied') {
					// Useful stream progress — only NOW does the reconnect
					// backoff reset. An open→immediate-close stream is a
					// failure and must escalate, not restart at 250 ms.
					this.attempt = 0;
					this.sync();
					this.onStateEvent?.(frame.event.type);
				}
			}
			if (this.stopped || ctl.signal.aborted) return;
			this.scheduleReconnect();
		} catch (err) {
			if (this.stopped || ctl.signal.aborted) return;
			await this.handleError(err);
		}
	}

	private async handleError(err: unknown): Promise<void> {
		if (err instanceof RelayError && err.status === 401) {
			// Auth expiry: refresh /auth/session; the gate returns to login.
			this.deps.onUnauthorized();
			this.state = 'offline';
			this.error = 'Session expired';
			return;
		}
		if (
			err instanceof RelayError &&
			(err.code === 'CURSOR_TOO_OLD' || err.code === 'CURSOR_AHEAD')
		) {
			// The cursor can no longer be proven — discard transient live
			// reconstruction, rehydrate durable history, re-subscribe.
			this.cursorErrors++;
			if (this.cursorErrors > MAX_CURSOR_RECOVERY) {
				this.state = 'offline';
				this.error = `stream cursor unrecoverable (${err.code})`;
				return;
			}
			try {
				await this.rehydrate();
			} catch (he) {
				if (this.stopped) return;
				this.state = 'offline';
				this.error = he instanceof Error ? he.message : 'history reload failed';
				return;
			}
			this.connect();
			return;
		}
		if (err instanceof DecodeError) {
			// Protocol violation — reconnecting would decode the same frame.
			this.state = 'offline';
			this.error = `protocol error: ${err.message}`;
			return;
		}
		this.scheduleReconnect();
	}

	private scheduleReconnect(): void {
		if (this.stopped) return;
		this.cancelReconnectTimer();
		const delay = BACKOFF[Math.min(this.attempt, BACKOFF.length - 1)];
		this.attempt++;
		this.state = 'reconnecting';
		this.timer = setTimeout(() => {
			this.timer = null;
			this.connect();
		}, delay);
	}

	private cancelReconnectTimer(): void {
		if (this.timer !== null) {
			clearTimeout(this.timer);
			this.timer = null;
		}
	}

	/** Snapshot the timeline into the reactive view. */
	private sync(): void {
		this.view = { ...this.timeline.state };
	}

	/** stop aborts the stream, the pending timer, and any in-flight work. */
	stop(): void {
		this.stopped = true;
		this.cancelReconnectTimer();
		this.abortCtl?.abort();
		this.abortCtl = null;
	}
}

/** Production seam for one session: typed client + NDJSON parser. */
export function liveDeps(key: string, onUnauthorized: () => void): LiveDeps {
	return {
		loadTranscript: (limit) => api.getTranscript(key, limit),
		openStream: async (after, signal) => {
			const resp = await openEventStream(key, after, signal);
			return parseNDJSON(resp.body as ReadableStream<Uint8Array>, signal);
		},
		onUnauthorized
	};
}
