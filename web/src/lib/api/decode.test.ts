import { describe, expect, it } from 'vitest';
import {
	DecodeError,
	decodeAuthSession,
	decodeCancelResponse,
	decodeInputResponse,
	decodeProjectInfo,
	decodeMachineTokenRotate,
	decodeMachineTokenStatus,
	decodeProjectList,
	decodeProjectResponse,
	decodePromptResponse,
	decodeRuntimeList,
	decodeRelayEvent,
	decodeSessionInfo,
	decodeSessionList,
	decodeSessionResponse,
	decodeTranscriptPage
} from './decode';

describe('decodeAuthSession', () => {
	it('decodes the unconfigured setup projection', () => {
		const s = decodeAuthSession({
			configured: false,
			authenticated: false,
			formToken: 'tok-setup'
		});
		expect(s.configured).toBe(false);
		expect(s.authenticated).toBe(false);
		expect(s.formToken).toBe('tok-setup');
		expect(s.username).toBeUndefined();
		expect(s.csrfToken).toBeUndefined();
	});

	it('decodes the configured logged-out projection', () => {
		const s = decodeAuthSession({
			configured: true,
			authenticated: false,
			formToken: 'tok-login'
		});
		expect(s.configured).toBe(true);
		expect(s.authenticated).toBe(false);
		expect(s.formToken).toBe('tok-login');
	});

	it('decodes the authenticated projection', () => {
		const s = decodeAuthSession({
			configured: true,
			authenticated: true,
			username: 'admin',
			csrfToken: 'csrf-1'
		});
		expect(s.authenticated).toBe(true);
		expect(s.username).toBe('admin');
		expect(s.csrfToken).toBe('csrf-1');
		expect(s.formToken).toBeUndefined();
	});

	it.each([null, 'x', 42, [], { configured: 'yes', authenticated: false }])(
		'rejects malformed payload %j',
		(v) => {
			expect(() => decodeAuthSession(v)).toThrow(DecodeError);
		}
	);

	it('rejects a non-string optional field instead of coercing', () => {
		expect(() =>
			decodeAuthSession({ configured: true, authenticated: false, formToken: 7 })
		).toThrow(DecodeError);
	});
});

describe('decodeSessionList', () => {
	const session = {
		key: 'demo',
		sessionId: 'a'.repeat(32),
		runtimeId: '',
		runtimeState: 'cold',
		harness: 'codex',
		cwd: '/home/x/proj',
		state: 'idle',
		activity: 'idle',
		generation: 0,
		pid: 0,
		createdAt: '2026-09-21T08:00:00Z',
		generationStartedAt: ''
	};

	it('decodes a full list', () => {
		const out = decodeSessionList({
			daemon: {
				instanceId: 'inst',
				pid: 1,
				apiVersion: 2,
				uptimeSeconds: 12.5,
				sessionCount: 1,
				activeSessions: 0,
				coldSessions: 1
			},
			sessions: [session]
		});
		expect(out.daemon.apiVersion).toBe(2);
		expect(out.sessions).toHaveLength(1);
		expect(out.sessions[0]?.key).toBe('demo');
		expect(out.sessions[0]?.model).toBeUndefined();
	});

	it('rejects a missing daemon block', () => {
		expect(() => decodeSessionList({ sessions: [] })).toThrow(DecodeError);
	});

	it('rejects sessions that are not an array', () => {
		expect(() =>
			decodeSessionList({
				daemon: {
					instanceId: 'i',
					pid: 1,
					apiVersion: 2,
					uptimeSeconds: 0,
					sessionCount: 0,
					activeSessions: 0,
					coldSessions: 0
				},
				sessions: 'nope'
			})
		).toThrow(DecodeError);
	});

	it('rejects a session missing a required field', () => {
		const { key: _dropped, ...rest } = session;
		expect(() =>
			decodeSessionList({
				daemon: {
					instanceId: 'i',
					pid: 1,
					apiVersion: 2,
					uptimeSeconds: 0,
					sessionCount: 1,
					activeSessions: 0,
					coldSessions: 1
				},
				sessions: [rest]
			})
		).toThrow(DecodeError);
	});
});

