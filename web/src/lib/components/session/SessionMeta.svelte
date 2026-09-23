<script lang="ts">
	// Session identity header: compact fields with full values on title.
	// Desired model/mode are the durable config; effective values live in
	// the metrics panel — the two are never conflated.
	import type { SessionInfo } from '$lib/api/types';
	import { providerName } from '$lib/domain/status';
	import StatusBadge from '$lib/components/StatusBadge.svelte';
	import { activityStatus, runtimeStatus } from '$lib/domain/status';
	import { formatTime } from '$lib/utils/format';

	let { session }: { session: SessionInfo } = $props();

	const fields = $derived(
		[
			{ label: 'Provider', value: providerName(session.harness) },
			// Derived grouping projection — observational only; project
			// editing lives under /settings.
			{ label: 'Project', value: session.projectName ?? 'Ungrouped' },
			{ label: 'Desired model', value: session.model || '—' },
			{ label: 'Desired mode', value: session.mode || '—' },
			{ label: 'Generation', value: String(session.generation) },
			{ label: 'PID', value: session.pid > 0 ? String(session.pid) : '—' },
			{ label: 'Native session', value: session.nativeSessionId || '—', mono: true },
			{ label: 'Runtime', value: session.runtimeId || '—', mono: true },
			{ label: 'Created', value: formatTime(session.createdAt), title: session.createdAt },
			{ label: 'Working directory', value: session.cwd, mono: true }
		] satisfies { label: string; value: string; mono?: boolean; title?: string }[]
	);
</script>

<div class="flex flex-wrap items-start gap-x-6 gap-y-2">
	<div class="flex items-center gap-2">
		<h1 class="font-mono text-lg font-semibold" title={session.key}>{session.key}</h1>
		<StatusBadge status={activityStatus(session)} />
		<StatusBadge status={runtimeStatus(session)} />
	</div>
	<dl class="grid flex-1 grid-cols-2 gap-x-6 gap-y-1 text-xs sm:grid-cols-3 lg:grid-cols-5">
		{#each fields as f (f.label)}
			<div class="min-w-0">
				<dt class="text-muted-foreground">{f.label}</dt>
				<dd
					class="truncate {f.mono ? 'font-mono' : ''}"
					title={f.title ?? (f.value !== '—' ? f.value : '')}
				>
					{f.value}
				</dd>
			</div>
		{/each}
	</dl>
</div>
