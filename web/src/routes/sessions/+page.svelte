<script lang="ts">
	// Sessions: the durable RelaySession registry, read-only. Every state
	// is rendered as icon + text — never color alone.
	import AuthGate from '$lib/components/AuthGate.svelte';
	import StatusBadge from '$lib/components/StatusBadge.svelte';
	import { api } from '$lib/api/client';
	import { RelayError } from '$lib/api/errors';
	import type { SessionList } from '$lib/api/types';
	import { activityStatus, providerName, runtimeStatus } from '$lib/domain/status';
	import { formatTime } from '$lib/utils/format';
	import { auth } from '$lib/auth/auth.svelte';
	import {
		Table,
		TableBody,
		TableCell,
		TableHead,
		TableHeader,
		TableRow
	} from '$lib/components/ui/table';
	import { Card, CardContent } from '$lib/components/ui/card';
	import { Alert, AlertDescription, AlertTitle } from '$lib/components/ui/alert';
	import { Button } from '$lib/components/ui/button';
	import { IconAlertTriangle, IconPlus } from '@tabler/icons-svelte';

	let data = $state<SessionList | null>(null);
	let error = $state<string | null>(null);

	async function load() {
		try {
			data = await api.listSessions();
			error = null;
		} catch (err) {
			error = err instanceof RelayError ? err.message : 'Failed to load sessions';
		}
	}

	$effect(() => {
		if (auth.authenticated) void load();
	});
</script>

<AuthGate onrefresh={load}>
	{#if error}
		<Alert variant="destructive" class="mb-4">
			<IconAlertTriangle size={16} aria-hidden="true" />
			<AlertTitle>Load failed</AlertTitle>
			<AlertDescription>{error}</AlertDescription>
		</Alert>
	{/if}

	<div class="mb-4 flex items-center justify-between">
		<h1 class="text-lg font-semibold">Sessions</h1>
		<Button href="/sessions/new" size="sm">
			<IconPlus size={14} stroke={1.75} aria-hidden="true" />
			New session
		</Button>
	</div>

	<Card>
		<CardContent class="p-0">
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
					{#each data?.sessions ?? [] as s (s.sessionId)}
						<TableRow>
							<TableCell class="max-w-40 truncate font-mono text-xs" title={s.key}>
								<a href="/sessions/{s.key}" class="underline-offset-2 hover:underline"
									>{s.key}</a
								>
							</TableCell>
							<TableCell>{providerName(s.harness)}</TableCell>
							<TableCell><StatusBadge status={activityStatus(s)} /></TableCell>
							<TableCell><StatusBadge status={runtimeStatus(s)} /></TableCell>
							<TableCell class="text-right tabular-nums">{s.generation}</TableCell>
							<TableCell class="max-w-32 truncate" title={s.model ?? ''}
								>{s.model ?? '—'}</TableCell
							>
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
		</CardContent>
	</Card>
</AuthGate>