describe('decodeSessionResponse', () => {
	const daemon = {
		instanceId: 'i',
		pid: 1,
		apiVersion: 2,
		uptimeSeconds: 1,
		sessionCount: 1,
		activeSessions: 0,
		coldSessions: 1
	};
	const session = {
		key: 'k',
		sessionId: 's1',
		runtimeId: '',
		runtimeState: 'cold',
		harness: 'codex',
		cwd: '/x',
		state: 'idle',
		activity: 'idle',
		generation: 0,
		pid: 0,
		createdAt: '2026-01-01T00:00:00Z',
		generationStartedAt: ''
	};

	it('decodes a session response', () => {
		const r = decodeSessionResponse({ daemon, session });
		expect(r.session.key).toBe('k');
		expect(r.session.runtimeState).toBe('cold');
		expect(r.daemon.apiVersion).toBe(2);
	});

	it('rejects a missing session', () => {
		expect(() => decodeSessionResponse({ daemon })).toThrow(DecodeError);
	});
});

describe('decodePromptResponse / decodeCancelResponse / decodeInputResponse', () => {
	const daemon = {
		instanceId: 'i',
		pid: 1,
		apiVersion: 2,
		uptimeSeconds: 1,
		sessionCount: 0,
		activeSessions: 0,
		coldSessions: 0
	};

	it('decodes prompt acceptance', () => {
		const r = decodePromptResponse({
			daemon,
			key: 'k',
			turnId: 't1',
			nativeSessionId: 'thr_1',
			runtimeId: 'rt1'
		});
		expect(r.turnId).toBe('t1');
		expect(r.nativeSessionId).toBe('thr_1');
	});

	it('decodes cancel acceptance (turnId optional)', () => {
		const r = decodeCancelResponse({ daemon, key: 'k' });
		expect(r.key).toBe('k');
		expect(r.turnId).toBeUndefined();
	});

	it('decodes input acceptance', () => {
		const r = decodeInputResponse({ daemon, key: 'k', inputId: 'in_1' });
		expect(r.inputId).toBe('in_1');
	});

	it('rejects malformed required fields', () => {
		expect(() => decodePromptResponse({ daemon, key: 'k', turnId: 5 })).toThrow(DecodeError);
		expect(() => decodeInputResponse({ daemon, key: 'k' })).toThrow(DecodeError);
	});
});

describe('decodeTranscriptPage / decodeRelayEvent — safe-integer cursors', () => {
	const rec = {
		version: 1,
		seq: 3,
		type: 'message.user',
		at: '2026-01-01T00:00:00Z',
		payload: { text: 'hi' }
	};

	it('decodes a transcript page', () => {
		const p = decodeTranscriptPage({ throughSeq: 7, records: [rec], hasMoreBefore: false });
		expect(p.throughSeq).toBe(7);
		expect(p.records[0]?.seq).toBe(3);
		expect(p.hasMoreBefore).toBe(false);
	});

	it('normalizes a null records array (Go empty slice)', () => {
		const p = decodeTranscriptPage({ throughSeq: 0, records: null, hasMoreBefore: false });
		expect(p.records).toEqual([]);
	});

	it('accepts seq == MAX_SAFE_INTEGER', () => {
		const p = decodeTranscriptPage({
			throughSeq: Number.MAX_SAFE_INTEGER,
			records: [],
			hasMoreBefore: false
		});
		expect(p.throughSeq).toBe(Number.MAX_SAFE_INTEGER);
	});

	it('rejects seq beyond MAX_SAFE_INTEGER', () => {
		expect(() =>
			decodeTranscriptPage({
				throughSeq: Number.MAX_SAFE_INTEGER + 1,
				records: [],
				hasMoreBefore: false
			})
		).toThrow(DecodeError);
		expect(() =>
			decodeTranscriptPage({
				throughSeq: 1,
				records: [{ ...rec, seq: Number.MAX_SAFE_INTEGER + 1 }],
				hasMoreBefore: false
			})
		).toThrow(DecodeError);
	});

	it('rejects fractional and negative seq', () => {
		expect(() =>
			decodeTranscriptPage({ throughSeq: 1.5, records: [], hasMoreBefore: false })
		).toThrow(DecodeError);
		expect(() =>
			decodeTranscriptPage({ throughSeq: -1, records: [], hasMoreBefore: false })
		).toThrow(DecodeError);
	});

	it('decodes a relay event with payload', () => {
		const ev = decodeRelayEvent({
			seq: 9,
			sessionId: 's1',
			type: 'message.agent.delta',
			at: '2026-01-01T00:00:00Z',
			payload: { turnId: 't', text: 'x' },
			durable: false
		});
		expect(ev.seq).toBe(9);
		expect(ev.durable).toBe(false);
	});

	it('rejects an event with unsafe seq', () => {
		expect(() =>
			decodeRelayEvent({
				seq: 2 ** 53,
				sessionId: 's1',
				type: 'x',
				at: '2026-01-01T00:00:00Z',
				durable: false
			})
		).toThrow(DecodeError);
	});
});

