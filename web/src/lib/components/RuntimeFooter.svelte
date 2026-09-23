<script lang="ts">
	// Global harness-runtime footer: a compact, always-visible observer
	// strip for the whole daemon — independent of selected project or
	// session. Polls GET /v1/runtimes every 5s while the tab is visible;
	// a failed refresh keeps the last good sample marked stale. Clicking
	// any segment (or the aggregate) opens the detailed inspector Sheet.
	// Hover previews are supplemental — the Sheet holds every detail.
	import { onDestroy, onMount } from 'svelte';
	import { api } from '$lib/api/client';
	import type { RuntimeList } from '$lib/api/types';
	import * as Tooltip from '$lib/components/ui/tooltip';
	import * as Sheet from '$lib/components/ui/sheet';
	import { Separator } from '$lib/components/ui/separator';
	import { ScrollArea } from '$lib/components/ui/scroll-area';
	import { IconActivity, IconX } from '@tabler/icons-svelte';
	import {
		activityText,
		bindingActivity,
		formatBytes,
		groupByProvider,
		segmentMemory,
		shortRuntimeId,
		totalsMemory,
		type ProviderGroup
	} from '$lib/runtimes/model';
	import { formatUptime } from '$lib/utils/format';

	const POLL_MS = 5000;

	let data = $state<RuntimeList | null>(null);
	let stale = $state(false); // last refresh failed after a good sample
	let loading = $state(true); // no successful sample yet
	let failed = $state(false); // never sampled successfully
	let sheetOpen = $state(false);
	let focusProvider = $state<string | null>(null);

	let timer: ReturnType<typeof setInterval> | undefined;
	let inFlight: AbortController | undefined;

	async function poll() {
		inFlight?.abort();
		const ctrl = new AbortController();
		inFlight = ctrl;
		try {
			const out = await api.listRuntimes({ signal: ctrl.signal });
			if (ctrl.signal.aborted) return;
			data = out;
			stale = false;
			failed = false;
		} catch {
			if (ctrl.signal.aborted) return;
			if (data === null) failed = true;
			else stale = true;
		} finally {
			loading = false;
		}
	}

	function syncPolling() {
		clearInterval(timer);
		timer = undefined;
		if (document.visibilityState === 'visible') {
			poll();
			timer = setInterval(poll, POLL_MS);
		} else {
			inFlight?.abort();
		}
	}

	function onVisibility() {
		syncPolling();
	}

	onMount(() => {
		syncPolling();
		document.addEventListener('visibilitychange', onVisibility);
	});

	onDestroy(() => {
		clearInterval(timer);
		inFlight?.abort();
		document.removeEventListener('visibilitychange', onVisibility);
	});

	const groups = $derived(data === null ? [] : groupByProvider(data));

	function openSheet(provider?: string) {
		focusProvider = provider ?? null;
		sheetOpen = true;
	}

	const shownGroups = $derived(
		focusProvider === null ? groups : groups.filter((g) => g.harness === focusProvider)
	);
</script>

