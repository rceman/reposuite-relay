// Pure form-contract helpers for the Settings project editor. The root
// is a filesystem path — it is sent EXACTLY as entered; whitespace is
// part of the path, so it is never trimmed or normalized in TypeScript.
// The name follows the backend's normalization contract (surrounding
// whitespace is canonicalized server-side, so the client sends it
// trimmed — it is display text, not a path).
import type { CreateProjectBody, ProjectInfo, UpdateProjectBody } from '$lib/api/types';
import { goTrimSpace } from '$lib/api/decode';

/**
 * UX-level absolute-path check only — the backend remains authoritative
 * for filepath.Clean and canonicalization. Uses the exact input: a
 * leading space makes this false rather than being silently trimmed.
 */
export function rootLooksAbsolute(root: string): boolean {
	return root.startsWith('/');
}

/** buildCreateBody sends the name Go-normalized and the root verbatim. */
export function buildCreateBody(name: string, root: string): CreateProjectBody {
	return { name: goTrimSpace(name), root };
}

/**
 * buildPatchBody diffs against the committed project: name is compared
 * after trim (its normal form), root is compared EXACTLY — "/work/x "
 * IS a different path than "/work/x". Returns null when nothing
 * changed, so the caller never sends an empty patch.
 */
export function buildPatchBody(
	prev: ProjectInfo,
	name: string,
	root: string
): UpdateProjectBody | null {
	const body: UpdateProjectBody = {};
	const n = goTrimSpace(name);
	if (n !== prev.name) body.name = n;
	if (root !== prev.root) body.root = root;
	if (body.name === undefined && body.root === undefined) return null;
	return body;
}
