<script lang="ts">
	// Sessions: the durable RelaySession registry grouped by local Project.
	// Membership is the backend's projectId projection — this page never
	// re-derives path matching. Ungrouped is a legitimate final section.
	import AuthGate from '$lib/components/AuthGate.svelte';
	import SessionTable from '$lib/components/session/SessionTable.svelte';
	import { api } from '$lib/api/client';
	import { RelayError } from '$lib/api/errors';
	import type { ProjectInfo, SessionList } from '$lib/api/types';
	import { groupSessions } from '$lib/projects/group';
	import { auth } from '$lib/auth/auth.svelte';
	import { Card, CardContent } from '$lib/components/ui/card';
	import { Alert, AlertDescription, AlertTitle } from '$lib/components/ui/alert';
	import { Button } from '$lib/components/ui/button';
	import { IconAlertTriangle, IconPlus } from '@tabler/icons-svelte';

	let data = $state<SessionList | null>(null);
	let projects = $state<ProjectInfo[]>([]);
	let error = $state<string | null>(null);

	async function load() {
		try {
			const [sl, pl] = await Promise.all([api.listSessions(), api.listProjects()]);
			data = sl;
			projects = pl.projects;
			error = null;
		} catch (err) {
			error = err instanceof RelayError ? err.message : 'Failed to load sessions';
		}
	}

	$effect(() => {
		if (auth.authenticated) void load();
	});

	const grouped = $derived(groupSessions(projects, data?.sessions ?? []));
	// On /sessions, project sections with zero sessions are omitted to
	// reduce noise — Settings still lists every configured project.
	const visible = $derived(grouped.sections.filter((s) => s.sessions.length > 0));
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

	{#each visible as section (section.project.id)}
		<section class="mb-6">
			<div class="mb-2 flex items-baseline gap-2">
				<h2 class="text-sm font-semibold">{section.project.name}</h2>
				<span class="max-w-64 truncate font-mono text-xs text-muted-foreground"
					>{section.project.root}</span
				>
				<span class="text-xs text-muted-foreground tabular-nums"
					>{section.sessions.length}
					{section.sessions.length === 1 ? 'session' : 'sessions'}</span
				>
			</div>
			<Card>
				<CardContent class="p-0">
					<SessionTable sessions={section.sessions} />
				</CardContent>
			</Card>
		</section>
	{/each}

	<section>
		{#if visible.length > 0 || grouped.ungrouped.length > 0}
			<div class="mb-2 flex items-baseline gap-2">
				<h2 class="text-sm font-semibold">Ungrouped</h2>
				<span class="text-xs text-muted-foreground tabular-nums"
					>{grouped.ungrouped.length}
					{grouped.ungrouped.length === 1 ? 'session' : 'sessions'}</span
				>
			</div>
		{/if}
		<Card>
			<CardContent class="p-0">
				<SessionTable sessions={grouped.ungrouped} />
			</CardContent>
		</Card>
	</section>
</AuthGate>
