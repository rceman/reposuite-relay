// Pure session timeline reducer — no DOM, no Svelte. It consumes durable
// transcript records plus live events and produces renderable rows plus
// the pending requested-input set. Kept separate so the whole reduction
// is unit-testable without a browser.
//
// Sequence discipline: the stream cursor is the snapshot's throughSeq —
// the canonical event cursor, NOT the last durable record seq (transient
// events and reserved gaps advance it past durable records). Live events
// must arrive monotonically; a seq <= lastSeq is a replay duplicate and
// is skipped defensively, never re-applied or re-ordered.
import { DecodeError } from '$lib/api/decode';
import {
	decodeConfigChanged,
	decodeHarnessError,
	decodeHarnessStarted,
	decodeInputAborted,
	decodeInputRequested,
	decodeInputResolved,
	decodeMessageAgent,
	decodeMessageUser,
	decodeMetricsUpdated,
	decodeNativeSession,
	decodeRuntimeExited,
	decodeTurnEvent
} from '$lib/api/payloads';
import type {
	InputRequestedPayload,
	RelayEvent,
	SessionMetrics,
	TranscriptPage,
	TranscriptRecord
} from '$lib/api/types';

export type TimelineRow =
	| {
			kind: 'user';
			seq: number;
			at: string;
			text: string;
			model?: string;
			effort?: string;
	  }
	| {
			kind: 'agent';
			seq: number;
			at: string;
			turnId: string;
			itemId?: string;
			text: string;
			/** live = transient draft still receiving deltas. */
			live: boolean;
	  }
	| { kind: 'system'; seq: number; at: string; label: string; detail?: string }
	| { kind: 'unknown'; seq: number; at: string; eventType: string };

export interface TimelineView {
	rows: TimelineRow[];
	pending: InputRequestedPayload[];
	metrics: SessionMetrics | undefined;
	lastSeq: number;
	hasMoreBefore: boolean;
}

export class Timeline {
	private view: TimelineView = {
		rows: [],
		pending: [],
		metrics: undefined,
		lastSeq: 0,
		hasMoreBefore: false
	};
	/** Live agent drafts keyed by turnId+itemId until completion lands. */
	private drafts = new Map<string, TimelineRow & { kind: 'agent' }>();

	get state(): TimelineView {
		return this.view;
	}

	/**
	 * The canonical stream cursor: the snapshot throughSeq until the first
	 * live event lands, then the highest applied seq. This is the value to
	 * pass as events?after= on (re)subscribe.
	 */
	get cursor(): number {
		return this.view.lastSeq;
	}

	/** hydrate replaces all state from a durable transcript snapshot. */
	hydrate(page: TranscriptPage): void {
		this.view = {
			rows: [],
			pending: [],
			metrics: undefined,
			lastSeq: page.throughSeq,
			hasMoreBefore: page.hasMoreBefore
		};
		this.drafts = new Map();
		for (const rec of page.records) this.applyRecord(rec);
	}

	/**
	 * applyEvent folds one live/replayed event into the timeline.
	 * Returns 'duplicate' for a replayed/already-applied seq.
	 */
	applyEvent(ev: RelayEvent): 'applied' | 'duplicate' {
		if (ev.seq <= this.view.lastSeq) return 'duplicate';
		this.view.lastSeq = ev.seq;
		this.apply(ev.type, ev.payload, ev.seq, ev.at, ev.durable);
		return 'applied';
	}

	/** applyRecord folds one durable transcript record (hydration). */
	private applyRecord(rec: TranscriptRecord): void {
		this.apply(rec.type, rec.payload, rec.seq, rec.at, true);
	}

	private apply(type: string, payload: unknown, seq: number, at: string, _durable: boolean): void {
		try {
			this.applyTyped(type, payload, seq, at, _durable);
		} catch (err) {
			// A malformed payload never crashes the page — render a quiet
			// diagnostic row and keep the stream alive.
			if (err instanceof DecodeError) {
				this.push({ kind: 'system', seq, at, label: `Malformed ${type} event` });
				return;
			}
			throw err;
		}
	}

