<script lang="ts">
	// Overview: daemon-level counters from GET /v1/sessions plus a compact
	// recent-session list. This page is a pure observer — reads never wake
	// a COLD session or start a harness runtime.
	import AuthGate from '$lib/components/AuthGate.svelte';
	import StatusBadge from '$lib/components/StatusBadge.svelte';
	import { api } from '$lib/api/client';
	import { RelayError } from '$lib/api/errors';
	import type { ProjectInfo, SessionList } from '$lib/api/types';
	import { activityStatus, providerName, runtimeStatus } from '$lib/domain/status';
	import { formatUptime } from '$lib/utils/format';
	import {
		Card,
		CardContent,
		CardHeader,
		CardTitle
	} from '$lib/components/ui/card';
	import { Alert, AlertDescription, AlertTitle } from '$lib/components/ui/alert';
	import { IconAlertTriangle } from '@tabler/icons-svelte';
	import { auth } from '$lib/auth/auth.svelte';

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

	// Fire once the gate confirms authentication — /v1 needs the live
	// admin cookie, and a fresh login after expiry must re-arm the load.
	$effect(() => {
		if (auth.authenticated) void load();
	});

	const recent = $derived((data?.sessions ?? []).slice(0, 5));
	// Per-project session/active counts from the backend projection.
	const projectCounts = $derived.by(() => {
		const m = new Map<string, { total: number; active: number }>();
		for (const s of data?.sessions ?? []) {
			if (s.projectId === undefined) continue;
			const c = m.get(s.projectId) ?? { total: 0, active: 0 };
			c.total++;
			if (s.runtimeState !== 'cold') c.active++;
			m.set(s.projectId, c);
		}
		return m;
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

	{#if data}
		<div class="mb-6 grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
			<Card>
				<CardHeader class="pb-2"><CardTitle class="text-xs font-medium text-muted-foreground">Sessions</CardTitle></CardHeader>
				<CardContent><p class="text-2xl font-semibold tabular-nums">{data.daemon.sessionCount}</p></CardContent>
			</Card>
			<Card>
				<CardHeader class="pb-2"><CardTitle class="text-xs font-medium text-muted-foreground">Active</CardTitle></CardHeader>
				<CardContent><p class="text-2xl font-semibold tabular-nums">{data.daemon.activeSessions}</p></CardContent>
			</Card>
			<Card>
				<CardHeader class="pb-2"><CardTitle class="text-xs font-medium text-muted-foreground">Cold</CardTitle></CardHeader>
				<CardContent><p class="text-2xl font-semibold tabular-nums">{data.daemon.coldSessions}</p></CardContent>
			</Card>
			<Card>
				<CardHeader class="pb-2"><CardTitle class="text-xs font-medium text-muted-foreground">Uptime</CardTitle></CardHeader>
				<CardContent><p class="text-2xl font-semibold tabular-nums">{formatUptime(data.daemon.uptimeSeconds)}</p></CardContent>
			</Card>
			<Card>
				<CardHeader class="pb-2"><CardTitle class="text-xs font-medium text-muted-foreground">API</CardTitle></CardHeader>
				<CardContent><p class="text-2xl font-semibold tabular-nums">v{data.daemon.apiVersion}</p></CardContent>
			</Card>
			<Card>
				<CardHeader class="pb-2"><CardTitle class="text-xs font-medium text-muted-foreground">Projects</CardTitle></CardHeader>
				<CardContent><p class="text-2xl font-semibold tabular-nums">{projects.length}</p></CardContent>
			</Card>
		</div>

		{#if projects.length > 0}
			<Card class="mb-6">
				<CardHeader><CardTitle class="text-sm">Projects</CardTitle></CardHeader>
				<CardContent>
					<ul class="divide-y">
						{#each projects as p (p.id)}
							{@const c = projectCounts.get(p.id)}
							<li class="flex items-center gap-3 py-2 text-sm">
								<span class="w-48 truncate font-medium" title={p.name}>{p.name}</span>
								<span class="min-w-0 truncate font-mono text-xs text-muted-foreground"
									>{p.root}</span
								>
								<span class="ml-auto text-xs text-muted-foreground tabular-nums">
									{c?.total ?? 0} sessions · {c?.active ?? 0} active ·
									{(c?.total ?? 0) - (c?.active ?? 0)} cold
								</span>
							</li>
						{/each}
					</ul>
				</CardContent>
			</Card>
		{/if}

		<Card>
			<CardHeader><CardTitle class="text-sm">Recent sessions</CardTitle></CardHeader>
			<CardContent>
				{#if recent.length === 0}
					<p class="text-sm text-muted-foreground">No sessions yet.</p>
				{:else}
					<ul class="divide-y">
						{#each recent as s (s.sessionId)}
							<li class="flex items-center gap-3 py-2 text-sm">
								<span class="w-40 truncate font-mono text-xs" title={s.key}>{s.key}</span>
								<span class="w-20 text-muted-foreground">{providerName(s.harness)}</span>
								<span
									class="w-32 truncate text-xs text-muted-foreground"
									title={s.projectName ?? ''}>{s.projectName ?? 'Ungrouped'}</span
								>
								<StatusBadge status={runtimeStatus(s)} />
								<StatusBadge status={activityStatus(s)} />
								<span
									class="ml-auto hidden truncate text-xs text-muted-foreground sm:block"
									title={s.cwd}>{s.cwd}</span
								>
							</li>
						{/each}
					</ul>
				{/if}
			</CardContent>
		</Card>
	{/if}
</AuthGate>
