// Single typed entry point for the Relay HTTP API. All /v1 access goes
// through relayRequest: same-origin fetch on the admin browser cookie,
// X-Relay-CSRF attached to unsafe methods from the live auth state, and
// the {error:{code,message}} envelope normalized into RelayError.
import { DecodeError, decodeSessionList } from './decode';
import { errorFromResponse, transportError } from './errors';
import type { SessionList } from './types';

const UNSAFE = new Set(['POST', 'PUT', 'PATCH', 'DELETE']);

interface ApiHooks {
	/** Current session CSRF token, undefined when unauthenticated. */
	getCsrf: () => string | undefined;
	/**
	 * Called when a /v1 request is rejected with 401: the browser session
	 * we believed live has expired or been revoked. The auth store uses
	 * this to re-bootstrap /auth/session and return to login.
	 */
	onUnauthorized: () => void;
}

let hooks: ApiHooks = {
	getCsrf: () => undefined,
	onUnauthorized: () => {}
};

/** The auth module registers its live state here at startup. */
export function configureApi(h: ApiHooks): void {
	hooks = h;
}

async function relayRequest(path: string, init: RequestInit = {}): Promise<Response> {
	const method = (init.method ?? 'GET').toUpperCase();
	const headers = new Headers(init.headers);
	headers.set('Accept', 'application/json');
	if (UNSAFE.has(method)) {
		const csrf = hooks.getCsrf();
		if (csrf !== undefined) headers.set('X-Relay-CSRF', csrf);
	}
	let resp: Response;
	try {
		resp = await fetch(path, { ...init, headers });
	} catch (err) {
		throw transportError(err);
	}
	if (!resp.ok) {
		const err = await errorFromResponse(resp);
		if (resp.status === 401) hooks.onUnauthorized();
		throw err;
	}
	return resp;
}

async function getJSON<T>(path: string, decode: (v: unknown) => T): Promise<T> {
	const resp = await relayRequest(path);
	let body: unknown;
	try {
		body = await resp.json();
	} catch {
		throw new DecodeError('body is not JSON');
	}
	return decode(body);
}

export const api = {
	/** GET /v1/sessions — observer read; never wakes a COLD session. */
	listSessions: (): Promise<SessionList> => getJSON('/v1/sessions', decodeSessionList)
};
