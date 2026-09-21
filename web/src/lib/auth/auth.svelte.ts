// Browser auth state for the Web Admin SPA. Everything here is
// memory-only: the session cookie is HttpOnly and invisible to JS, and
// the csrfToken/username/formToken we hold die with the page lifetime.
// Nothing is written to localStorage, sessionStorage, or IndexedDB.
import { decodeAuthSession, DecodeError } from '$lib/api/decode';
import { configureApi } from '$lib/api/client';
import { errorFromResponse, transportError } from '$lib/api/errors';
import type { AuthSession } from '$lib/api/types';

/** POST a urlencoded form to an /auth/* endpoint; 2xx/303 = success. */
async function postForm(path: string, fields: Record<string, string>): Promise<void> {
	let resp: Response;
	try {
		resp = await fetch(path, {
			method: 'POST',
			headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
			body: new URLSearchParams(fields)
		});
	} catch (err) {
		throw transportError(err);
	}
	if (!resp.ok) throw await errorFromResponse(resp);
}

class AuthStore {
	/** The last decoded /auth/session projection; null until bootstrap. */
	state = $state<AuthSession | null>(null);
	/** True until the first /auth/session round-trip settles. */
	loading = $state(true);
	/** A bootstrap failure — daemon unreachable or malformed response. */
	bootError = $state<string | null>(null);

	async refresh(): Promise<void> {
		try {
			const resp = await fetch('/auth/session', {
				headers: { Accept: 'application/json' }
			});
			if (!resp.ok) throw await errorFromResponse(resp);
			const body: unknown = await resp.json();
			this.state = decodeAuthSession(body);
			this.bootError = null;
		} catch (err) {
			this.bootError = err instanceof Error ? err.message : 'request failed';
		} finally {
			this.loading = false;
		}
	}

	get authenticated(): boolean {
		return this.state?.authenticated ?? false;
	}

	/** Session CSRF token for unsafe /v1 calls — memory only. */
	get csrfToken(): string | undefined {
		return this.state?.authenticated ? this.state.csrfToken : undefined;
	}

	async setup(username: string, password: string, confirm: string): Promise<void> {
		const formToken = this.state?.formToken;
		if (this.state === null || this.state.configured || formToken === undefined) {
			throw new DecodeError('not in setup mode');
		}
		await postForm('/auth/setup', {
			form_token: formToken,
			username,
			password,
			confirm
		});
		await this.refresh();
	}

	async login(username: string, password: string): Promise<void> {
		const formToken = this.state?.formToken;
		if (!this.state?.configured || this.state.authenticated || formToken === undefined) {
			throw new DecodeError('not in login mode');
		}
		await postForm('/auth/login', { form_token: formToken, username, password });
		await this.refresh();
	}

	async logout(): Promise<void> {
		const csrf = this.state?.csrfToken;
		if (!this.state?.authenticated || csrf === undefined) return;
		try {
			await postForm('/auth/logout', { csrf });
		} finally {
			this.state = null;
			await this.refresh();
		}
	}
}

export const auth = new AuthStore();

configureApi({
	getCsrf: () => auth.csrfToken,
	onUnauthorized: () => void auth.refresh()
});
