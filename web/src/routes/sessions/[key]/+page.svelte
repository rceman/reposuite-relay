<script lang="ts">
	// Session detail: durable session projection + transcript → live event
	// stream. One NDJSON stream per viewed session, aborted on navigation.
	// Reads never wake a COLD session; prompt/cancel/input/config/delete
	// are the mutating surfaces.
	import AuthGate from '$lib/components/AuthGate.svelte';
	import { api } from '$lib/api/client';
	import { RelayError } from '$lib/api/errors';
	import { auth } from '$lib/auth/auth.svelte';
	import { page } from '$app/state';
	import { SessionLive, liveDeps } from '$lib/session/live.svelte';
	import type { SessionInfo } from '$lib/api/types';
	import { Button } from '$lib/components/ui/button';
	import { Card, CardContent, CardHeader, CardTitle } from '$lib/components/ui/card';
	import { ScrollArea } from '$lib/components/ui/scroll-area';
	import { Skeleton } from '$lib/components/ui/skeleton';
	import SessionMeta from '$lib/components/session/SessionMeta.svelte';
	import MetricsPanel from '$lib/components/session/MetricsPanel.svelte';
	import LiveState from '$lib/components/session/LiveState.svelte';
	import Timeline from '$lib/components/session/Timeline.svelte';
	import PromptComposer from '$lib/components/session/PromptComposer.svelte';
	import InputCard from '$lib/components/session/InputCard.svelte';
	import ConfigPanel from '$lib/components/session/ConfigPanel.svelte';
	import DeleteDialog from '$lib/components/session/DeleteDialog.svelte';
	import { IconArrowLeft, IconPlayerStop, IconRefresh } from '@tabler/icons-svelte';

	const KEY_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
	// Transcript tail growth for load-more — bounded by TranscriptMaxLimit.
	const LIMIT_STEPS = [200, 400, 800, 1000] as const;

	const key = $derived(page.params.key ?? '');

	let session = $state<SessionInfo | null>(null);
	let notFound = $state<string | null>(null);
	let live = $state<SessionLive | null>(null);
	let limitIdx = $state(0);
	let cancelErr = $state<string | null>(null);
	let cancelling = $state(false);
	let refreshTimer: ReturnType<typeof setTimeout> | null = null;

	// Events that change the session projection — coalesce into one refresh.
	const STATE_EVENTS = new Set([
		'harness.started',
		'runtime.exited',
		'turn.failed',
		'turn.interrupted',
		'message.agent.completed',
		'input.requested',
		'input.resolved',
		'input.aborted',
		'session.native',
		'session.config'
	]);

	async function refreshSession(k: string) {
		try {
			const resp = await api.getSession(k);
			session = resp.session;
			notFound = null;
		} catch (err) {
			if (err instanceof RelayError && err.status === 404) {
				notFound = 'This session no longer exists.';
			}
		}
	}

	function onStateEvent(type: string) {
		if (!STATE_EVENTS.has(type)) return;
		if (refreshTimer !== null) return;
		refreshTimer = setTimeout(() => {
			refreshTimer = null;
			void refreshSession(key);
		}, 250);
	}

	$effect(() => {
		if (!auth.authenticated || !KEY_RE.test(key)) return;
		const k = key;
		const l = new SessionLive(k, liveDeps(k, () => void auth.refresh()), onStateEvent);
		live = l;
		limitIdx = 0;
		session = null;
		notFound = null;
		void refreshSession(k);
		void l.start();
		return () => {
			l.stop();
			if (refreshTimer !== null) clearTimeout(refreshTimer);
			refreshTimer = null;
		};
	});

	const view = $derived(live?.view);
	const pending = $derived(view?.pending ?? []);
	const canCancel = $derived(
		session !== null && (session.activity === 'active' || session.activity === 'waiting_input')
	);

	async function cancelTurn() {
		if (!canCancel || cancelling) return;
		cancelling = true;
		cancelErr = null;
		try {
			await api.cancel(key);
			// Acceptance only — the turn.* terminal event reconciles state.
		} catch (err) {
			if (err instanceof RelayError && err.code === 'NO_ACTIVE_TURN') {
				// The turn already ended — reconcile quietly via status refresh.
				void refreshSession(key);
			} else {
				cancelErr = err instanceof RelayError ? err.message : 'Cancel failed';
			}
		} finally {
			cancelling = false;
		}
	}

	async function loadMore() {
		if (live === null || limitIdx >= LIMIT_STEPS.length - 1) return;
		limitIdx++;
		await live.setLimit(LIMIT_STEPS[limitIdx] ?? 1000);
	}
