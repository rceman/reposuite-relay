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
	/**
	 * Derived presentation projection — computed at read time from the
	 * local project catalog and the session cwd. Either both are present
	 * or neither; never persisted on the session.
	 */
	projectId?: string;
	projectName?: string;
}

/** One Relay-local project — grouping metadata only. */
export interface ProjectInfo {
	id: string;
	name: string;
	root: string;
}

/** GET /v1/projects — the catalog in canonical order (name, root, id). */
export interface ProjectList {
	daemon: DaemonInfo;
	projects: ProjectInfo[];
}

/** POST/PATCH project responses. */
export interface ProjectResponse {
	daemon: DaemonInfo;
	project: ProjectInfo;
}

/** POST /v1/projects body. */
export interface CreateProjectBody {
	name: string;
	root: string;
}

/** PATCH /v1/projects/{id} body — changed fields only. */
export interface UpdateProjectBody {
	name?: string;
	root?: string;
}

/**
 * GET /v1/settings/machine-token — presence metadata only. The
 * persistent machine credential is never readable through the API.
 */
export interface MachineTokenStatusResponse {
	daemon: DaemonInfo;
	configured: boolean;
}

/**
 * POST /v1/settings/machine-token/rotate — the only channel that
 * returns a machine credential: the freshly generated one.
 * durabilityConfirmed=false means the rename committed but the
 * directory fsync failed — the token IS active; durability is
 * unconfirmed.
 */
export interface MachineTokenRotateResponse {
	daemon: DaemonInfo;
	token: string;
	durabilityConfirmed: boolean;
}

/** GET /v1/sessions payload. */
export interface SessionList {
	daemon: DaemonInfo;
	sessions: SessionInfo[];
}

/** GET /v1/sessions/{key} and mutating single-session responses. */
export interface SessionResponse {
	daemon: DaemonInfo;
	session: SessionInfo;
}

/** GET /v1/daemon, DELETE /v1/sessions/{key}, POST /v1/daemon/shutdown. */
export interface DaemonResponse {
	daemon: DaemonInfo;
}

/** POST /v1/sessions/{key}/prompt — the accepted native turn. */
export interface PromptResponse {
	daemon: DaemonInfo;
	key: string;
	turnId: string;
	nativeSessionId: string;
	runtimeId: string;
}

/** POST /v1/sessions/{key}/cancel — cancellation request accepted. */
export interface CancelResponse {
	daemon: DaemonInfo;
	key: string;
	turnId?: string;
}

/** POST /v1/sessions/{key}/input — the accepted requested-input answer. */
export interface InputResponse {
	daemon: DaemonInfo;
	key: string;
	inputId: string;
}

/** One durable transcript record (internal/store.Record). */
export interface TranscriptRecord {
	version: number;
	seq: number;
	type: string;
	at: string;
	payload?: unknown;
}

/**
 * GET /v1/sessions/{key}/transcript — a bounded durable tail. throughSeq
 * is the canonical event cursor at snapshot time (NOT the last record
 * seq): subscribe to events after exactly this value.
 */
export interface TranscriptPage {
	throughSeq: number;
	records: TranscriptRecord[];
	hasMoreBefore: boolean;
}

/** One canonical event on the NDJSON stream (internal/events.Event). */
export interface RelayEvent {
	seq: number;
	sessionId: string;
	type: string;
	at: string;
	payload?: unknown;
	durable: boolean;
}

/** POST /v1/sessions/{provider} body. */
export interface CreateSessionBody {
	key: string;
	cwd: string;
	model?: string;
	mode?: string;
}

/** PATCH /v1/sessions/{key}/config body — only changed fields are sent. */
export interface ConfigBody {
	model?: string;
	mode?: string;
}

/** One answer to one requested-input question. */
export interface InputAnswer {
	questionId: string;
	answers: string[];
}

/** POST /v1/sessions/{key}/input body. */
export interface InputBody {
	inputId: string;
	answers: InputAnswer[];
}

// --- canonical event payloads -------------------------------------------
// These mirror internal/api/events.go and the Codex adapter's input
// payloads. Payloads on the wire are `unknown` and narrowed through
// decode.ts — never cast.

export interface MessageUserPayload {
	text: string;
	model?: string;
	effort?: string;
}

/** message.agent.completed AND message.agent.delta share this shape. */
export interface MessageAgentPayload {
	turnId: string;
	itemId?: string;
	text?: string;
}

export interface TurnEventPayload {
	turnId?: string;
	error?: string;
}

export interface HarnessStartedPayload {
	runtimeId: string;
	nativeSessionId: string;
	model?: string;
	resumed: boolean;
}

export interface RuntimeExitedPayload {
	runtimeId: string;
	reason?: string;
}

export interface ConfigChangedPayload {
	model: string;
	mode: string;
}

export interface NativeSessionPayload {
	nativeSessionId: string;
	generation: number;
}

export interface HarnessErrorPayload {
	message: string;
	fatal?: boolean;
}

/** metrics.updated embeds SessionMetrics flat on the wire (Go embed). */
export interface MetricsUpdatedPayload extends SessionMetrics {
	kind: string;
}

export interface InputOption {
	label: string;
	description?: string;
}

export interface InputQuestion {
	id: string;
	header: string;
	question: string;
	options?: InputOption[];
	isOther?: boolean;
	isSecret?: boolean;
}

export interface InputRequestedPayload {
	inputId: string;
	turnId: string;
	itemId: string;
	isBlocking: boolean;
	questions: InputQuestion[];
}

export interface InputResolvedPayload {
	inputId: string;
	answers: Record<string, string[]>;
	redacted?: string[];
}

export interface InputAbortedPayload {
	inputId: string;
	reason: string;
}

// --- /v1/runtimes — global live harness-runtime inventory ---------------
// Observer-only endpoint: reading it never wakes a COLD session or
// spawns a harness. Activity is Relay-observed state only — it says
// nothing about external child processes or background work.

/** One RelaySession bound to a runtime generation. */
export interface RuntimeBinding {
	key: string;
	sessionId: string;
	/** idle | active | waiting_input */
	activity: string;
	mutating: boolean;
}

/** Best-effort process-tree measurement; available=false is not zero. */
export interface RuntimeResources {
	available: boolean;
	pssBytes?: number;
	rssBytes?: number;
	processCount?: number;
}

export interface RuntimeInfo {
	runtimeId: string;
	harness: string;
	shared: boolean;
	pid: number;
	startedAt: string;
	uptimeSeconds: number;
	state: string;
	sessionCount: number;
	activeSessionCount: number;
	waitingInputCount: number;
	mutationCount: number;
	sessions: RuntimeBinding[];
	resources: RuntimeResources;
}

/**
 * measuredRuntimeCount < runtimeCount means pssBytes/rssBytes cover only
 * measured runtimes — never present the sum as the full inventory.
 */
export interface RuntimeTotals {
	runtimeCount: number;
	sessionCount: number;
	activeSessionCount: number;
	waitingInputCount: number;
	measuredRuntimeCount: number;
	pssBytes: number;
	rssBytes: number;
}

/** GET /v1/runtimes payload. */
export interface RuntimeList {
	daemon: DaemonInfo;
	sampledAt: string;
	runtimes: RuntimeInfo[];
	totals: RuntimeTotals;
}