describe('project decoders — strict wire contract', () => {
	const daemon = {
		instanceId: 'i',
		pid: 1,
		apiVersion: 2,
		uptimeSeconds: 1,
		sessionCount: 0,
		activeSessions: 0,
		coldSessions: 0
	};
	const project = { id: 'prj_' + 'a'.repeat(32), name: 'Relay', root: '/work/relay' };

	it('decodes a project list, normalizing null to empty', () => {
		const out = decodeProjectList({ daemon, projects: [project] });
		expect(out.projects).toEqual([project]);
		expect(decodeProjectList({ daemon, projects: null }).projects).toEqual([]);
	});

	it('decodes a project response', () => {
		const out = decodeProjectResponse({ daemon, project });
		expect(out.project.id).toBe(project.id);
	});

	it.each([
		['', 'empty id'],
		['prj_x', 'short suffix'],
		['project-1', 'wrong prefix'],
		['prj_' + 'A'.repeat(32), 'uppercase hex'],
		['prj_' + 'a'.repeat(31), '31 hex'],
		['prj_' + 'a'.repeat(33), '33 hex'],
		['prj_' + 'g'.repeat(32), 'non-hex']
	])('rejects malformed id %j (%s)', (id) => {
		expect(() =>
			decodeProjectInfo({ ...project, id })
		).toThrow(DecodeError);
	});

	it.each([
		['', 'empty'],
		['   ', 'whitespace only'],
		[' Relay', 'non-normalized leading space'],
		['Relay ', 'non-normalized trailing space'],
		['Rel\nay', 'control char'],
		['Rel\u0085ay', 'NEL control char'],
		['x'.repeat(129), 'over 128 bytes']
	])('rejects malformed name %j (%s)', (name) => {
		expect(() => decodeProjectInfo({ ...project, name })).toThrow(DecodeError);
	});

	it('accepts ordinary non-ASCII names', () => {
		const out = decodeProjectInfo({ ...project, name: 'プロジェクト Ω' });
		expect(out.name).toBe('プロジェクト Ω');
	});

	it('mirrors Go strings.TrimSpace, not ECMAScript trim()', () => {
		// U+00A0 NBSP is White_Space in both — a canonical name never has
		// it at the edges, so the wire value is malformed.
		expect(() =>
			decodeProjectInfo({ ...project, name: '\u00A0Relay\u00A0' })
		).toThrow(DecodeError);
		// U+FEFF is NOT White_Space in Go — the backend keeps it, so this
		// IS a canonical name (FEFF is Cf format, not a control char).
		// ECMAScript trim() would wrongly call it unnormalized.
		expect(decodeProjectInfo({ ...project, name: '\uFEFFRelay' }).name).toBe('\uFEFFRelay');
		expect(decodeProjectInfo({ ...project, name: 'Relay\uFEFF' }).name).toBe('Relay\uFEFF');
		// U+0085 NEL is White_Space (trimmed at edges) AND a control char —
		// interior it is rejected for the control reason, not whitespace.
		expect(() => decodeProjectInfo({ ...project, name: 'x\u0085y' })).toThrow(DecodeError);
	});

	it.each(['', 'relative/path', 42, null, '/work/a\u0000b'])(
		'rejects malformed root %j',
		(root) => {
			expect(() =>
				decodeProjectInfo({ ...project, root: root as unknown as string })
			).toThrow(DecodeError);
		}
	);

	it('accepts a root with a trailing space — it is a real path', () => {
		const out = decodeProjectInfo({ ...project, root: '/work/relay ' });
		expect(out.root).toBe('/work/relay ');
	});
});