</script>

<AuthGate>
	{#if !KEY_RE.test(key)}
		<p class="text-sm text-destructive">Invalid session key.</p>
	{:else if notFound}
		<div class="flex flex-col items-start gap-3">
			<Button href="/sessions" variant="ghost" size="sm" class="-ml-2">
				<IconArrowLeft size={14} stroke={1.75} aria-hidden="true" />
				Sessions
			</Button>
			<p class="text-sm text-muted-foreground">{notFound}</p>
		</div>
	{:else if session === null}
		<div class="flex flex-col gap-3">
			<Skeleton class="h-8 w-64" />
			<Skeleton class="h-24 w-full" />
			<Skeleton class="h-64 w-full" />
		</div>
	{:else}
		<div class="flex flex-col gap-4">
			<div class="flex items-center justify-between gap-2">
				<Button href="/sessions" variant="ghost" size="sm" class="-ml-2">
					<IconArrowLeft size={14} stroke={1.75} aria-hidden="true" />
					Sessions
				</Button>
				<div class="flex items-center gap-3">
					{#if live}
						<LiveState state={live.state} />
					{/if}
					<Button
						variant="ghost"
						size="icon-sm"
						title="Refresh session"
						onclick={() => refreshSession(key)}
					>
						<IconRefresh size={15} stroke={1.75} aria-hidden="true" />
					</Button>
				</div>
			</div>

			<SessionMeta {session} />

			{#if view?.metrics}
				<MetricsPanel metrics={view.metrics} />
			{:else if session.metrics}
				<MetricsPanel metrics={session.metrics} />
			{/if}

			{#if live?.error}
				<p class="text-xs text-destructive" role="alert">{live.error}</p>
			{/if}

			{#each pending as req (req.inputId)}
				<InputCard sessionKey={key} request={req} />
			{/each}

			<Card>
				<CardHeader class="flex-row items-center justify-between space-y-0 pb-3">
					<CardTitle class="text-sm">Transcript</CardTitle>
					{#if view?.hasMoreBefore}
						{#if (LIMIT_STEPS[limitIdx] ?? 200) < 1000}
							<Button variant="outline" size="sm" onclick={loadMore}>
								Load more history
							</Button>
						{:else}
							<span class="text-xs text-muted-foreground">
								Older history exists beyond the current API window
							</span>
						{/if}
					{/if}
				</CardHeader>
				<CardContent>
					<ScrollArea class="max-h-[32rem]">
						<Timeline rows={view?.rows ?? []} />
					</ScrollArea>
				</CardContent>
			</Card>

			{#if cancelErr}
				<p class="text-xs text-destructive" role="alert">{cancelErr}</p>
			{/if}
			<div class="flex items-start gap-2">
				<div class="flex-1">
					<PromptComposer sessionKey={key} cold={session.runtimeState === 'cold'} />
				</div>
				<Button
					variant="outline"
					size="sm"
					onclick={cancelTurn}
					disabled={!canCancel || cancelling}
					class="mt-0.5 shrink-0"
				>
					<IconPlayerStop size={14} stroke={1.75} aria-hidden="true" />
					{cancelling ? 'Cancelling…' : 'Cancel turn'}
				</Button>
			</div>

			<div class="flex items-start justify-between gap-3">
				<div class="flex-1">
					<ConfigPanel
						{session}
						onupdated={(s) => {
							session = s;
						}}
					/>
				</div>
				<DeleteDialog
					sessionKey={key}
					ondelete={() => {
						live?.stop();
					}}
				/>
			</div>
		</div>
	{/if}
</AuthGate>
