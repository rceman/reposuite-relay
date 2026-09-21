<script lang="ts">
	// Effective (runtime-observed) accounting — SessionMetrics, last-known
	// and never persisted. Absent fields render nothing, never a fake 0.
	import type { SessionMetrics } from '$lib/api/types';

	let { metrics }: { metrics: SessionMetrics | undefined } = $props();

	function int(v: number | undefined): string | undefined {
		return v === undefined ? undefined : v.toLocaleString();
	}

	function quota(used: number | undefined, limit: number | undefined): string | undefined {
		if (used === undefined) return undefined;
		return limit !== undefined
			? `${used.toFixed(1)} / ${limit.toFixed(1)}`
			: used.toFixed(1);
	}

	const rows = $derived.by(() => {
		if (metrics === undefined) return [];
		const out: { label: string; value: string }[] = [];
		if (metrics.model) out.push({ label: 'Effective model', value: metrics.model });
		if (metrics.mode) out.push({ label: 'Effective mode', value: metrics.mode });
		const ctx =
			metrics.contextUsed !== undefined
				? metrics.contextLimit !== undefined
					? `${int(metrics.contextUsed)} / ${int(metrics.contextLimit)}`
					: int(metrics.contextUsed)
				: undefined;
		if (ctx !== undefined) out.push({ label: 'Context', value: ctx });
		const tokens = [
			['Input tokens', metrics.inputTokens],
			['Output tokens', metrics.outputTokens],
			['Reasoning tokens', metrics.reasoningTokens],
			['Cache tokens', metrics.cacheTokens]
		] as const;
		for (const [label, v] of tokens) {
			const s = int(v);
			if (s !== undefined) out.push({ label, value: s });
		}
		const q = quota(metrics.quotaUsed, metrics.quotaLimit);
		if (q !== undefined) out.push({ label: 'Quota', value: q });
		if (metrics.resetAt) out.push({ label: 'Quota resets', value: metrics.resetAt });
		return out;
	});
</script>

{#if rows.length > 0}
	<div class="flex flex-wrap gap-x-6 gap-y-1 text-xs">
		{#each rows as r (r.label)}
			<div>
				<span class="text-muted-foreground">{r.label}</span>
				<span class="ml-1 font-mono tabular-nums">{r.value}</span>
			</div>
		{/each}
	</div>
{/if}
