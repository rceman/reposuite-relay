<script lang="ts">
	// Prompt composer — POST /v1/sessions/{key}/prompt via the typed client.
	// Accepted prompts wake a COLD session; canonical events (message.user,
	// agent deltas, completion) update the timeline — nothing is faked.
	import { api } from '$lib/api/client';
	import { RelayError } from '$lib/api/errors';
	import { Button } from '$lib/components/ui/button';
	import { Textarea } from '$lib/components/ui/textarea';
	import { IconSend } from '@tabler/icons-svelte';

	let { sessionKey, cold }: { sessionKey: string; cold: boolean } = $props();

	let text = $state('');
	let busy = $state(false);
	let error = $state<string | null>(null);

	const canSend = $derived(text.trim() !== '' && !busy);

	async function send() {
		const body = text.trim();
		if (body === '' || busy) return;
		busy = true;
		error = null;
		try {
			await api.prompt(sessionKey, body);
			// Accepted — the canonical message.user event drives the timeline.
			text = '';
		} catch (err) {
			error = err instanceof RelayError ? err.message : 'Prompt failed';
		} finally {
			busy = false;
		}
	}

	function onkeydown(e: KeyboardEvent) {
		if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
			e.preventDefault();
			void send();
		}
	}
</script>

<div class="flex flex-col gap-2">
	{#if error}
		<p class="text-xs text-destructive" role="alert">{error}</p>
	{/if}
	<div class="flex items-end gap-2">
		<Textarea
			bind:value={text}
			placeholder={cold
				? 'Send a prompt — this wakes the cold session…'
				: 'Send a prompt…'}
			rows={2}
			class="min-h-10 resize-y"
			{onkeydown}
			disabled={busy}
		/>
		<Button onclick={send} disabled={!canSend} size="sm" class="shrink-0">
			<IconSend size={14} stroke={1.75} aria-hidden="true" />
			{busy ? 'Sending…' : 'Send'}
		</Button>
	</div>
	<p class="text-[11px] text-muted-foreground">
		Ctrl+Enter to send{#if cold} · sending wakes the session{/if}
	</p>
</div>
