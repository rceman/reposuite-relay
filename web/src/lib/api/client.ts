// Single typed entry point for the Relay HTTP API. All /v1 access goes
// through relayRequest: same-origin fetch on the admin browser cookie,
// X-Relay-CSRF attached to unsafe methods from the live auth state, and
// the {error:{code,message}} envelope normalized into RelayError.
import {
	DecodeError,
	decodeCancelResponse,
	decodeDaemonResponse,
	decodeMachineTokenRotate,
	decodeMachineTokenStatus,
	decodeInputResponse,
	decodeProjectList,
	decodeRuntimeList,
	decodeTelemetryHealth,
	decodeProjectResponse,
	decodePromptResponse,
	decodeSessionList,
	decodeSessionResponse,
	decodeTranscriptPage
} from './decode';
import { errorFromResponse, transportError } from './errors';
import type {
	CancelResponse,
	ConfigBody,
	CreateSessionBody,
	CreateProjectBody,
	DaemonResponse,
	InputBody,
	InputResponse,
	ProjectList,
	MachineTokenRotateResponse,
	MachineTokenStatusResponse,
	ProjectResponse,
	PromptResponse,
	RuntimeList,
	TelemetryHealth,
	SessionList,
	SessionResponse,
	TranscriptPage,
	UpdateProjectBody
} from './types';

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

async function getJSON<T>(
	path: string,
	decode: (v: unknown) => T,
	init?: RequestInit
): Promise<T> {
	const resp = await relayRequest(path, init);
	let body: unknown;
	try {
		body = await resp.json();
	} catch {
		throw new DecodeError('body is not JSON');
	}
	return decode(body);
}

async function sendJSON<T>(
	path: string,
	method: string,
	body: unknown,
	decode: (v: unknown) => T
): Promise<T> {
	const resp = await relayRequest(path, {
		method,
		headers: { 'Content-Type': 'application/json' },
		body: body === undefined ? '{}' : JSON.stringify(body)
	});
	let parsed: unknown;
	try {
		parsed = await resp.json();
	} catch {
		throw new DecodeError('body is not JSON');
	}
	return decode(parsed);
}

/**
 * sessionPath is the single builder for session-scoped /v1 paths. Keys
 * match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ but are still encoded so no
 * arbitrary path fragment can ever be concatenated into the URL.
 */
export function sessionPath(key: string, suffix = ''): string {
	return `/v1/sessions/${encodeURIComponent(key)}${suffix}`;
}

