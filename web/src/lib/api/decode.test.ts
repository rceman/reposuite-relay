import { describe, expect, it } from 'vitest';
import {
	DecodeError,
	decodeAuthSession,
	decodeCancelResponse,
	decodeInputResponse,
	decodeProjectList,
	decodeProjectResponse,
	decodePromptResponse,
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

describe('project decoders', () => {
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

	it('rejects a malformed project entry', () => {
		expect(() =>
			decodeProjectList({ daemon, projects: [{ id: 1, name: 'x', root: '/r' }] })
		).toThrow(DecodeError);
	});

	it('decodes a project response', () => {
		const out = decodeProjectResponse({ daemon, project });
		expect(out.project.id).toBe(project.id);
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

	it('accepts the pair absent', () => {
		const out = decodeSessionInfo(session);
		expect(out.projectId).toBeUndefined();
		expect(out.projectName).toBeUndefined();
	});

	it('accepts the pair present', () => {
		const out = decodeSessionInfo({ ...session, projectId: 'prj_x', projectName: 'P' });
		expect(out.projectId).toBe('prj_x');
		expect(out.projectName).toBe('P');
	});

	it('rejects a one-sided projection', () => {
		expect(() => decodeSessionInfo({ ...session, projectId: 'prj_x' })).toThrow(DecodeError);
		expect(() => decodeSessionInfo({ ...session, projectName: 'P' })).toThrow(DecodeError);
	});

	it('rejects an empty member of the pair', () => {
		expect(() =>
			decodeSessionInfo({ ...session, projectId: '', projectName: 'P' })
		).toThrow(DecodeError);
	});
});