{#snippet runtimeRow(rt: import('$lib/api/types').RuntimeInfo)}
	<div class="rounded-md border p-3">
		<div class="flex items-center justify-between text-sm">
			<span class="font-mono font-medium">{shortRuntimeId(rt.runtimeId)}</span>
			<span class="text-xs text-muted-foreground capitalize">{rt.state}</span>
		</div>
		<dl class="mt-2 grid grid-cols-2 gap-x-4 gap-y-1 text-xs">
			<dt class="text-muted-foreground">PID</dt>
			<dd class="text-right">{rt.pid > 0 ? rt.pid : '—'}</dd>
			<dt class="text-muted-foreground">uptime</dt>
			<dd class="text-right">{formatUptime(rt.uptimeSeconds)}</dd>
			<dt class="text-muted-foreground">PSS</dt>
			<dd class="text-right">
				{rt.resources.available ? formatBytes(rt.resources.pssBytes ?? 0) : '—'}
			</dd>
			<dt class="text-muted-foreground">RSS</dt>
			<dd class="text-right">
				{rt.resources.available ? formatBytes(rt.resources.rssBytes ?? 0) : '—'}
			</dd>
			<dt class="text-muted-foreground">processes</dt>
			<dd class="text-right">
				{rt.resources.available ? rt.resources.processCount : '—'}
			</dd>
			<dt class="text-muted-foreground">activity</dt>
			<dd class="text-right">{activityText(rt)}</dd>
		</dl>
		{#if rt.sessions.length > 0}
			<div class="mt-2 border-t pt-2">
				<div class="mb-1 text-xs text-muted-foreground">
					{rt.sessionCount} bound session{rt.sessionCount === 1 ? '' : 's'}
				</div>
				{#each rt.sessions as s (s.sessionId)}
					<div class="flex items-center justify-between text-xs">
						<span class="font-mono">{s.key || s.sessionId.slice(0, 8)}</span>
						<span class="text-muted-foreground">
							{bindingActivity(s.activity)}{s.mutating ? ' · mutating' : ''}
						</span>
					</div>
				{/each}
			</div>
		{/if}
	</div>
{/snippet}

<footer class="sticky bottom-0 border-t bg-background">
	<div
		class="mx-auto flex h-8 w-full max-w-6xl items-center gap-3 px-4 text-xs text-muted-foreground"
	>
		<IconActivity size={13} stroke={1.75} aria-hidden="true" />
		{#if loading}
			<span>Harnesses · loading…</span>
		{:else if failed}
			<span>Harnesses · unavailable</span>
		{:else if data !== null}
			<!-- Compact aggregate (all widths); provider segments on md+. -->
			<button
				type="button"
				class="flex items-center gap-1.5 hover:text-foreground focus-visible:text-foreground focus-visible:outline-none"
				onclick={() => openSheet()}
				aria-haspopup="dialog"
			>
				<span class="font-medium text-foreground">Harnesses</span>
				<span class="hidden sm:inline">
					· {data.totals.runtimeCount} runtime{data.totals.runtimeCount === 1 ? '' : 's'} ·
					{data.totals.sessionCount} session{data.totals.sessionCount === 1 ? '' : 's'}{data
						.totals.activeSessionCount > 0
						? ` · ${data.totals.activeSessionCount} active`
						: ''}
				</span>
				<span>· {totalsMemory(data)} PSS</span>
				{#if stale}
					<span class="italic">(stale)</span>
				{/if}
			</button>
			<Separator orientation="vertical" class="hidden h-4 md:block" />
			<div class="hidden items-center gap-3 md:flex">
				{#each groups as g (g.harness)}
					<Tooltip.Root delayDuration={300}>
						<Tooltip.Trigger>
							<button
								type="button"
								class="hover:text-foreground focus-visible:text-foreground focus-visible:outline-none"
								onclick={() => openSheet(g.harness)}
								aria-label="{g.label}: {g.runtimeCount} runtimes, {segmentMemory(g)} — open details"
							>
								{g.name}
								{g.runtimeCount}{g.runtimeCount > 0 ? ` · ${segmentMemory(g)}` : ''}{g.activeCount > 0
									? ` · ${g.activeCount} active`
									: ''}
							</button>
						</Tooltip.Trigger>
						<Tooltip.Content side="top" class="bg-popover text-popover-foreground border p-3 text-left normal-case w-72">
							<div class="text-xs">
								<div class="mb-1 font-medium">{g.label}</div>
								<div class="space-y-0.5 text-muted-foreground">
									<div>
										{g.runtimeCount} runtime{g.runtimeCount === 1 ? '' : 's'} · {g.sessionCount}
										bound session{g.sessionCount === 1 ? '' : 's'}
										{g.activeCount > 0 ? ` · ${g.activeCount} active` : ''}
									</div>
									<div>
										{g.measuredCount === g.runtimeCount
											? `${formatBytes(g.pssBytes)} PSS · ${formatBytes(g.rssBytes)} RSS`
											: `${formatBytes(g.pssBytes)} PSS measured (${g.measuredCount}/${g.runtimeCount})`}
									</div>
								</div>
								{#each g.runtimes as rt (rt.runtimeId)}
									<div class="mt-2 border-t pt-1.5">
										<div class="flex justify-between font-mono">
											<span>{shortRuntimeId(rt.runtimeId)}</span>
											<span class="text-muted-foreground">PID {rt.pid > 0 ? rt.pid : '—'}</span>
										</div>
										<div class="text-muted-foreground">
											{rt.resources.available ? formatBytes(rt.resources.pssBytes ?? 0) : '—'} PSS ·
											uptime {formatUptime(rt.uptimeSeconds)} · {rt.sessionCount} session{rt
												.sessionCount === 1
												? ''
												: 's'} · {activityText(rt)}
										</div>
									</div>
								{/each}
							</div>
						</Tooltip.Content>
					</Tooltip.Root>
				{/each}
			</div>
		{/if}
	</div>
</footer>

<Sheet.Root bind:open={sheetOpen}>
	<Sheet.Content side="right" class="p-0">
		<div class="flex items-center justify-between border-b px-4 py-3">
			<div>
				<Sheet.Title class="text-sm">Harness Runtimes</Sheet.Title>
				{#if data !== null}
					<Sheet.Description>
						{data.totals.runtimeCount} runtimes · {data.totals.sessionCount} bound sessions ·
						{data.totals.activeSessionCount} active ·
						{data.totals.waitingInputCount} waiting input ·
						{totalsMemory(data)} PSS{data.totals.measuredRuntimeCount < data.totals.runtimeCount
							? ` (measured ${data.totals.measuredRuntimeCount}/${data.totals.runtimeCount})`
							: ''}
					</Sheet.Description>
				{/if}
			</div>
			<Sheet.Close
				class="rounded-md p-1 text-muted-foreground hover:text-foreground focus-visible:outline-2 focus-visible:outline-ring"
				aria-label="Close runtime inspector"
			>
				<IconX size={16} stroke={1.75} aria-hidden="true" />
			</Sheet.Close>
		</div>
		<ScrollArea class="flex-1 px-4">
			{#each shownGroups as g (g.harness)}
				{#if g.runtimeCount > 0 || focusProvider === null}
					<div class="py-3">
						<div class="mb-2 text-xs font-medium uppercase tracking-wide text-muted-foreground">
							{g.label}
							{#if g.runtimeCount === 0}
								<span class="normal-case">· none running</span>
							{/if}
						</div>
						<div class="space-y-2">
							{#each g.runtimes as rt (rt.runtimeId)}
								{@render runtimeRow(rt)}
							{/each}
						</div>
					</div>
				{/if}
			{/each}
			<div class="h-4"></div>
		</ScrollArea>
	</Sheet.Content>
</Sheet.Root>