	private applyTyped(
		type: string,
		payload: unknown,
		seq: number,
		at: string,
		_durable: boolean
	): void {
		switch (type) {
			case 'message.user': {
				const p = decodeMessageUser(payload);
				const row: TimelineRow = { kind: 'user', seq, at, text: p.text };
				if (p.model !== undefined) row.model = p.model;
				if (p.effort !== undefined) row.effort = p.effort;
				this.push(row);
				return;
			}
			case 'message.agent.delta': {
				// Transient draft chunk: accumulate by turn+item; the durable
				// completion replaces it wholesale.
				const p = decodeMessageAgent(payload);
				const id = `${p.turnId}${p.itemId ?? ''}`;
				const draft = this.drafts.get(id);
				if (draft) {
					draft.text += p.text ?? '';
					draft.seq = seq;
					draft.at = at;
					return;
				}
				const row: TimelineRow = {
					kind: 'agent',
					seq,
					at,
					turnId: p.turnId,
					...(p.itemId !== undefined ? { itemId: p.itemId } : {}),
					text: p.text ?? '',
					live: true
				};
				this.drafts.set(id, row as TimelineRow & { kind: 'agent' });
				this.push(row);
				return;
			}
			case 'message.agent.completed': {
				const p = decodeMessageAgent(payload);
				const id = `${p.turnId}${p.itemId ?? ''}`;
				const draft = this.drafts.get(id);
				const row: TimelineRow = {
					kind: 'agent',
					seq,
					at,
					turnId: p.turnId,
					...(p.itemId !== undefined ? { itemId: p.itemId } : {}),
					text: p.text ?? '',
					live: false
				};
				if (draft) {
					// The durable completion is authority — it replaces the
					// transient draft in place, keeping visual position.
					this.drafts.delete(id);
					const i = this.view.rows.indexOf(draft);
					if (i >= 0) {
						this.view.rows[i] = row;
						return;
					}
				}
				this.push(row);
				return;
			}
			case 'turn.failed': {
				const p = decodeTurnEvent(payload);
				this.push({
					kind: 'system',
					seq,
					at,
					label: 'Turn failed',
					...(p.error !== undefined ? { detail: p.error } : {})
				});
				return;
			}
			case 'turn.interrupted': {
				decodeTurnEvent(payload);
				this.push({ kind: 'system', seq, at, label: 'Turn interrupted' });
				return;
			}
			case 'harness.started': {
				const p = decodeHarnessStarted(payload);
				this.push({
					kind: 'system',
					seq,
					at,
					label: p.resumed ? 'Runtime resumed' : 'Runtime started',
					...(p.model !== undefined ? { detail: `model ${p.model}` } : {})
				});
				return;
			}
			case 'runtime.exited': {
				const p = decodeRuntimeExited(payload);
				this.push({
					kind: 'system',
					seq,
					at,
					label: 'Runtime exited',
					...(p.reason !== undefined ? { detail: p.reason } : {})
				});
				return;
			}
			case 'session.native': {
				const p = decodeNativeSession(payload);
				this.push({
					kind: 'system',
					seq,
					at,
					label: 'Native session recorded',
					detail: `generation ${p.generation}`
				});
				return;
			}
			case 'session.config': {
				const p = decodeConfigChanged(payload);
				this.push({
					kind: 'system',
					seq,
					at,
					label: 'Configuration changed',
					detail: `model ${p.model || '—'} · mode ${p.mode || '—'}`
				});
				return;
			}
			case 'input.requested': {
				const p = decodeInputRequested(payload);
				this.view.pending.push(p);
				const heads = p.questions.map((q) => q.header).filter(Boolean).join(' · ');
				this.push({
					kind: 'system',
					seq,
					at,
					label: 'Input requested',
					...(heads !== '' ? { detail: heads } : {})
				});
				return;
			}
			case 'input.resolved': {
				const p = decodeInputResolved(payload);
				this.view.pending = this.view.pending.filter((r) => r.inputId !== p.inputId);
				this.push({ kind: 'system', seq, at, label: 'Input answered' });
				return;
			}
			case 'input.aborted': {
				const p = decodeInputAborted(payload);
				this.view.pending = this.view.pending.filter((r) => r.inputId !== p.inputId);
				this.push({ kind: 'system', seq, at, label: 'Input aborted', detail: p.reason });
				return;
			}
			case 'metrics.updated': {
				const { kind: _kind, ...metrics } = decodeMetricsUpdated(payload);
				this.view.metrics = metrics;
				return; // not a row — the metrics panel reads it directly
			}
			case 'harness.error': {
				const p = decodeHarnessError(payload);
				this.push({
					kind: 'system',
					seq,
					at,
					label: p.fatal ? 'Harness error (fatal)' : 'Harness error',
					detail: p.message
				});
				return;
			}
			default:
				// Unknown event types are a forward-compat surface: a quiet
				// diagnostic row, never a crash.
				this.push({ kind: 'unknown', seq, at, eventType: type });
		}
	}

	private push(row: TimelineRow): void {
		this.view.rows.push(row);
	}
}
