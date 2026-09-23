// Runtime decoders for the Relay wire DTOs. TypeScript types describe the
// boundary but do not validate network input — every response is narrowed
// explicitly so a malformed payload becomes a controlled DecodeError,
// never a blind `as` cast.
import type {
	AuthSession,
	CancelResponse,
	DaemonInfo,
	DaemonResponse,
	ErrorBody,
	InputResponse,
	ProjectInfo,
	ProjectList,
	ProjectResponse,
	PromptResponse,
	RelayEvent,
	SessionInfo,
	SessionList,
	SessionMetrics,
	SessionResponse,
	TranscriptPage,
	TranscriptRecord
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

export function decodeSessionInfo(v: unknown): SessionInfo {
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
	// The derived project projection is a pair: both non-empty strings or
	// both absent — a one-sided projection is malformed.
	const projectId = optString(v, 'projectId');
	const projectName = optString(v, 'projectName');
	if ((projectId === undefined) !== (projectName === undefined)) {
		throw new DecodeError('projectId/projectName');
	}
	if (projectId !== undefined && projectName !== undefined) {
		if (projectId === '' || projectName === '') {
			throw new DecodeError('projectId/projectName');
		}
		out.projectId = projectId;
		out.projectName = projectName;
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

/**
 * Canonical sequence/cursor guard: uint64 on the wire must survive JSON
 * exactly. A value beyond Number.MAX_SAFE_INTEGER (or fractional/negative)
 * is protocol corruption — the response is rejected, never rounded.
 */
function reqSeq(v: Record<string, unknown>, key: string): number {
	const n = v[key];
	if (typeof n !== 'number' || !Number.isSafeInteger(n) || n < 0) {
		throw new DecodeError(key);
	}
	return n;
}

export function decodeSessionResponse(v: unknown): SessionResponse {
	if (!isRecord(v)) throw new DecodeError('sessionResponse');
	return { daemon: decodeDaemonInfo(v.daemon), session: decodeSessionInfo(v.session) };
}

export function decodeDaemonResponse(v: unknown): DaemonResponse {
	if (!isRecord(v)) throw new DecodeError('daemonResponse');
	return { daemon: decodeDaemonInfo(v.daemon) };
}

export function decodeProjectInfo(v: unknown): ProjectInfo {
	if (!isRecord(v)) throw new DecodeError('project');
	return {
		id: reqString(v, 'id'),
		name: reqString(v, 'name'),
		root: reqString(v, 'root')
	};
}

export function decodeProjectList(v: unknown): ProjectList {
	if (!isRecord(v)) throw new DecodeError('projectList');
	const raw = v.projects;
	if (raw !== null && !Array.isArray(raw)) throw new DecodeError('projects');
	return {
		daemon: decodeDaemonInfo(v.daemon),
		projects: (raw ?? []).map(decodeProjectInfo)
	};
}

export function decodeProjectResponse(v: unknown): ProjectResponse {
	if (!isRecord(v)) throw new DecodeError('projectResponse');
	return { daemon: decodeDaemonInfo(v.daemon), project: decodeProjectInfo(v.project) };
}

export function decodePromptResponse(v: unknown): PromptResponse {
	if (!isRecord(v)) throw new DecodeError('promptResponse');
	return {
		daemon: decodeDaemonInfo(v.daemon),
		key: reqString(v, 'key'),
		turnId: reqString(v, 'turnId'),
		nativeSessionId: reqString(v, 'nativeSessionId'),
		runtimeId: reqString(v, 'runtimeId')
	};
}

export function decodeCancelResponse(v: unknown): CancelResponse {
	if (!isRecord(v)) throw new DecodeError('cancelResponse');
	const out: CancelResponse = {
		daemon: decodeDaemonInfo(v.daemon),
		key: reqString(v, 'key')
	};
	const turnId = optString(v, 'turnId');
	if (turnId !== undefined) out.turnId = turnId;
	return out;
}

export function decodeInputResponse(v: unknown): InputResponse {
	if (!isRecord(v)) throw new DecodeError('inputResponse');
	return {
		daemon: decodeDaemonInfo(v.daemon),
		key: reqString(v, 'key'),
		inputId: reqString(v, 'inputId')
	};
}

function decodeRecord(v: unknown): TranscriptRecord {
	if (!isRecord(v)) throw new DecodeError('record');
	const out: TranscriptRecord = {
		version: reqNumber(v, 'version'),
		seq: reqSeq(v, 'seq'),
		type: reqString(v, 'type'),
		at: reqString(v, 'at')
	};
	// payload is unknown until an event-specific decoder narrows it.
	if (v.payload !== undefined) out.payload = v.payload;
	return out;
}

export function decodeTranscriptPage(v: unknown): TranscriptPage {
	if (!isRecord(v)) throw new DecodeError('transcriptPage');
	const raw = v.records;
	if (raw !== null && !Array.isArray(raw)) throw new DecodeError('records');
	return {
		throughSeq: reqSeq(v, 'throughSeq'),
		records: (raw ?? []).map(decodeRecord),
		hasMoreBefore: reqBool(v, 'hasMoreBefore')
	};
}

export function decodeRelayEvent(v: unknown): RelayEvent {
	if (!isRecord(v)) throw new DecodeError('event');
	const out: RelayEvent = {
		seq: reqSeq(v, 'seq'),
		sessionId: reqString(v, 'sessionId'),
		type: reqString(v, 'type'),
		at: reqString(v, 'at'),
		durable: reqBool(v, 'durable')
	};
	if (v.payload !== undefined) out.payload = v.payload;
	return out;
}
