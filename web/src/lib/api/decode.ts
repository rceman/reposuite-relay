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
	MachineTokenRotateResponse,
	MachineTokenStatusResponse,
	ProjectInfo,
	ProjectList,
	ProjectResponse,
	PromptResponse,
	RelayEvent,
	SessionInfo,
	SessionList,
	SessionMetrics,
	SessionResponse,
	RuntimeBinding,
	RuntimeInfo,
	RuntimeList,
	RuntimeResources,
	TelemetryHealth,
	TelemetrySnapshot,
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
	// The derived project projection is a pair: both valid or both
	// absent — a one-sided or malformed projection is DecodeError.
	const projectId = optString(v, 'projectId');
	const projectName = optString(v, 'projectName');
	if ((projectId === undefined) !== (projectName === undefined)) {
		throw new DecodeError('projectId/projectName');
	}
	if (projectId !== undefined && projectName !== undefined) {
		if (!isProjectId(projectId) || !isProjectName(projectName)) {
			throw new DecodeError('projectId/projectName');
		}
		out.projectId = projectId;
		out.projectName = projectName;
	}
	if (v.metrics !== undefined) out.metrics = decodeMetrics(v.metrics);
	return out;
}

// --- project wire contract -------------------------------------------------
// The decoder validates the canonical syntax only — it never mints,
// repairs, or normalizes malformed server output.

/** Canonical project ID: prj_ + 32 lowercase hex chars. */
const PROJECT_ID_RE = /^prj_[0-9a-f]{32}$/;

export function isProjectId(s: string): boolean {
	return PROJECT_ID_RE.test(s);
}

/** Unicode control characters (Cc) — mirrors Go's unicode.IsControl. */
const CONTROL_RE = /\p{Cc}/u;

/**
 * Whitespace at the string edges, using Unicode White_Space — the same
 * set Go's strings.TrimSpace strips (unicode.IsSpace == White_Space).
 * ECMAScript String.trim() is BROADER (it also strips U+FEFF), so it
 * must not be used for canonical-wire checks or name normalization.
 */
const GO_WS_EDGE_RE = /^[\p{White_Space}]+|[\p{White_Space}]+$/gu;

/** Mirror of Go strings.TrimSpace — edge White_Space only. */
export function goTrimSpace(s: string): string {
	return s.replace(GO_WS_EDGE_RE, '');
}

/**
 * Canonical project name: already whitespace-normalized (Go
 * strings.TrimSpace semantics — a leading U+FEFF is part of the name,
 * not whitespace), 1..128 UTF-8 bytes, no control characters. A name
 * that needs normalization is malformed server output — never repaired.
 */
export function isProjectName(s: string): boolean {
	if (s === '' || s !== goTrimSpace(s)) return false;
	if (new TextEncoder().encode(s).length > 128) return false;
	return !CONTROL_RE.test(s);
}

/**
 * Canonical project root: non-empty absolute path with no NUL — a
 * canonical backend root can never contain one. The browser does NOT
 * reimplement filepath.Clean; ordinary whitespace stays legal because
 * it may be part of a real path.
 */
export function isProjectRoot(s: string): boolean {
	return s !== '' && s.startsWith('/') && !s.includes('\u0000');
}

/** Machine token syntax: 64 lowercase hex — same shape auth.Valid enforces. */
const MACHINE_TOKEN_RE = /^[0-9a-f]{64}$/;

/** GET /v1/settings/machine-token — no secret fields exist to decode. */
export function decodeMachineTokenStatus(v: unknown): MachineTokenStatusResponse {
	if (!isRecord(v)) throw new DecodeError('machineTokenStatus');
	if (typeof v.configured !== 'boolean') {
		throw new DecodeError('machineTokenStatus.configured');
	}
	return {
		daemon: decodeDaemonInfo(v.daemon),
		configured: v.configured
	};
}

/**
 * POST rotate response — carries the new credential. Syntax is strictly
 * validated; a malformed credential field is a DecodeError, never a
 * blind cast.
 */
export function decodeMachineTokenRotate(v: unknown): MachineTokenRotateResponse {
	if (!isRecord(v)) throw new DecodeError('machineTokenRotate');
	const daemon = decodeDaemonInfo(v.daemon);
	if (typeof v.token !== 'string' || !MACHINE_TOKEN_RE.test(v.token)) {
		throw new DecodeError('machineTokenRotate.token');
	}
	if (typeof v.durabilityConfirmed !== 'boolean') {
		throw new DecodeError('machineTokenRotate.durabilityConfirmed');
	}
	return { daemon, token: v.token, durabilityConfirmed: v.durabilityConfirmed };
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
	const id = reqString(v, 'id');
	const name = reqString(v, 'name');
	const root = reqString(v, 'root');
	if (!isProjectId(id)) throw new DecodeError('project.id');
	if (!isProjectName(name)) throw new DecodeError('project.name');
	if (!isProjectRoot(root)) throw new DecodeError('project.root');
	return { id, name, root };
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

// --- /v1/runtimes decoders -----------------------------------------------

function optNonNeg(v: Record<string, unknown>, key: string): number | undefined {
	const n = optNumber(v, key);
	if (n !== undefined && (!Number.isSafeInteger(n) || n < 0)) throw new DecodeError(key);
	return n;
}

function reqNonNeg(v: Record<string, unknown>, key: string): number {
	const n = reqNumber(v, key);
	if (!Number.isSafeInteger(n) || n < 0) throw new DecodeError(key);
	return n;
}

function decodeRuntimeBinding(v: unknown): RuntimeBinding {
	if (!isRecord(v)) throw new DecodeError('binding');
	const activity = reqString(v, 'activity');
	if (!['idle', 'active', 'waiting_input'].includes(activity)) {
		throw new DecodeError('activity');
	}
	return {
		key: reqString(v, 'key'),
		sessionId: reqString(v, 'sessionId'),
		activity,
		mutating: reqBool(v, 'mutating')
	};
}

/** RFC3339/RFC3339Nano UTC timestamp — the only shape Go emits here. */
function reqTimestamp(v: Record<string, unknown>, key: string): string {
	const s = reqString(v, key);
	if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$/.test(s)) {
		throw new DecodeError(key);
	}
	if (Number.isNaN(Date.parse(s))) throw new DecodeError(key);
	return s;
}

