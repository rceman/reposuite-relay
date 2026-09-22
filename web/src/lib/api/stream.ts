// NDJSON stream parser for the canonical event endpoint. The Relay
// transport is newline-delimited JSON over a long-lived HTTP response —
// fetch + ReadableStream, never WebSocket/EventSource.
//
// The parser operates on raw bytes: chunk boundaries are arbitrary, a
// chunk may hold many lines, and a UTF-8 code point may split mid-
// sequence — all handled by accumulating bytes, not text. The canonical
// MaxEventFrameBytes (4 MiB) bound is enforced on the frame's UTF-8 BYTE
// count — the same bound the daemon enforces on the wire — never on JS
// string length (UTF-16 code units), which would let a multibyte frame
// exceed the wire limit undetected. A frame is decoded to text only
// after it has been isolated and size-checked.
import { DecodeError, decodeRelayEvent } from './decode';
import type { RelayEvent } from './types';

/** Mirror of api.MaxEventFrameBytes — the canonical NDJSON frame bound. */
export const MAX_FRAME_BYTES = 4 << 20;

const NEWLINE = 0x0a;

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
 * throws DecodeError on malformed JSON or an over-bound frame — callers
 * distinguish stream end (iterator return) from protocol errors
 * (thrown). Aborts when `signal` fires.
 */
export async function* parseNDJSON(
	body: ReadableStream<Uint8Array>,
	signal?: AbortSignal
): AsyncGenerator<StreamFrame> {
	const reader = body.getReader();
	const decoder = new TextDecoder();
	// Frame-in-progress as a chunk list + exact byte count: appending a
	// chunk is O(1) amortized — bytes are copied once, when a complete
	// bounded frame is isolated, never re-encoded to count length.
	let pending: Uint8Array[] = [];
	let pendingBytes = 0;
	try {
		for (;;) {
			if (signal?.aborted) {
				await reader.cancel().catch(() => undefined);
				return;
			}
			const { value, done } = await reader.read();
			if (done) break;
			let offset = 0;
			while (offset < value.length) {
				const nl = value.indexOf(NEWLINE, offset);
				const end = nl === -1 ? value.length : nl;
				pendingBytes += end - offset;
				if (pendingBytes > MAX_FRAME_BYTES) {
					// Beyond the canonical wire bound this can never become
					// a valid frame — abort instead of buffering further.
					throw new DecodeError('event frame exceeds the 4 MiB bound');
				}
				if (nl === -1) {
					if (end > offset) pending.push(value.subarray(offset));
					break;
				}
				// A complete frame: pending tail + this chunk's slice.
				const frame = takeFrame(pending, pendingBytes, value.subarray(offset, end));
				pending = [];
				pendingBytes = 0;
				offset = nl + 1;
				if (frame.length === 0) continue; // blank line
				yield decodeFrame(decoder, frame);
			}
		}
		// Final partial line (no trailing newline) is still a valid frame.
		if (pendingBytes > 0) {
			const frame = takeFrame(pending, pendingBytes, new Uint8Array(0));
			const text = decoder.decode(frame);
			if (text.trim() !== '') yield decodeLine(text);
		}
	} finally {
		reader.releaseLock();
	}
}

/**
 * Assemble one complete frame from the pending chunk list plus the final
 * slice — one allocation, one copy, O(frame size).
 */
function takeFrame(
	pending: Uint8Array[],
	pendingBytes: number,
	last: Uint8Array
): Uint8Array {
	if (pendingBytes === last.length) return last; // whole frame in one slice
	const frame = new Uint8Array(pendingBytes);
	let off = 0;
	for (const part of pending) {
		frame.set(part, off);
		off += part.length;
	}
	frame.set(last, off);
	return frame;
}

function decodeFrame(decoder: TextDecoder, frame: Uint8Array): StreamFrame {
	return decodeLine(decoder.decode(frame));
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
