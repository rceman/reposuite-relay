// Pure requested-input answer construction — extracted from InputCard so
// the exact-preservation contract is unit-testable without a DOM.
//
// Free-text answers are forwarded EXACTLY as entered: no trim, no
// normalization. A secret (isSecret) may legally contain leading or
// trailing whitespace — a token the browser silently rewrote would be
// the wrong credential. Presence is rawValue.length > 0, never a
// whitespace-insensitive check.
import type { InputAnswer, InputQuestion } from '$lib/api/types';

/** Per-question draft state: checked option labels + free-text input. */
export interface AnswerDraft {
	selected: Record<string, string[]>;
	freeText: Record<string, string>;
}

/** True when at least one question has a selection or entered text. */
export function hasAnyAnswer(questions: InputQuestion[], draft: AnswerDraft): boolean {
	return questions.some(
		(q) => (draft.selected[q.id]?.length ?? 0) > 0 || (draft.freeText[q.id]?.length ?? 0) > 0
	);
}

/**
 * buildAnswers maps the draft to the wire shape: selected option labels
 * first, then the free-text value verbatim (including whitespace) when
 * the question offers one (isOther). Questions with no answer are
 * omitted — the daemon validates membership and non-emptiness.
 */
export function buildAnswers(questions: InputQuestion[], draft: AnswerDraft): InputAnswer[] {
	return questions
		.map((q) => {
			const out = [...(draft.selected[q.id] ?? [])];
			const raw = draft.freeText[q.id];
			if (raw !== undefined && raw.length > 0) out.push(raw);
			return { questionId: q.id, answers: out };
		})
		.filter((a) => a.answers.length > 0);
}
