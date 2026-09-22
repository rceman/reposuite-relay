// Answer construction: free text is forwarded verbatim — never trimmed —
// because a secret may legally contain surrounding whitespace. Presence
// is rawValue.length > 0; questions with no answer are omitted.
import { describe, expect, it } from 'vitest';
import { buildAnswers, hasAnyAnswer } from './answers';
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
