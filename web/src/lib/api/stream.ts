// NDJSON stream parser for the canonical event endpoint. The Relay
// transport is newline-delimited JSON over a long-lived HTTP response —
// fetch + ReadableStream + TextDecoder, never WebSocket/EventSource.
//
// Chunk boundaries are arbitrary: a line may span many chunks, a chunk
// may hold many lines, and a UTF-8 code point may split mid-sequence
// (TextDecoder stream mode handles that). One frame is bounded by the
// canonical MaxEventFrameBytes (4 MiB) — the same bound the daemon
// enforces on publication — so an unterminated runaway line aborts the
// stream instead of growing an unbounded string.
import { DecodeError, decodeRelayEvent } from './decode';
import type { RelayEvent } from './types';

/** Mirror of api.MaxEventFrameBytes — the canonical NDJSON frame bound. */
export const MAX_FRAME_BYTES = 4 << 20;

/** One decoded stream frame: either an event or a stream error frame. */
export type StreamFrame =
	| { kind: 'event'; event: RelayEvent }
	| { kind: 'error'; code: string; message: string };

function isErrorFrame(v: unknown): { code: string; message: string } | undefined {
	if (typeof v !== 'object' || v === null || Array.isArray(v)) return undefined;
	const e = (v as Record<string, unknown>).error;
	if (typeof e !== 'object' || e === null) return undefined;
	const code = (e as Record<string, unknown>).code;
	const message = (e as Record<string, unknown>).message;
	if (typeof code !== 'string' || typeof message !== 'string') return undefined;
	return { code, message };
}

/**
 * Parse an NDJSON byte stream into frames. Ends when the stream ends;
 * throws DecodeError on a malformed JSON line and RelayError-free —
 * callers distinguish stream end (iterator return) from protocol errors
 * (thrown). Aborts when `signal` fires.
 */
export async function* parseNDJSON(
	body: ReadableStream<Uint8Array>,
	signal?: AbortSignal
): AsyncGenerator<StreamFrame> {
	const reader = body.getReader();
	const decoder = new TextDecoder();
	let buf = '';
	try {
		for (;;) {
			if (signal?.aborted) {
				await reader.cancel().catch(() => undefined);
				return;
			}
			const { value, done } = await reader.read();
			if (done) break;
			buf += decoder.decode(value, { stream: true });
			let nl: number;
			while ((nl = buf.indexOf('\n')) >= 0) {
				const line = buf.slice(0, nl);
				buf = buf.slice(nl + 1);
				if (line === '') continue;
				yield decodeLine(line);
			}
			// The undelivered remainder IS the current frame-in-progress;
			// beyond the canonical bound it can never become a valid event.
			if (buf.length > MAX_FRAME_BYTES) {
				throw new DecodeError('event frame exceeds the 4 MiB bound');
			}
		}
		// Final partial line (no trailing newline) is still a valid frame.
		const rest = decoder.decode();
		buf += rest;
		if (buf.trim() !== '') yield decodeLine(buf);
	} finally {
		reader.releaseLock();
	}
}

function decodeLine(line: string): StreamFrame {
	let v: unknown;
	try {
		v = JSON.parse(line);
	} catch {
		throw new DecodeError('event line is not JSON');
	}
	const err = isErrorFrame(v);
	if (err) return { kind: 'error', code: err.code, message: err.message };
	return { kind: 'event', event: decodeRelayEvent(v) };
}