describe('sessionInfo project projection pair', () => {
	const session = {
		key: 'k',
		sessionId: 's1',
		runtimeId: '',
		runtimeState: 'cold',
		harness: 'codex',
		cwd: '/x',
		state: 'idle',
		activity: 'idle',
		generation: 0,
		pid: 0,
		createdAt: '2026-01-01T00:00:00Z',
		generationStartedAt: ''
	};
	const goodId = 'prj_' + 'b'.repeat(32);

	it('accepts the pair absent', () => {
		const out = decodeSessionInfo(session);
		expect(out.projectId).toBeUndefined();
		expect(out.projectName).toBeUndefined();
	});

	it('accepts a canonical pair', () => {
		const out = decodeSessionInfo({ ...session, projectId: goodId, projectName: 'P' });
		expect(out.projectId).toBe(goodId);
		expect(out.projectName).toBe('P');
	});

	it('rejects a one-sided projection', () => {
		expect(() => decodeSessionInfo({ ...session, projectId: goodId })).toThrow(DecodeError);
		expect(() => decodeSessionInfo({ ...session, projectName: 'P' })).toThrow(DecodeError);
	});

	it('rejects a malformed project ID on the pair', () => {
		expect(() => decodeSessionInfo({ ...session, projectId: 'prj_x', projectName: 'P' })).toThrow(
			DecodeError
		);
	});

	it('rejects an empty or control-char project name', () => {
		expect(() =>
			decodeSessionInfo({ ...session, projectId: goodId, projectName: '' })
		).toThrow(DecodeError);
		expect(() =>
			decodeSessionInfo({ ...session, projectId: goodId, projectName: 'a\tb' })
		).toThrow(DecodeError);
	});
});

// --- machine-token DTOs -------------------------------------------------

describe('decodeMachineTokenStatus', () => {
	const status = { daemon: {
		instanceId: 'i',
		pid: 1,
		apiVersion: 2,
		uptimeSeconds: 1,
		sessionCount: 0,
		activeSessions: 0,
		coldSessions: 0
	}, configured: true };

	it('accepts a metadata-only status', () => {
		expect(decodeMachineTokenStatus(status).configured).toBe(true);
		expect(decodeMachineTokenStatus({ ...status, configured: false }).configured).toBe(false);
	});

	it.each([null, 'x', {}, { daemon: {
		instanceId: 'i',
		pid: 1,
		apiVersion: 2,
		uptimeSeconds: 1,
		sessionCount: 0,
		activeSessions: 0,
		coldSessions: 0
	} }])(
		'rejects malformed status %j',
		(v) => {
			expect(() => decodeMachineTokenStatus(v)).toThrow(DecodeError);
		}
	);
});

