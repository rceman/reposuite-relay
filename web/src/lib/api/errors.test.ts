import { describe, expect, it } from 'vitest';
import { RelayError, errorFromResponse, transportError } from './errors';

function resp(status: number, body: string, headers: Record<string, string> = {}) {
	return new Response(body, { status, headers });
}

describe('errorFromResponse', () => {
	it('unwraps the canonical error envelope', async () => {
		const err = await errorFromResponse(
			resp(401, '{"error":{"code":"UNAUTHORIZED","message":"nope"}}')
		);
		expect(err).toBeInstanceOf(RelayError);
		expect(err.code).toBe('UNAUTHORIZED');
		expect(err.message).toBe('nope');
		expect(err.status).toBe(401);
	});

	it('captures Retry-After on throttled responses', async () => {
		const err = await errorFromResponse(
			resp(429, '{"error":{"code":"INVALID_REQUEST","message":"too many login attempts"}}', {
				'Retry-After': '31'
			})
		);
		expect(err.status).toBe(429);
		expect(err.retryAfter).toBe(31);
	});

	it('survives a non-JSON body', async () => {
		const err = await errorFromResponse(resp(500, 'oops'));
		expect(err.code).toBe('INTERNAL');
		expect(err.status).toBe(500);
		expect(err.retryAfter).toBeUndefined();
	});

	it('survives a JSON body without the envelope', async () => {
		const err = await errorFromResponse(resp(404, '{"x":1}'));
		expect(err.code).toBe('INTERNAL');
	});
});

describe('transportError', () => {
	it('normalizes network failures', () => {
		const err = transportError(new TypeError('fetch failed'));
		expect(err.code).toBe('TRANSPORT');
		expect(err.status).toBe(0);
	});

	it('passes through an existing RelayError', () => {
		const e = new RelayError('x', { code: 'Y', status: 500 });
		expect(transportError(e)).toBe(e);
	});
});
