// Package protocol defines the bounded, versioned JSON request/response
// control protocol spoken on the RepoSuite Relay daemon's Unix socket.
//
// One connection = one request = one response = close. No multiplexing, no
// streaming, no persistent subscriptions.
package protocol

// Version is the wire protocol version. It is bumped only on incompatible
// changes; mismatched peers get PROTOCOL_MISMATCH.
const Version = 1

// MaxRequest bounds a client request in bytes. The daemon never allocates
// beyond this based on declared lengths.
const MaxRequest = 64 << 10

// Operations.
const (
	OpPing          = "ping"
	OpDaemonStatus  = "daemon_status"
	OpServeFixture  = "serve_fixture"
	OpListSessions  = "list_sessions"
	OpSessionStatus = "session_status"
	OpStopSession   = "stop_session"
	OpShutdown      = "shutdown"
)

// Stable machine-oriented error codes.
const (
	ErrInvalidRequest    = "INVALID_REQUEST"
	ErrProtocolMismatch  = "PROTOCOL_MISMATCH"
	ErrInvalidSessionKey = "INVALID_SESSION_KEY"
	ErrSessionExists     = "SESSION_EXISTS"
	ErrSessionNotFound   = "SESSION_NOT_FOUND"
	ErrFixtureStart      = "FIXTURE_START_FAILED"
	ErrShuttingDown      = "DAEMON_SHUTTING_DOWN"
	ErrInternal          = "INTERNAL"
)

// Request is the only wire type a client may send. It deliberately has no
// command/executable/args fields: the daemon never executes client-supplied
// commands.
type Request struct {
	Version int    `json:"v"`
	Op      string `json:"op"`
	// Key is the logical session key for session-scoped ops.
	Key string `json:"key,omitempty"`
	// Cwd is the client working directory, captured for serve_fixture.
	Cwd string `json:"cwd,omitempty"`
}

// DaemonInfo identifies a live daemon.
type DaemonInfo struct {
	PID             int     `json:"pid"`
	ProtocolVersion int     `json:"protocolVersion"`
	UptimeSeconds   float64 `json:"uptimeSeconds"`
	SessionCount    int     `json:"sessionCount"`
}

// SessionInfo is the wire view of a durable RelaySession plus its
// optional live HarnessRuntime. Process handles are never exposed. For a
// COLD restored session runtimeId is "", pid 0, generationStartedAt "",
// and runtimeState "cold".
type SessionInfo struct {
	Key                 string `json:"key"`
	SessionID           string `json:"sessionId"`
	RuntimeID           string `json:"runtimeId"`
	RuntimeState        string `json:"runtimeState"`
	NativeSessionID     string `json:"nativeSessionId,omitempty"`
	Harness             string `json:"harness"`
	Cwd                 string `json:"cwd"`
	State               string `json:"state"`
	Generation          int    `json:"generation"`
	PID                 int    `json:"pid"`
	CreatedAt           string `json:"createdAt"`
	GenerationStartedAt string `json:"generationStartedAt"`
}

// Response is the only wire type the daemon sends.
type Response struct {
	Version  int           `json:"v"`
	OK       bool          `json:"ok"`
	Code     string        `json:"code,omitempty"`
	Error    string        `json:"error,omitempty"`
	Daemon   *DaemonInfo   `json:"daemon,omitempty"`
	Session  *SessionInfo  `json:"session,omitempty"`
	Sessions []SessionInfo `json:"sessions,omitempty"`
}

// Ok builds a success response carrying daemon identity.
func Ok(d *DaemonInfo) Response {
	return Response{Version: Version, OK: true, Daemon: d}
}

// Fail builds a structured error response.
func Fail(code, msg string) Response {
	return Response{Version: Version, OK: false, Code: code, Error: msg}
}
