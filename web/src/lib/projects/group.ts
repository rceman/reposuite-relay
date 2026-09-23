// Pure session→project organization. Grouping consumes the backend's
// derived SessionInfo.projectId projection — it NEVER reimplements cwd
// path matching in TypeScript. Svelte-free and unit-testable.
import type { ProjectInfo, SessionInfo } from '$lib/api/types';

/** One rendered section on /sessions: a project plus its sessions. */
export interface ProjectSection {
	project: ProjectInfo;
	sessions: SessionInfo[];
}

/** The full grouped projection: project sections then Ungrouped. */
export interface GroupedSessions {
	sections: ProjectSection[];
	ungrouped: SessionInfo[];
}

function byKey(a: SessionInfo, b: SessionInfo): number {
	return a.key < b.key ? -1 : a.key > b.key ? 1 : 0;
}

/**
 * groupSessions buckets sessions by the server-projected projectId.
 * Project sections follow the canonical ProjectList order; sessions
 * inside a section are stable-sorted by key; sessions whose projectId
 * is absent or references a project no longer in the catalog land in
 * Ungrouped — deterministic, last.
 */
export function groupSessions(
	projects: ProjectInfo[],
	sessions: SessionInfo[]
): GroupedSessions {
	const byId = new Map<string, SessionInfo[]>();
	for (const p of projects) byId.set(p.id, []);
	const ungrouped: SessionInfo[] = [];
	for (const s of [...sessions].sort(byKey)) {
		const bucket = s.projectId !== undefined ? byId.get(s.projectId) : undefined;
		if (bucket === undefined) ungrouped.push(s);
		else bucket.push(s);
	}
	return {
		sections: projects.map((project) => ({
			project,
			sessions: byId.get(project.id) ?? []
		})),
		ungrouped
	};
}

/**
 * projectRootFor resolves the cwd preset for the New Session project
 * selector: choosing a project prefills cwd with its root. Returns the
 * current cwd unchanged for an empty/unknown selection — the user's
 * hand-edited cwd is never forced back to a root.
 */
export function projectRootFor(
	projectId: string,
	projects: ProjectInfo[]
): string | undefined {
	return projects.find((p) => p.id === projectId)?.root;
}

/**
 * sessionCountByProject counts projected sessions per project ID — the
 * Settings page's per-project session column. Ungrouped sessions are
 * ignored.
 */
export function sessionCountByProject(sessions: SessionInfo[]): Map<string, number> {
	const counts = new Map<string, number>();
	for (const s of sessions) {
		if (s.projectId === undefined) continue;
		counts.set(s.projectId, (counts.get(s.projectId) ?? 0) + 1);
	}
	return counts;
}
