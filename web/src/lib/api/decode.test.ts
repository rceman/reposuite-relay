import { describe, expect, it } from 'vitest';
import { DecodeError, decodeAuthSession, decodeSessionList } from './decode';

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
