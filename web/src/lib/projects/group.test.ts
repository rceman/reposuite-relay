// Pure grouping projection tests — no Svelte, no filesystem. Membership
// consumes SessionInfo.projectId; TS never re-derives path matching.
import { describe, expect, it } from 'vitest';
import type { ProjectInfo, SessionInfo } from '$lib/api/types';
import { groupSessions, projectRootFor, sessionCountByProject } from './group';

function project(id: string, name: string, root: string): ProjectInfo {
	return { id, name, root };
}

function session(key: string, projectId?: string): SessionInfo {
	const s: SessionInfo = {
		key,
		sessionId: `ses_${key}`,
		runtimeId: '',
		runtimeState: 'cold',
		harness: 'codex',
		cwd: `/w/${key}`,
		state: 'idle',
		activity: 'idle',
		generation: 0,
		pid: 0,
		createdAt: '2026-01-01T00:00:00Z',
		generationStartedAt: ''
	};
	if (projectId !== undefined) {
		s.projectId = projectId;
		s.projectName = `name-for-${projectId}`;
	}
	return s;
}

describe('groupSessions', () => {
	it('buckets by projectId in canonical project order, ungrouped last', () => {
		const projects = [
			project('prj_b', 'Beta', '/w/b'),
			project('prj_a', 'Alpha', '/w/a')
		];
		const sessions = [
			session('s2', 'prj_a'),
			session('s1', 'prj_a'),
			session('s4', 'prj_b'),
			session('s3'), // ungrouped
			session('s9', 'prj_gone') // dangling projection → ungrouped
		];
		const g = groupSessions(projects, sessions);
		expect(g.sections.map((s) => s.project.id)).toEqual(['prj_b', 'prj_a']);
		expect(g.sections[0]?.sessions.map((s) => s.key)).toEqual(['s4']);
		expect(g.sections[1]?.sessions.map((s) => s.key)).toEqual(['s1', 's2']);
		expect(g.ungrouped.map((s) => s.key)).toEqual(['s3', 's9']);
	});

	it('keeps zero-session projects (the page decides whether to render)', () => {
		const g = groupSessions([project('prj_x', 'X', '/w/x')], []);
		expect(g.sections).toHaveLength(1);
		expect(g.sections[0]?.sessions).toHaveLength(0);
		expect(g.ungrouped).toHaveLength(0);
	});

	it('is deterministic regardless of input session order', () => {
		const projects = [project('prj_a', 'A', '/a')];
		const a = [session('z', 'prj_a'), session('a', 'prj_a'), session('m')];
		const b = [session('m'), session('a', 'prj_a'), session('z', 'prj_a')];
		expect(groupSessions(projects, a)).toEqual(groupSessions(projects, b));
	});

	it('separates duplicate display names by project ID', () => {
		const projects = [
			project('prj_1', 'same', '/w/one'),
			project('prj_2', 'same', '/w/two')
		];
		const g = groupSessions(projects, [
			session('s1', 'prj_1'),
			session('s2', 'prj_2')
		]);
		expect(g.sections[0]?.sessions[0]?.key).toBe('s1');
		expect(g.sections[1]?.sessions[0]?.key).toBe('s2');
	});
});

describe('projectRootFor', () => {
	it('returns the root for a known project and undefined otherwise', () => {
		const projects = [project('prj_a', 'A', '/w/a')];
		expect(projectRootFor('prj_a', projects)).toBe('/w/a');
		expect(projectRootFor('', projects)).toBeUndefined();
		expect(projectRootFor('prj_missing', projects)).toBeUndefined();
	});
});

describe('sessionCountByProject', () => {
	it('counts projected sessions per project, ignoring ungrouped', () => {
		const counts = sessionCountByProject([
			session('a', 'prj_1'),
			session('b', 'prj_1'),
			session('c', 'prj_2'),
			session('d')
		]);
		expect(counts.get('prj_1')).toBe(2);
		expect(counts.get('prj_2')).toBe(1);
		expect(counts.get('prj_3')).toBeUndefined();
	});
});
