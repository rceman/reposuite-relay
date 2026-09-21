<script lang="ts">
	// Desired model/mode — the durable config RelaySession carries.
	// Effective runtime values are SessionMetrics, shown separately.
	// Only changed fields are PATCHed; an empty patch is never sent.
	import { api } from '$lib/api/client';
	import { RelayError } from '$lib/api/errors';
	import type { SessionInfo } from '$lib/api/types';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { IconSettings } from '@tabler/icons-svelte';

	let {
		session,
		onupdated
	}: {
		session: SessionInfo;
		onupdated: (s: SessionInfo) => void;
	} = $props();

	let model = $state('');
	let mode = $state('');
	let open = $state(false);
	let busy = $state(false);
	let error = $state<string | null>(null);

	function openForm() {
		model = session.model ?? '';
		mode = session.mode ?? '';
		error = null;
		open = !open;
	}

	const dirty = $derived(model !== (session.model ?? '') || mode !== (session.mode ?? ''));

	async function apply() {
		if (!dirty || busy) return;
		busy = true;
		error = null;
		try {
			const patch: { model?: string; mode?: string } = {};
			if (model !== (session.model ?? '')) patch.model = model.trim();
			if (mode !== (session.mode ?? '')) patch.mode = mode.trim();
			const resp = await api.patchConfig(session.key, patch);
			// The server response is authority for the projection.
			onupdated(resp.session);
			open = false;
		} catch (err) {
			// SESSION_BUSY / INVALID_CONFIG / UNSUPPORTED_OPERATION land here
			// with the server's bounded message; previous values are kept.
			error = err instanceof RelayError ? err.message : 'Config failed';
		} finally {
			busy = false;
		}
	}
</script>

<div class="rounded-lg border">
	<button
		type="button"
		class="flex w-full items-center gap-2 px-3 py-2 text-left text-xs font-medium text-muted-foreground"
		onclick={openForm}
		aria-expanded={open}
	>
		<IconSettings size={14} stroke={1.75} aria-hidden="true" />
		Desired configuration{open ? '' : `: model ${session.model || '—'} · mode ${session.mode || '—'}`}
	</button>
	{#if open}
		<div class="flex flex-wrap items-end gap-3 border-t px-3 py-3">
			<div>
				<Label for="cfg-model" class="text-xs">Desired model</Label>
				<Input id="cfg-model" bind:value={model} class="mt-1 w-56" placeholder="unchanged" />
			</div>
			<div>
				<Label for="cfg-mode" class="text-xs">Desired mode</Label>
				<Input id="cfg-mode" bind:value={mode} class="mt-1 w-40" placeholder="unchanged" />
			</div>
			<Button size="sm" onclick={apply} disabled={!dirty || busy}>
				{busy ? 'Applying…' : 'Apply'}
			</Button>
			{#if error}
				<p class="w-full text-xs text-destructive" role="alert">{error}</p>
			{/if}
		</div>
	{/if}
</div>