function decodeRuntimeResources(v: unknown): RuntimeResources {
	if (!isRecord(v)) throw new DecodeError('resources');
	const available = reqBool(v, 'available');
	const pss = optNonNeg(v, 'pssBytes');
	const rss = optNonNeg(v, 'rssBytes');
	const count = optNonNeg(v, 'processCount');
	if (!available) {
		// Canonical unavailable shape carries no numbers at all — a
		// contradictory payload is rejected rather than silently read.
		if (pss !== undefined || rss !== undefined || count !== undefined) {
			throw new DecodeError('resources');
		}
		return { available: false };
	}
	// available=true requires the complete measurement; processCount is
	// the number of measured processes so it must be at least one.
	if (pss === undefined || rss === undefined || count === undefined || count < 1) {
		throw new DecodeError('resources');
	}
	return { available: true, pssBytes: pss, rssBytes: rss, processCount: count };
}

function decodeRuntimeInfo(v: unknown): RuntimeInfo {
	if (!isRecord(v)) throw new DecodeError('runtime');
	const raw = v.sessions;
	if (!Array.isArray(raw)) throw new DecodeError('sessions');
	return {
		runtimeId: reqString(v, 'runtimeId'),
		harness: reqString(v, 'harness'),
		shared: reqBool(v, 'shared'),
		pid: reqNonNeg(v, 'pid'),
		startedAt: reqTimestamp(v, 'startedAt'),
		uptimeSeconds: reqNonNeg(v, 'uptimeSeconds'),
		state: reqString(v, 'state'),
		sessionCount: reqNonNeg(v, 'sessionCount'),
		activeSessionCount: reqNonNeg(v, 'activeSessionCount'),
		waitingInputCount: reqNonNeg(v, 'waitingInputCount'),
		mutationCount: reqNonNeg(v, 'mutationCount'),
		sessions: raw.map(decodeRuntimeBinding),
		resources: decodeRuntimeResources(v.resources)
	};
}

export function decodeRuntimeList(v: unknown): RuntimeList {
	if (!isRecord(v)) throw new DecodeError('runtimes');
	const raw = v.runtimes;
	if (!Array.isArray(raw)) throw new DecodeError('runtimes');
	const t = v.totals;
	if (!isRecord(t)) throw new DecodeError('totals');
	const measuredRuntimeCount = reqNonNeg(t, 'measuredRuntimeCount');
	if (measuredRuntimeCount > reqNonNeg(t, 'runtimeCount')) {
		throw new DecodeError('totals.measuredRuntimeCount');
	}
	return {
		daemon: decodeDaemonInfo(v.daemon),
		sampledAt: reqTimestamp(v, 'sampledAt'),
		runtimes: raw.map(decodeRuntimeInfo),
		totals: {
			runtimeCount: reqNonNeg(t, 'runtimeCount'),
			sessionCount: reqNonNeg(t, 'sessionCount'),
			activeSessionCount: reqNonNeg(t, 'activeSessionCount'),
			waitingInputCount: reqNonNeg(t, 'waitingInputCount'),
			measuredRuntimeCount,
			pssBytes: reqNonNeg(t, 'pssBytes'),
			rssBytes: reqNonNeg(t, 'rssBytes')
		}
	};
}

/** decodeTelemetryHealth validates GET /v1/telemetry/repodex. */
export function decodeTelemetryHealth(v: unknown): TelemetryHealth {
	if (!isRecord(v)) throw new DecodeError('telemetry');
	const t = v.telemetry;
	if (!isRecord(t)) throw new DecodeError('telemetry');
	const state = t.state;
	if (typeof state !== 'string' ||
		!['disabled', 'disconnected', 'connected', 'degraded', 'incompatible'].includes(state)) {
		throw new DecodeError('telemetry.state');
	}
	const snap: TelemetrySnapshot = {
			enabled: t.enabled === true,
			state: state as TelemetrySnapshot['state'],

			queueDepth: reqNonNeg(t, 'queueDepth'),
			spoolBytes: reqNonNeg(t, 'spoolBytes'),
			observed: reqNonNeg(t, 'observed'),
			canonicalized: reqNonNeg(t, 'canonicalized'),
			acknowledged: reqNonNeg(t, 'acknowledged'),
			duplicates: reqNonNeg(t, 'duplicates'),
			rejected: reqNonNeg(t, 'rejected'),
			lost: reqNonNeg(t, 'lost'),
			retries: reqNonNeg(t, 'retries'),
			rediscoveries: reqNonNeg(t, 'rediscoveries'),
		};
		{ const _la = 0; }
		const instanceId = optString(t, 'instanceId');
		if (instanceId !== undefined) snap.instanceId = instanceId;
		const endpoint = optString(t, 'endpoint');
		if (endpoint !== undefined) snap.endpoint = endpoint;
		const lastAckAt = optString(t, 'lastAckAt');
		if (lastAckAt !== undefined) snap.lastAckAt = lastAckAt;
		return { daemon: decodeDaemonInfo(v.daemon), telemetry: snap };
}
