// NDJSON parser tests: arbitrary chunk boundaries, UTF-8 splits, the
// 4 MiB frame bound, and stream error frames — no network involved.
import { describe, expect, it } from 'vitest';
import { DecodeError } from './decode';
import { MAX_FRAME_BYTES, parseNDJSON, type StreamFrame } from './stream';
import type { RelayEvent } from './types';

function ev(seq: number, type = 'message.agent.delta'): string {
	return JSON.stringify({
		seq,
		sessionId: 's1',
		type,
		at: '2026-01-01T00:00:00Z',
		durable: false,
		payload: { turnId: 't1', text: 'x' }
	});
}

/** Feed raw byte chunks as one ReadableStream. */
function streamOf(chunks: Uint8Array[]): ReadableStream<Uint8Array> {
	return new ReadableStream({
		start(c) {
			for (const b of chunks) c.enqueue(b);
			c.close();
		}
	});
}

function encode(s: string): Uint8Array {
	return new TextEncoder().encode(s);
}

async function collect(chunks: Uint8Array[]): Promise<StreamFrame[]> {
	const out: StreamFrame[] = [];
	for await (const f of parseNDJSON(streamOf(chunks))) out.push(f);
	return out;
}

describe('parseNDJSON', () => {
	it('decodes one event per chunk', async () => {
		const frames = await collect([encode(ev(1) + '\n'), encode(ev(2) + '\n')]);
		expect(frames).toHaveLength(2);
		expect(frames[0]).toMatchObject({ kind: 'event' });
		expect((frames[0] as { event: RelayEvent }).event.seq).toBe(1);
	});

	it('decodes many events packed in one chunk', async () => {
		const frames = await collect([encode(`${ev(1)}\n${ev(2)}\n${ev(3)}\n`)]);
		expect(frames).toHaveLength(3);
		expect((frames[2] as { event: RelayEvent }).event.seq).toBe(3);
	});

	it('decodes one event split across many chunks', async () => {
		const line = ev(7);
		const parts = [line.slice(0, 10), line.slice(10, 40), `${line.slice(40)}\n`];
		const frames = await collect(parts.map(encode));
		expect(frames).toHaveLength(1);
		expect((frames[0] as { event: RelayEvent }).event.seq).toBe(7);
	});

	it('decodes a UTF-8 code point split across chunks', async () => {
		// 'é' is 2 bytes (0xC3 0xA9); split it mid-sequence.
		const line = ev(1).replace('"text":"x"', '"text":"héllo"');
		const bytes = encode(line + '\n');
		const cut = bytes.findIndex((b, i) => b === 0xc3 && bytes[i + 1] === 0xa9);
		expect(cut).toBeGreaterThan(0);
		const frames = await collect([bytes.slice(0, cut + 1), bytes.slice(cut + 1)]);
		expect(frames).toHaveLength(1);
		const e = (frames[0] as { event: RelayEvent }).event;
		expect((e.payload as { text: string }).text).toBe('héllo');
	});

	it('accepts a final line without a trailing newline', async () => {
		const frames = await collect([encode(ev(1))]);
		expect(frames).toHaveLength(1);
	});

	it('rejects a malformed JSON line', async () => {
		await expect(collect([encode('{not json\n')])).rejects.toThrow(DecodeError);
	});

	it('aborts on a frame exceeding the canonical bound', async () => {
		const big = `{"seq":1,"pad":"${'x'.repeat(MAX_FRAME_BYTES)}"`;
		await expect(collect([encode(big)])).rejects.toThrow(DecodeError);
	});

	it('accepts an ASCII frame at exactly the canonical bound', async () => {
		// The bound is a BYTE bound: a well-formed event whose line is
		// exactly MAX_FRAME_BYTES is still inside it.
		const base = ev(1);
		// base has a 1-char pad; extending it to MAX_FRAME_BYTES - base + 1
		// makes the total line exactly MAX_FRAME_BYTES.
		const line = ev(1).replace('"text":"x"', `"text":"${'x'.repeat(MAX_FRAME_BYTES - base.length + 1)}"`);
		expect(encode(line).length).toBe(MAX_FRAME_BYTES);
		const frames = await collect([encode(line + '\n')]);
		expect(frames).toHaveLength(1);
		expect(frames[0]?.kind).toBe('event');
	});

	it('rejects a frame one byte over the bound even when newline-delimited', async () => {
		const base = ev(1);
		const line = ev(1).replace('"text":"x"', `"text":"${'x'.repeat(MAX_FRAME_BYTES - base.length + 2)}"`);
		expect(encode(line).length).toBe(MAX_FRAME_BYTES + 1);
		await expect(collect([encode(`${line}\n`)])).rejects.toThrow(DecodeError);
	});

	it('enforces the bound on UTF-8 bytes, not JS string length (CJK)', async () => {
		// '界' is 1 UTF-16 code unit but 3 UTF-8 bytes: the string length
		// stays below 4 MiB while the wire frame exceeds it. The old
		// code-unit check would have accepted this frame.
		const pad = '界'.repeat(2 << 20); // ~2 Mi chars, ~6 MiB on the wire
		const line = `{"seq":1,"pad":"${pad}"}`;
		expect(line.length).toBeLessThan(MAX_FRAME_BYTES);
		expect(encode(line).length).toBeGreaterThan(MAX_FRAME_BYTES);
		await expect(collect([encode(`${line}\n`)])).rejects.toThrow(DecodeError);
	});

	it('enforces the bound on UTF-8 bytes, not JS string length (emoji)', async () => {
		// '😀' is 2 UTF-16 code units but 4 UTF-8 bytes.
		const pad = '😀'.repeat(1500 << 10); // ~3 Mi code units, ~6 MiB
		const line = `{"seq":1,"pad":"${pad}"}`;
		expect(line.length).toBeLessThan(MAX_FRAME_BYTES);
		expect(encode(line).length).toBeGreaterThan(MAX_FRAME_BYTES);
		await expect(collect([encode(line)])).rejects.toThrow(DecodeError);
	});

	it('recognizes a stream error frame separately from events', async () => {
		const errFrame = JSON.stringify({
			error: { code: 'SUBSCRIBER_EVICTED', message: 'subscriber evicted: queue full' }
		});
		const frames = await collect([encode(`${ev(1)}\n${errFrame}\n`)]);
		expect(frames).toHaveLength(2);
		expect(frames[1]).toEqual({
			kind: 'error',
			code: 'SUBSCRIBER_EVICTED',
			message: 'subscriber evicted: queue full'
		});
	});

	it('does not mistake a normal event for an error frame', async () => {
		// An event with a payload field named "error" is still an event.
		const withErr = JSON.stringify({
			seq: 4,
			sessionId: 's1',
			type: 'turn.failed',
			at: '2026-01-01T00:00:00Z',
			durable: true,
			payload: { turnId: 't', error: 'boom' }
		});
		const frames = await collect([encode(withErr + '\n')]);
		expect(frames[0]?.kind).toBe('event');
	});
});