export const api = {
	/** GET /v1/sessions — observer read; never wakes a COLD session. */
	listSessions: (): Promise<SessionList> => getJSON('/v1/sessions', decodeSessionList),

	/**
	 * GET /v1/runtimes — global live harness-runtime inventory.
	 * Observer-only: never wakes a COLD session or spawns a harness.
	 */
	listRuntimes: (init?: { signal?: AbortSignal }): Promise<RuntimeList> =>
		getJSON('/v1/runtimes', decodeRuntimeList, init),

	/**
	 * GET /v1/telemetry/repodex — observer-only RepoDex telemetry health.
	 * Never wakes a session or starts RepoDex; carries no credentials.
	 */
	telemetryHealth: (init?: { signal?: AbortSignal }): Promise<TelemetryHealth> =>
		getJSON('/v1/telemetry/repodex', decodeTelemetryHealth, init),

	/**
	 * GET /v1/sessions/{key} — observer read; never wakes a COLD session.
	 * An optional AbortSignal lets the route cancel a stale in-flight
	 * detail request on navigation (a superseded response must never
	 * overwrite a newer route's state).
	 */
	getSession: (key: string, init?: { signal?: AbortSignal }): Promise<SessionResponse> =>
		getJSON(sessionPath(key), decodeSessionResponse, init),

	/** POST /v1/sessions/{provider} — durable COLD creation. */
	createSession: (provider: string, body: CreateSessionBody): Promise<SessionResponse> =>
		sendJSON(`/v1/sessions/${encodeURIComponent(provider)}`, 'POST', body, decodeSessionResponse),

	/** GET /v1/sessions/{key}/transcript?limit=N — bounded durable tail. */
	getTranscript: (key: string, limit: number): Promise<TranscriptPage> =>
		getJSON(`${sessionPath(key, '/transcript')}?limit=${limit}`, decodeTranscriptPage),

	/** POST /v1/sessions/{key}/prompt — 202 on acceptance. */
	prompt: (key: string, text: string): Promise<PromptResponse> =>
		sendJSON(sessionPath(key, '/prompt'), 'POST', { text }, decodePromptResponse),

	/** POST /v1/sessions/{key}/cancel — request turn interruption. */
	cancel: (key: string): Promise<CancelResponse> =>
		sendJSON(sessionPath(key, '/cancel'), 'POST', {}, decodeCancelResponse),

	/** POST /v1/sessions/{key}/input — answer a requested-input request. */
	answerInput: (key: string, body: InputBody): Promise<InputResponse> =>
		sendJSON(sessionPath(key, '/input'), 'POST', body, decodeInputResponse),

	/** PATCH /v1/sessions/{key}/config — changed desired fields only. */
	patchConfig: (key: string, body: ConfigBody): Promise<SessionResponse> =>
		sendJSON(sessionPath(key, '/config'), 'PATCH', body, decodeSessionResponse),

	/** DELETE /v1/sessions/{key} — durable deletion, not just a stop. */
	deleteSession: (key: string): Promise<DaemonResponse> =>
		sendJSON(sessionPath(key), 'DELETE', undefined, decodeDaemonResponse),

	/** GET /v1/projects — the local presentation catalog. */
	listProjects: (): Promise<ProjectList> => getJSON('/v1/projects', decodeProjectList),

	/** POST /v1/projects — create a Relay-local grouping project. */
	createProject: (body: CreateProjectBody): Promise<ProjectResponse> =>
		sendJSON('/v1/projects', 'POST', body, decodeProjectResponse),

	/** PATCH /v1/projects/{id} — changed fields only. */
	patchProject: (id: string, body: UpdateProjectBody): Promise<ProjectResponse> =>
		sendJSON(
			`/v1/projects/${encodeURIComponent(id)}`,
			'PATCH',
			body,
			decodeProjectResponse
		),

	/** DELETE /v1/projects/{id} — removes grouping metadata only. */
	deleteProject: (id: string): Promise<DaemonResponse> =>
		sendJSON(`/v1/projects/${encodeURIComponent(id)}`, 'DELETE', undefined, decodeDaemonResponse),

	/**
	 * GET /v1/settings/machine-token — presence metadata only; the
	 * persistent credential is never returned by any GET.
	 */
	getMachineTokenStatus: (): Promise<MachineTokenStatusResponse> =>
		getJSON('/v1/settings/machine-token', decodeMachineTokenStatus),

	/**
	 * POST /v1/settings/machine-token/rotate — the explicit rotation.
	 * Returns the NEW credential exactly once. NEVER retried: a lost
	 * response is ambiguous (the rotation may have committed) and a
	 * second rotate would invalidate the committed credential.
	 */
	rotateMachineToken: (): Promise<MachineTokenRotateResponse> =>
		sendJSON(
			'/v1/settings/machine-token/rotate',
			'POST',
			{},
			decodeMachineTokenRotate
		)
};

/**
 * Open the canonical NDJSON event stream: GET /v1/sessions/{key}/events
 * ?after=<seq>. Safe method — the admin cookie authenticates without a
 * CSRF header. The caller owns parsing (stream.ts) and abort (signal).
 * Non-2xx responses (401, 409 CURSOR_TOO_OLD/CURSOR_AHEAD) throw RelayError
 * before streaming begins.
 */
export async function openEventStream(
	key: string,
	after: number,
	signal: AbortSignal
): Promise<Response> {
	const resp = await relayRequest(`${sessionPath(key, '/events')}?after=${after}`, { signal });
	if (resp.body === null) {
		throw new DecodeError('events: empty body');
	}
	return resp;
}
