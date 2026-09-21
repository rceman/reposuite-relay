<script lang="ts">
	// The session timeline: durable transcript records + live events as a
	// compact conversation — user/agent messages distinguished by alignment,
	// label, and icon (never color alone); system/control rows are quieter.
	// All content is plain text with whitespace preserved — no HTML ever.
	import type { TimelineRow } from '$lib/session/timeline';
	import { formatTime } from '$lib/utils/format';
	import { IconInfoCircle, IconRobot, IconUser } from '@tabler/icons-svelte';

	let { rows }: { rows: TimelineRow[] } = $props();
</script>

<div class="flex flex-col gap-3">
	{#each rows as row, i (i)}
		{#if row.kind === 'user'}
			<div class="ml-auto max-w-[85%] rounded-lg border bg-muted/40 px-3 py-2">
				<div class="mb-1 flex items-center gap-1.5 text-[11px] font-medium text-muted-foreground">
					<IconUser size={12} stroke={1.75} aria-hidden="true" />
					You · {formatTime(row.at)}
				</div>
				<p class="text-sm break-words whitespace-pre-wrap">{row.text}</p>
			</div>
		{:else if row.kind === 'agent'}
			<div
				class="mr-auto max-w-[85%] rounded-lg border px-3 py-2 {row.live
					? 'border-dashed'
					: 'bg-card'}"
			>
				<div class="mb-1 flex items-center gap-1.5 text-[11px] font-medium text-muted-foreground">
					<IconRobot size={12} stroke={1.75} aria-hidden="true" />
					Agent · {formatTime(row.at)}{row.live ? ' · streaming…' : ''}
				</div>
				<p class="text-sm break-words whitespace-pre-wrap">{row.text}</p>
			</div>
		{:else if row.kind === 'system'}
			<div class="flex items-center gap-2 px-1 text-xs text-muted-foreground">
				<IconInfoCircle size={12} stroke={1.75} aria-hidden="true" />
				<span>{row.label}</span>
				{#if row.detail}
					<span class="truncate font-mono text-[11px]" title={row.detail}>{row.detail}</span>
				{/if}
				<span class="ml-auto shrink-0 tabular-nums">{formatTime(row.at)}</span>
			</div>
		{:else}
			<div class="flex items-center gap-2 px-1 text-xs text-muted-foreground/70">
				<IconInfoCircle size={12} stroke={1.75} aria-hidden="true" />
				<span>Event: {row.eventType}</span>
				<span class="ml-auto shrink-0 tabular-nums">{formatTime(row.at)}</span>
			</div>
		{/if}
	{:else}
		<p class="py-8 text-center text-sm text-muted-foreground">No transcript records yet.</p>
	{/each}
</div>
