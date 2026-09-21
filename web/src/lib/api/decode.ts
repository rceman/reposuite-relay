// Runtime decoders for the Relay wire DTOs. TypeScript types describe the
// boundary but do not validate network input — every response is narrowed
// explicitly so a malformed payload becomes a controlled DecodeError,
// never a blind `as` cast.
import type {
	AuthSession,
	DaemonInfo,
	ErrorBody,
	SessionInfo,
	SessionList,
	SessionMetrics
} from './types';

export class DecodeError extends Error {
	constructor(field: string) {
		super(`malformed response: ${field}`);
		this.name = 'DecodeError';
	}
}

function isRecord(v: unknown): v is Record<string, unknown> {
	return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function reqString(v: Record<string, unknown>, key: string): string {
	const s = v[key];
	if (typeof s !== 'string') throw new DecodeError(key);
	return s;
}

function reqNumber(v: Record<string, unknown>, key: string): number {
	const n = v[key];
	if (typeof n !== 'number' || !Number.isFinite(n)) throw new DecodeError(key);
	return n;
}

function reqBool(v: Record<string, unknown>, key: string): boolean {
	const b = v[key];
	if (typeof b !== 'boolean') throw new DecodeError(key);
	return b;
}

/** Optional string field — absent or string, anything else is malformed. */
function optString(v: Record<string, unknown>, key: string): string | undefined {
	const s = v[key];
	if (s === undefined) return undefined;
	if (typeof s !== 'string') throw new DecodeError(key);
	return s;
}

function optNumber(v: Record<string, unknown>, key: string): number | undefined {
	const n = v[key];
	if (n === undefined) return undefined;
	if (typeof n !== 'number' || !Number.isFinite(n)) throw new DecodeError(key);
	return n;
}

export function decodeAuthSession(v: unknown): AuthSession {
	if (!isRecord(v)) throw new DecodeError('authSession');
	const out: AuthSession = {
		configured: reqBool(v, 'configured'),
		authenticated: reqBool(v, 'authenticated')
	};
	const username = optString(v, 'username');
	const csrfToken = optString(v, 'csrfToken');
	const formToken = optString(v, 'formToken');
	if (username !== undefined) out.username = username;
	if (csrfToken !== undefined) out.csrfToken = csrfToken;
	if (formToken !== undefined) out.formToken = formToken;
	return out;
}

export function decodeErrorBody(v: unknown): ErrorBody | undefined {
	if (!isRecord(v) || !isRecord(v.error)) return undefined;
	const code = v.error.code;
	const message = v.error.message;
	if (typeof code !== 'string' || typeof message !== 'string') return undefined;
	return { error: { code, message } };
}

function decodeDaemonInfo(v: unknown): DaemonInfo {
	if (!isRecord(v)) throw new DecodeError('daemon');
	return {
		instanceId: reqString(v, 'instanceId'),
		pid: reqNumber(v, 'pid'),
		apiVersion: reqNumber(v, 'apiVersion'),
		uptimeSeconds: reqNumber(v, 'uptimeSeconds'),
		sessionCount: reqNumber(v, 'sessionCount'),
		activeSessions: reqNumber(v, 'activeSessions'),
		coldSessions: reqNumber(v, 'coldSessions')
	};
}

function decodeMetrics(v: unknown): SessionMetrics {
	if (!isRecord(v)) throw new DecodeError('metrics');
	const out: SessionMetrics = {};
	const str = ['model', 'mode', 'resetAt'] as const;
	for (const k of str) {
		const s = optString(v, k);
		if (s !== undefined) out[k] = s;
	}
	const num = [
		'contextUsed',
		'contextLimit',
		'inputTokens',
		'outputTokens',
		'reasoningTokens',
		'cacheTokens',
		'quotaUsed',
		'quotaLimit'
	] as const;
	for (const k of num) {
		const n = optNumber(v, k);
		if (n !== undefined) out[k] = n;
	}
	return out;
}

function decodeSessionInfo(v: unknown): SessionInfo {
	if (!isRecord(v)) throw new DecodeError('session');
	const out: SessionInfo = {
		key: reqString(v, 'key'),
		sessionId: reqString(v, 'sessionId'),
		runtimeId: reqString(v, 'runtimeId'),
		runtimeState: reqString(v, 'runtimeState'),
		harness: reqString(v, 'harness'),
		cwd: reqString(v, 'cwd'),
		state: reqString(v, 'state'),
		activity: reqString(v, 'activity'),
		generation: reqNumber(v, 'generation'),
		pid: reqNumber(v, 'pid'),
		createdAt: reqString(v, 'createdAt'),
		generationStartedAt: reqString(v, 'generationStartedAt')
	};
	const opt = ['nativeSessionId', 'model', 'mode'] as const;
	for (const k of opt) {
		const s = optString(v, k);
		if (s !== undefined) out[k] = s;
	}
	if (v.metrics !== undefined) out.metrics = decodeMetrics(v.metrics);
	return out;
}

export function decodeSessionList(v: unknown): SessionList {
	if (!isRecord(v)) throw new DecodeError('sessionList');
	const daemon = decodeDaemonInfo(v.daemon);
	// Go encodes an empty slice as null — normalize to [].
	const raw = v.sessions;
	if (raw !== null && !Array.isArray(raw)) throw new DecodeError('sessions');
	return { daemon, sessions: (raw ?? []).map(decodeSessionInfo) };
}
