// Normalized frontend error for every failure of a Relay HTTP call:
// transport failure, non-2xx status (with the server's error envelope
// when present), or a malformed body. No stack traces, no internals.
import { decodeErrorBody } from './decode';

export class RelayError extends Error {
	readonly code: string;
	readonly status: number;
	readonly retryAfter: number | undefined;

	constructor(
		message: string,
		opts: { code?: string; status?: number; retryAfter?: number } = {}
	) {
		super(message);
		this.name = 'RelayError';
		this.code = opts.code ?? 'INTERNAL';
		this.status = opts.status ?? 0;
		this.retryAfter = opts.retryAfter;
	}
}

function parseRetryAfter(h: Headers): number | undefined {
	const raw = h.get('Retry-After');
	if (raw === null) return undefined;
	const seconds = Number.parseInt(raw, 10);
	return Number.isFinite(seconds) && seconds >= 0 ? seconds : undefined;
}

/** Build the RelayError for a non-2xx response, consuming its body. */
export async function errorFromResponse(resp: Response): Promise<RelayError> {
	const retryAfter = parseRetryAfter(resp.headers);
	let text = '';
	try {
		text = await resp.text();
	} catch {
		text = '';
	}
	let parsed: unknown;
	try {
		parsed = JSON.parse(text);
	} catch {
		parsed = undefined;
	}
	const body = decodeErrorBody(parsed);
	if (body) {
		return new RelayError(body.error.message, {
			code: body.error.code,
			status: resp.status,
			...(retryAfter !== undefined ? { retryAfter } : {})
		});
	}
	return new RelayError(`request failed (HTTP ${resp.status})`, {
		status: resp.status,
		...(retryAfter !== undefined ? { retryAfter } : {})
	});
}

/** Network-level failure (connection refused, DNS, reset). */
export function transportError(err: unknown): RelayError {
	if (err instanceof RelayError) return err;
	return new RelayError('cannot reach the Relay daemon', { code: 'TRANSPORT' });
}