describe('decodeMachineTokenRotate', () => {
	const TOK = 'a'.repeat(64);
	const good = { daemon: {
		instanceId: 'i',
		pid: 1,
		apiVersion: 2,
		uptimeSeconds: 1,
		sessionCount: 0,
		activeSessions: 0,
		coldSessions: 0
	}, token: TOK, durabilityConfirmed: true };

	it('accepts a committed rotation', () => {
		const out = decodeMachineTokenRotate(good);
		expect(out.token).toBe(TOK);
		expect(out.durabilityConfirmed).toBe(true);
		expect(decodeMachineTokenRotate({ ...good, durabilityConfirmed: false })
			.durabilityConfirmed).toBe(false);
	});

	it.each([
		'token-prefix',          // too short
		'A'.repeat(64),          // uppercase rejected — lowercase hex only
		'g'.repeat(64),          // non-hex
		'a'.repeat(63),          // 63
		'a'.repeat(65),          // 65
		42,
		null
	])('rejects malformed token %j', (token) => {
		expect(() =>
			decodeMachineTokenRotate({ ...good, token: token as unknown as string })
		).toThrow(DecodeError);
	});

	it.each(['yes', 1, null, undefined])(
		'rejects malformed durabilityConfirmed %j',
		(durabilityConfirmed) => {
			expect(() =>
				decodeMachineTokenRotate({
					...good,
					durabilityConfirmed: durabilityConfirmed as unknown as boolean
				})
			).toThrow(DecodeError);
		}
	);
});

describe('decodeRuntimeList', () => {
	const goodRuntime = {
		runtimeId: 'a1b2c3',
		harness: 'codex',
		shared: true,
		pid: 1234,
		startedAt: '2026-01-01T00:00:00Z',
		uptimeSeconds: 42,
		state: 'warm',
		sessionCount: 1,
		activeSessionCount: 1,
		waitingInputCount: 0,
		mutationCount: 0,
		sessions: [
			{ key: 'api-refactor', sessionId: 's1', activity: 'active', mutating: false }
		],
		resources: { available: true, pssBytes: 1000, rssBytes: 2000, processCount: 2 }
	};
	const good = {
		daemon: {
			instanceId: 'x', pid: 1, apiVersion: 1, uptimeSeconds: 1,
			sessionCount: 1, activeSessions: 1, coldSessions: 0
		},
		sampledAt: '2026-01-01T00:00:00Z',
		runtimes: [goodRuntime],
		totals: {
			runtimeCount: 1, sessionCount: 1, activeSessionCount: 1,
			waitingInputCount: 0, measuredRuntimeCount: 1,
			pssBytes: 1000, rssBytes: 2000
		}
	};

	it('decodes a full inventory', () => {
		const out = decodeRuntimeList(good);
		expect(out.runtimes[0]?.harness).toBe('codex');
		expect(out.runtimes[0]?.resources.pssBytes).toBe(1000);
		expect(out.totals.measuredRuntimeCount).toBe(1);
	});

	it('accepts an empty inventory and unavailable resources', () => {
		const out = decodeRuntimeList({ ...good, runtimes: [
			{ ...goodRuntime, pid: 0, resources: { available: false } }
		] });
		expect(out.runtimes[0]?.resources.available).toBe(false);
		expect(out.runtimes[0]?.resources.pssBytes).toBeUndefined();
	});

	it.each([['runtimes', 'x'], ['totals', []], ['sampledAt', 5]])(
		'rejects malformed %s',
		(key, val) => {
			expect(() => decodeRuntimeList({ ...good, [key]: val })).toThrow(DecodeError);
		}
	);

	it.each([['pid', -1], ['sessionCount', 1.5], ['uptimeSeconds', -0.5]])(
		'rejects invalid %s',
		(key, val) => {
			expect(() =>
				decodeRuntimeList({ ...good, runtimes: [{ ...goodRuntime, [key]: val }] })
			).toThrow(DecodeError);
		}
	);

	it('rejects malformed activity and non-array sessions', () => {
		expect(() =>
			decodeRuntimeList({
				...good,
				runtimes: [{ ...goodRuntime, sessions: [{ ...goodRuntime.sessions[0], activity: 'bogus' }] }]
			})
		).toThrow(DecodeError);
		expect(() =>
			decodeRuntimeList({ ...good, runtimes: [{ ...goodRuntime, sessions: 'x' }] })
		).toThrow(DecodeError);
	});

	it('rejects negative resource bytes', () => {
		expect(() =>
			decodeRuntimeList({
				...good,
				runtimes: [
					{ ...goodRuntime, resources: { available: true, pssBytes: -1, rssBytes: 0, processCount: 1 } }
				]
			})
		).toThrow(DecodeError);
	});
});
