// Settings reconciliation + form-contract tests. The key invariant: a
// rejected mutation promise is NOT a rollback — the canonical reload
// always runs after the mutation settles, and the mutation error stays
// separately visible.
import { describe, expect, it } from 'vitest';
import type { ProjectInfo, ProjectList, SessionList } from '$lib/api/types';
import { buildCreateBody, buildPatchBody, rootLooksAbsolute } from './form';
import { ProjectsSettings, type CanonicalReads } from './settings.svelte';
import { RelayError } from '$lib/api/errors';

function daemon() {
	return {
		instanceId: 'i',
		pid: 1,
		apiVersion: 2,
		uptimeSeconds: 0,
		sessionCount: 0,
		activeSessions: 0,
		coldSessions: 0
	};
}

function project(id: string, name = 'P', root = '/w'): ProjectInfo {
	return { id, name, root };
}

function reads(projects: ProjectInfo[]): CanonicalReads {
	return {
		listProjects: async (): Promise<ProjectList> => ({ daemon: daemon(), projects }),
		listSessions: async (): Promise<SessionList> => ({ daemon: daemon(), sessions: [] })
	};
}

function postCommitErr(): RelayError {
	return new RelayError(
		'presentation config committed; post-commit dirsync failed: injected',
		{ code: 'INTERNAL', status: 500 }
	);
}

describe('ProjectsSettings reconciliation', () => {
	it('reloads canonical state after a clean mutation', async () => {
		const ctl = new ProjectsSettings(reads([project('prj_1')]));
		const ok = await ctl.mutate(async () => {});
		expect(ok).toBe(true);
		expect(ctl.projects.map((p) => p.id)).toEqual(['prj_1']);
		expect(ctl.mutationError).toBeNull();
		expect(ctl.loadError).toBeNull();
	});

	it('post-commit create: reload exposes committed catalog, error stays', async () => {
		const committed = project('prj_new', 'Committed', '/w/new');
		const ctl = new ProjectsSettings(reads([committed]));
		const ok = await ctl.mutate(async () => {
			throw postCommitErr();
		});
		expect(ok).toBe(false);
		// Canonical reload ran and replaced the stale empty list.
		expect(ctl.projects.map((p) => p.id)).toEqual(['prj_new']);
		// The post-commit error remains visible — the UI does not pretend
		// the save cleanly failed.
		expect(ctl.mutationError).toContain('post-commit');
		expect(ctl.loadError).toBeNull();
	});

	it('post-commit delete: project disappears from reconciled state', async () => {
		const gone = project('prj_del', 'Gone', '/w/gone');
		const ctl = new ProjectsSettings(reads([]));
		ctl.projects = [gone];
		const ok = await ctl.mutate(async () => {
			throw postCommitErr();
		});
		expect(ok).toBe(false);
		// The committed deletion is reflected — the project is NOT
		// locally restored just because HTTP returned 500.
		expect(ctl.projects).toEqual([]);
		expect(ctl.mutationError).toContain('post-commit');
	});

	it('mutation error survives a canonical reload (separate channels)', async () => {
		const ctl = new ProjectsSettings(reads([]));
		await ctl.mutate(async () => {
			throw new RelayError('bad name', { code: 'INVALID_REQUEST', status: 400 });
		});
		expect(ctl.mutationError).toBe('bad name');
		await ctl.load();
		expect(ctl.mutationError).toBe('bad name'); // not erased by reload
		expect(ctl.loadError).toBeNull();
	});

	it('load failure during reconciliation preserves both errors', async () => {
		const failing: CanonicalReads = {
			listProjects: async () => {
				throw new RelayError('expired', { code: 'UNAUTHORIZED', status: 401 });
			},
			listSessions: async () => ({ daemon: daemon(), sessions: [] })
		};
		const ctl = new ProjectsSettings(failing);
		ctl.projects = [project('prj_stale')];
		const ok = await ctl.mutate(async () => {
			throw postCommitErr();
		});
		expect(ok).toBe(false);
		expect(ctl.mutationError).toContain('post-commit');
		expect(ctl.loadError).toBe('expired');
		// No fabricated authority: last-known state is kept, errors say so.
		expect(ctl.projects.map((p) => p.id)).toEqual(['prj_stale']);
	});
});

describe('project form contract — exact root', () => {
	it('create sends the root verbatim, including a trailing space', () => {
		expect(buildCreateBody(' P ', '/work/project ')).toEqual({
			name: 'P',
			root: '/work/project '
		});
	});

	it('leading-space root fails the absolute-path UX check, not silently trimmed', () => {
		expect(rootLooksAbsolute(' /work/project')).toBe(false);
		expect(rootLooksAbsolute('/work/project')).toBe(true);
		expect(rootLooksAbsolute('')).toBe(false);
	});

	it('patch treats a trailing-space root as a real change, sent verbatim', () => {
		const prev = project('prj_1', 'P', '/work/project');
		expect(buildPatchBody(prev, 'P', '/work/project ')).toEqual({
			root: '/work/project '
		});
	});

	it('patch diffs only changed fields and rejects the empty patch', () => {
		const prev = project('prj_1', 'P', '/w');
		expect(buildPatchBody(prev, 'P', '/w')).toBeNull();
		expect(buildPatchBody(prev, 'New', '/w')).toEqual({ name: 'New' });
	});
});
