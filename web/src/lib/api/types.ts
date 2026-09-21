// Wire DTOs for the Relay local API — field names mirror the Go JSON
// contract in internal/api exactly. These types describe the boundary;
// decode.ts is what actually trusts the network.

/** GET /auth/session — the anonymous SPA bootstrap projection. */
export interface AuthSession {
	configured: boolean;
	authenticated: boolean;
	/** Present only when authenticated. */
	username?: string;
	/** Present only when authenticated; unsafe /v1 calls need it. */
	csrfToken?: string;
	/** Setup token when unconfigured, login token when logged out. */
	formToken?: string;
}

/** The single error envelope for every failed request. */
export interface ErrorBody {
	error: { code: string; message: string };
}

/** GET /v1/daemon payload — a live daemon generation. */
export interface DaemonInfo {
	instanceId: string;
	pid: number;
	apiVersion: number;
	uptimeSeconds: number;
	sessionCount: number;
	activeSessions: number;
	coldSessions: number;
}

/** Last-known in-memory harness accounting; never persisted. */
export interface SessionMetrics {
	model?: string;
	mode?: string;
	contextUsed?: number;
	contextLimit?: number;
	inputTokens?: number;
	outputTokens?: number;
	reasoningTokens?: number;
	cacheTokens?: number;
	quotaUsed?: number;
	quotaLimit?: number;
	resetAt?: string;
}

/** One durable RelaySession plus its optional live runtime. */
export interface SessionInfo {
	key: string;
	sessionId: string;
	runtimeId: string;
	runtimeState: string;
	nativeSessionId?: string;
	harness: string;
	cwd: string;
	state: string;
	activity: string;
	model?: string;
	mode?: string;
	generation: number;
	pid: number;
	createdAt: string;
	generationStartedAt: string;
	metrics?: SessionMetrics;
}

/** GET /v1/sessions payload. */
export interface SessionList {
	daemon: DaemonInfo;
	sessions: SessionInfo[];
}
