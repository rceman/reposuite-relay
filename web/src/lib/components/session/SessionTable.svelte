<script lang="ts">
	// One session table — shared by the flat and project-grouped layouts.
	// Read-only; every state is icon + text, never color alone.
	import StatusBadge from '$lib/components/StatusBadge.svelte';
	import type { SessionInfo } from '$lib/api/types';
	import { activityStatus, providerName, runtimeStatus } from '$lib/domain/status';
	import { formatTime } from '$lib/utils/format';
	import {
		Table,
		TableBody,
		TableCell,
		TableHead,
		TableHeader,
		TableRow
	} from '$lib/components/ui/table';

	let { sessions }: { sessions: SessionInfo[] } = $props();
</script>

<Table>
	<TableHeader>
		<TableRow>
			<TableHead>Key</TableHead>
			<TableHead>Provider</TableHead>
			<TableHead>Activity</TableHead>
			<TableHead>Runtime</TableHead>
			<TableHead class="text-right">Gen</TableHead>
			<TableHead>Model</TableHead>
			<TableHead>Created</TableHead>
			<TableHead class="hidden md:table-cell">Working directory</TableHead>
		</TableRow>
	</TableHeader>
	<TableBody>
		{#each sessions as s (s.sessionId)}
			<TableRow>
				<TableCell class="max-w-40 truncate font-mono text-xs" title={s.key}>
					<a href="/sessions/{s.key}" class="underline-offset-2 hover:underline">{s.key}</a>
				</TableCell>
				<TableCell>{providerName(s.harness)}</TableCell>
				<TableCell><StatusBadge status={activityStatus(s)} /></TableCell>
				<TableCell><StatusBadge status={runtimeStatus(s)} /></TableCell>
				<TableCell class="text-right tabular-nums">{s.generation}</TableCell>
				<TableCell class="max-w-32 truncate" title={s.model ?? ''}>{s.model ?? '—'}</TableCell>
				<TableCell class="text-xs text-muted-foreground tabular-nums"
					>{formatTime(s.createdAt)}</TableCell
				>
				<TableCell
					class="hidden max-w-64 truncate text-xs text-muted-foreground md:table-cell"
					title={s.cwd}>{s.cwd}</TableCell
				>
			</TableRow>
		{:else}
			<TableRow>
				<TableCell colspan={8} class="py-8 text-center text-sm text-muted-foreground">
					No sessions yet.
				</TableCell>
			</TableRow>
		{/each}
	</TableBody>
</Table>
