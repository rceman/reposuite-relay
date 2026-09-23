// Runtime-inventory view model: pure derived helpers for the global
// harness footer. All wire data arrives already decoded; this module
// only groups, orders, and formats it.
import type { RuntimeInfo, RuntimeList } from '../api/types';

/** Canonical provider order: first-class slots, then anything else. */
export const PROVIDER_ORDER = ['codex', 'devin', 'opencode'] as const;

/** Human-facing harness server labels. */
const PROVIDER_LABELS: Record<string, string> = {
	codex: 'Codex app-server',
	devin: 'Devin ACP',
	opencode: 'OpenCode ACP'
};

export function providerLabel(harness: string): string {
	return PROVIDER_LABELS[harness] ?? harness;
}

export function providerName(harness: string): string {
	const l = PROVIDER_LABELS[harness];
	return (l === undefined ? harness : (l.split(' ')[0] ?? harness));
}

/** One footer segment per provider, canonical order, then others. */
export interface ProviderGroup {
	harness: string;
	name: string;
	label: string;
	runtimes: RuntimeInfo[];
	runtimeCount: number;
	sessionCount: number;
	activeCount: number;
	/** Measured PSS sum; partial when measuredCount < runtimeCount. */
	pssBytes: number;
	rssBytes: number;
	measuredCount: number;
}

export function groupByProvider(list: RuntimeList): ProviderGroup[] {
	const byHarness = new Map<string, RuntimeInfo[]>();
	for (const rt of list.runtimes) {
		const arr = byHarness.get(rt.harness) ?? [];
		arr.push(rt);
		byHarness.set(rt.harness, arr);
	}
	const order = [...PROVIDER_ORDER, ...[...byHarness.keys()].filter(
		(h) => !PROVIDER_ORDER.includes(h as (typeof PROVIDER_ORDER)[number])
	).sort()];
	return order.map((h) => {
		const rts = byHarness.get(h) ?? [];
		let sessions = 0, active = 0, pss = 0, rss = 0, measured = 0;
		for (const r of rts) {
			sessions += r.sessionCount;
			active += r.activeSessionCount;
			if (r.resources.available) {
				measured++;
				pss += r.resources.pssBytes ?? 0;
				rss += r.resources.rssBytes ?? 0;
			}
		}
		return {
			harness: h,
			name: providerName(h),
			label: providerLabel(h),
			runtimes: rts,
			runtimeCount: rts.length,
			sessionCount: sessions,
			activeCount: active,
			pssBytes: pss,
			rssBytes: rss,
			measuredCount: measured
		};
	});
}

/** "336 MiB", "1.2 GiB" — never silently rounds to a wrong unit. */
export function formatBytes(bytes: number): string {
	if (bytes < 1024) return `${bytes} B`;
	const mib = bytes / (1024 * 1024);
	if (mib < 1024) return `${mib >= 100 ? Math.round(mib) : mib.toFixed(1)} MiB`;
	return `${(mib / 1024).toFixed(2)} GiB`;
}

/**
 * Provider-segment memory text: exact when fully measured, ≥ when
 * partial, — when nothing measured. Counts are always shown.
 */
export function segmentMemory(g: ProviderGroup): string {
	if (g.measuredCount === 0) return 'PSS —';
	const v = formatBytes(g.pssBytes);
	return g.measuredCount < g.runtimeCount ? `≥${v}` : v;
}

/** Totals line memory text — same partial semantics as segments. */
export function totalsMemory(list: RuntimeList): string {
	const t = list.totals;
	if (t.measuredRuntimeCount === 0) return 'PSS —';
	const v = formatBytes(t.pssBytes);
	return t.measuredRuntimeCount < t.runtimeCount ? `≥${v}` : v;
}

/** "#8f31a2" — deterministic short prefix of the ephemeral runtime ID. */
export function shortRuntimeId(id: string): string {
	return '#' + id.slice(0, 6);
}

/** "2 active · 1 waiting" / "Active" / "Idle" — compact activity text. */
export function activityText(rt: RuntimeInfo): string {
	const parts: string[] = [];
	if (rt.activeSessionCount > 0) parts.push(`${rt.activeSessionCount} active`);
	if (rt.waitingInputCount > 0) parts.push(`${rt.waitingInputCount} waiting input`);
	if (rt.mutationCount > 0) parts.push(`${rt.mutationCount} mutating`);
	return parts.length ? parts.join(' · ') : 'Idle';
}

export function bindingActivity(a: string): string {
	switch (a) {
		case 'active': return 'Active';
		case 'waiting_input': return 'Waiting input';
		default: return 'Idle';
	}
}
