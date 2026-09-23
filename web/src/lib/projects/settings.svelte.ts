// Settings → Projects reconciliation controller. The server's canonical
// reads are the ONLY authority for catalog state — a rejected mutation
// promise is never treated as a rollback, because the catalog commit
// point (atomic rename) may already have passed when the response was a
// post-commit 500. Mutation errors and load errors are SEPARATE state:
// reconciling canonical data must never erase the error that triggered
// the reconciliation.
import { RelayError } from '$lib/api/errors';
import type { ProjectInfo, ProjectList, SessionInfo, SessionList } from '$lib/api/types';

function describe(err: unknown, fallback: string): string {
	return err instanceof RelayError ? err.message : fallback;
}

/** The two canonical reads the catalog/UI reconcile against. */
export interface CanonicalReads {
	listProjects: () => Promise<ProjectList>;
	listSessions: () => Promise<SessionList>;
}

export class ProjectsSettings {
	/** Canonical catalog — written ONLY from a successful reload. */
	projects = $state<ProjectInfo[]>([]);
	/** Canonical session projection — same authority. */
	sessions = $state<SessionInfo[]>([]);
	/** Load/reconciliation failure — never masks a mutation error. */
	loadError = $state<string | null>(null);
	/** Mutation outcome — never cleared by a reload. */
	mutationError = $state<string | null>(null);

	constructor(private readonly reads: CanonicalReads) {}

	/**
	 * Canonical reload: replaces catalog + session projection on success.
	 * A reload failure records loadError and keeps the last-known state —
	 * it does NOT clear mutationError and does not invent catalog state.
	 */
	async load(): Promise<void> {
		try {
			const [pl, sl] = await Promise.all([
				this.reads.listProjects(),
				this.reads.listSessions()
			]);
			this.projects = pl.projects;
			this.sessions = sl.sessions;
			this.loadError = null;
		} catch (err) {
			this.loadError = describe(err, 'Failed to load settings');
		}
	}

	/**
	 * Run a mutation, then ALWAYS reconcile against canonical reads —
	 * success or error. A post-commit failure (rename committed, dirsync
	 * failed → 500) leaves the new catalog committed; the reload exposes
	 * it while mutationError stays visible. Returns true on clean success
	 * (callers may reset the form only then).
	 */
	async mutate(fn: () => Promise<unknown>): Promise<boolean> {
		let ok = true;
		try {
			await fn();
			this.mutationError = null;
		} catch (err) {
			ok = false;
			this.mutationError = describe(err, 'Project mutation failed');
		}
		await this.load();
		return ok;
	}
}
