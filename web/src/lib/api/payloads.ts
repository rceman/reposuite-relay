// Narrow runtime decoders for the canonical event payloads the UI
// renders. Event/record payloads arrive as `unknown`; each decoder
// narrows to the canonical shape from internal/api/events.go (and the
// Codex input payloads) or throws DecodeError. Unknown extra fields are
// ignored for forward compatibility.
import { DecodeError } from './decode';
import type {
	ConfigChangedPayload,
	HarnessErrorPayload,
	HarnessStartedPayload,
	InputAbortedPayload,
	InputQuestion,
	InputRequestedPayload,
	InputResolvedPayload,
	MessageAgentPayload,
	MessageUserPayload,
	MetricsUpdatedPayload,
	NativeSessionPayload,
	RuntimeExitedPayload,
	SessionMetrics,
	TurnEventPayload
} from './types';

function isRecord(v: unknown): v is Record<string, unknown> {
	return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function reqString(v: Record<string, unknown>, key: string): string {
	const s = v[key];
	if (typeof s !== 'string') throw new DecodeError(key);
	return s;
}

function reqBool(v: Record<string, unknown>, key: string): boolean {
	const b = v[key];
	if (typeof b !== 'boolean') throw new DecodeError(key);
	return b;
}

function optString(v: Record<string, unknown>, key: string): string | undefined {
	const s = v[key];
	if (s === undefined) return undefined;
	if (typeof s !== 'string') throw new DecodeError(key);
	return s;
}

function optBool(v: Record<string, unknown>, key: string): boolean | undefined {
	const b = v[key];
	if (b === undefined) return undefined;
	if (typeof b !== 'boolean') throw new DecodeError(key);
	return b;
}

function optNumber(v: Record<string, unknown>, key: string): number | undefined {
	const n = v[key];
	if (n === undefined) return undefined;
	if (typeof n !== 'number' || !Number.isFinite(n)) throw new DecodeError(key);
	return n;
}

export function decodeMessageUser(v: unknown): MessageUserPayload {
	if (!isRecord(v)) throw new DecodeError('message.user');
	const out: MessageUserPayload = { text: reqString(v, 'text') };
	const model = optString(v, 'model');
	const effort = optString(v, 'effort');
	if (model !== undefined) out.model = model;
	if (effort !== undefined) out.effort = effort;
	return out;
}

/** message.agent.completed and message.agent.delta share this shape. */
export function decodeMessageAgent(v: unknown): MessageAgentPayload {
	if (!isRecord(v)) throw new DecodeError('message.agent');
	const out: MessageAgentPayload = { turnId: reqString(v, 'turnId') };
	const itemId = optString(v, 'itemId');
	const text = optString(v, 'text');
	if (itemId !== undefined) out.itemId = itemId;
	if (text !== undefined) out.text = text;
	return out;
}

export function decodeTurnEvent(v: unknown): TurnEventPayload {
	if (!isRecord(v)) throw new DecodeError('turn.*');
	const out: TurnEventPayload = {};
	const turnId = optString(v, 'turnId');
	const error = optString(v, 'error');
	if (turnId !== undefined) out.turnId = turnId;
	if (error !== undefined) out.error = error;
	return out;
}

export function decodeHarnessStarted(v: unknown): HarnessStartedPayload {
	if (!isRecord(v)) throw new DecodeError('harness.started');
	const out: HarnessStartedPayload = {
		runtimeId: reqString(v, 'runtimeId'),
		nativeSessionId: reqString(v, 'nativeSessionId'),
		resumed: reqBool(v, 'resumed')
	};
	const model = optString(v, 'model');
	if (model !== undefined) out.model = model;
	return out;
}

export function decodeRuntimeExited(v: unknown): RuntimeExitedPayload {
	if (!isRecord(v)) throw new DecodeError('runtime.exited');
	const out: RuntimeExitedPayload = { runtimeId: reqString(v, 'runtimeId') };
	const reason = optString(v, 'reason');
	if (reason !== undefined) out.reason = reason;
	return out;
}

export function decodeConfigChanged(v: unknown): ConfigChangedPayload {
	if (!isRecord(v)) throw new DecodeError('session.config');
	return { model: reqString(v, 'model'), mode: reqString(v, 'mode') };
}

export function decodeNativeSession(v: unknown): NativeSessionPayload {
	if (!isRecord(v)) throw new DecodeError('session.native');
	const generation = v.generation;
	if (typeof generation !== 'number' || !Number.isSafeInteger(generation)) {
		throw new DecodeError('generation');
	}
	return { nativeSessionId: reqString(v, 'nativeSessionId'), generation };
}

export function decodeHarnessError(v: unknown): HarnessErrorPayload {
	if (!isRecord(v)) throw new DecodeError('harness.error');
	const out: HarnessErrorPayload = { message: reqString(v, 'message') };
	const fatal = optBool(v, 'fatal');
	if (fatal !== undefined) out.fatal = fatal;
	return out;
}

/** metrics.updated embeds SessionMetrics flat (Go embed on the wire). */
export function decodeMetricsUpdated(v: unknown): MetricsUpdatedPayload {
	if (!isRecord(v)) throw new DecodeError('metrics.updated');
	const out: MetricsUpdatedPayload = { kind: reqString(v, 'kind') };
	const str = ['model', 'mode', 'resetAt'] as const;
	for (const k of str) {
		const s = optString(v, k);
		if (s !== undefined) out[k] = s;
	}
	const num = [
		'contextUsed',
		'contextLimit',
		'inputTokens',
		'outputTokens',
		'reasoningTokens',
		'cacheTokens',
		'quotaUsed',
		'quotaLimit'
	] as const;
	for (const k of num) {
		const n = optNumber(v, k);
		if (n !== undefined) out[k] = n;
	}
	return out;
}

function decodeQuestion(v: unknown): InputQuestion {
	if (!isRecord(v)) throw new DecodeError('question');
	const out: InputQuestion = {
		id: reqString(v, 'id'),
		header: reqString(v, 'header'),
		question: reqString(v, 'question')
	};
	const raw = v.options;
	if (raw !== undefined) {
		if (!Array.isArray(raw)) throw new DecodeError('question.options');
		out.options = raw.map((o) => {
			if (!isRecord(o)) throw new DecodeError('question.options');
			const label = o.label;
			if (typeof label !== 'string') throw new DecodeError('option.label');
			const description = o.description;
			if (description !== undefined && typeof description !== 'string') {
				throw new DecodeError('option.description');
			}
			const res: { label: string; description?: string } = { label };
			if (description !== undefined) res.description = description;
			return res;
		});
	}
	const isOther = optBool(v, 'isOther');
	const isSecret = optBool(v, 'isSecret');
	if (isOther !== undefined) out.isOther = isOther;
	if (isSecret !== undefined) out.isSecret = isSecret;
	return out;
}

export function decodeInputRequested(v: unknown): InputRequestedPayload {
	if (!isRecord(v)) throw new DecodeError('input.requested');
	const raw = v.questions;
	if (!Array.isArray(raw)) throw new DecodeError('questions');
	return {
		inputId: reqString(v, 'inputId'),
		turnId: reqString(v, 'turnId'),
		itemId: reqString(v, 'itemId'),
		isBlocking: reqBool(v, 'isBlocking'),
		questions: raw.map(decodeQuestion)
	};
}

export function decodeInputResolved(v: unknown): InputResolvedPayload {
	if (!isRecord(v)) throw new DecodeError('input.resolved');
	const raw = v.answers;
	if (!isRecord(raw)) throw new DecodeError('answers');
	const answers: Record<string, string[]> = {};
	for (const [k, list] of Object.entries(raw)) {
		if (!Array.isArray(list) || list.some((a) => typeof a !== 'string')) {
			throw new DecodeError('answers');
		}
		answers[k] = list as string[];
	}
	const out: InputResolvedPayload = { inputId: reqString(v, 'inputId'), answers };
	const redacted = v.redacted;
	if (redacted !== undefined) {
		if (!Array.isArray(redacted) || redacted.some((r) => typeof r !== 'string')) {
			throw new DecodeError('redacted');
		}
		out.redacted = redacted as string[];
	}
	return out;
}

export function decodeInputAborted(v: unknown): InputAbortedPayload {
	if (!isRecord(v)) throw new DecodeError('input.aborted');
	return { inputId: reqString(v, 'inputId'), reason: reqString(v, 'reason') };
}
