// Answer construction: free text is forwarded verbatim — never trimmed —
// because a secret may legally contain surrounding whitespace. Presence
// is rawValue.length > 0; questions with no answer are omitted.
import { describe, expect, it } from 'vitest';
import { buildAnswers, hasAnyAnswer, planSubmit } from './answers';
import type { InputQuestion } from '$lib/api/types';

const QUESTIONS: InputQuestion[] = [
	{
		id: 'q1',
		header: 'Choose',
		question: 'Pick one',
		options: [{ label: 'alpha' }, { label: 'beta' }]
	},
	{ id: 'q2', header: 'Credential', question: 'Enter the token', isOther: true, isSecret: true },
	{ id: 'q3', header: 'Note', question: 'Anything else?', isOther: true }
];

describe('buildAnswers', () => {
	it('forwards a secret free-text answer exactly as entered', () => {
		const answers = buildAnswers(QUESTIONS, {
			selected: {},
			freeText: { q2: '  secret token  ' }
		});
		expect(answers).toEqual([{ questionId: 'q2', answers: ['  secret token  '] }]);
	});

	it('forwards a non-secret free-text answer exactly as entered', () => {
		const answers = buildAnswers(QUESTIONS, {
			selected: {},
			freeText: { q3: '  spaced note  ' }
		});
		expect(answers).toEqual([{ questionId: 'q3', answers: ['  spaced note  '] }]);
	});

	it('combines selected options with verbatim free text', () => {
		const answers = buildAnswers(QUESTIONS, {
			selected: { q1: ['alpha', 'beta'] },
			freeText: { q2: 'tok' }
		});
		expect(answers).toEqual([
			{ questionId: 'q1', answers: ['alpha', 'beta'] },
			{ questionId: 'q2', answers: ['tok'] }
		]);
	});

	it('omits questions with no answer', () => {
		expect(buildAnswers(QUESTIONS, { selected: {}, freeText: {} })).toEqual([]);
		expect(
			buildAnswers(QUESTIONS, { selected: { q1: ['alpha'] }, freeText: {} })
		).toEqual([{ questionId: 'q1', answers: ['alpha'] }]);
	});

	it('treats an empty string as unanswered but whitespace as an answer', () => {
		// Presence is rawValue.length > 0 — never a trim-based check. A
		// whitespace-only secret is still the exact value the user entered.
		expect(
			buildAnswers(QUESTIONS, { selected: {}, freeText: { q2: '' } })
		).toEqual([]);
		expect(
			buildAnswers(QUESTIONS, { selected: {}, freeText: { q2: '   ' } })
		).toEqual([{ questionId: 'q2', answers: ['   '] }]);
	});
});

describe('planSubmit', () => {
	it('carries the exact secret in the body but strips it from the retained draft', () => {
		// The durable-commit-failure path returns an error AFTER the native
		// side may already hold the answer — so the secret leaves component
		// memory before the send is awaited, not after it succeeds. The
		// cleared draft is what the component keeps while awaiting and
		// after a failure; the body still forwards the secret verbatim.
		const plan = planSubmit(
			QUESTIONS,
			{
				selected: { q1: ['alpha'] },
				freeText: { q2: '  secret token  ', q3: 'a note' }
			},
			'in_x'
		);
		expect(plan.body).toEqual({
			inputId: 'in_x',
			answers: [
				{ questionId: 'q1', answers: ['alpha'] },
				{ questionId: 'q2', answers: ['  secret token  '] },
				{ questionId: 'q3', answers: ['a note'] }
			]
		});
		// The retained draft (visible while the promise is in flight AND
		// after an API failure) contains no secret value.
		expect(plan.draft.freeText.q2).toBeUndefined();
		expect(plan.draft.freeText.q3).toBe('a note'); // non-secret stays for retry UX
		expect(plan.draft.selected.q1).toEqual(['alpha']);
	});

	it('does not mutate the caller draft', () => {
		const draft = { selected: {}, freeText: { q2: 'tok' } };
		planSubmit(QUESTIONS, draft, 'in_x');
		expect(draft.freeText.q2).toBe('tok');
	});
});

describe('hasAnyAnswer', () => {
	it('is false with no selections and no text', () => {
		expect(hasAnyAnswer(QUESTIONS, { selected: {}, freeText: {} })).toBe(false);
		expect(hasAnyAnswer(QUESTIONS, { selected: {}, freeText: { q2: '' } })).toBe(false);
	});

	it('is true with a selection or any entered text', () => {
		expect(hasAnyAnswer(QUESTIONS, { selected: { q1: ['alpha'] }, freeText: {} })).toBe(true);
		expect(hasAnyAnswer(QUESTIONS, { selected: {}, freeText: { q2: ' ' } })).toBe(true);
	});
});
