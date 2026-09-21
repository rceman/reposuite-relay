<script lang="ts">
	// Durable session deletion — explicit AlertDialog confirmation naming
	// the session key. DELETE removes the RelaySession permanently; it is
	// not a runtime stop, so it is never labeled as one.
	import { api } from '$lib/api/client';
	import { RelayError } from '$lib/api/errors';
	import { goto } from '$app/navigation';
	import { Button } from '$lib/components/ui/button';
	import {
		AlertDialog,
		AlertDialogAction,
		AlertDialogCancel,
		AlertDialogContent,
		AlertDialogDescription,
		AlertDialogFooter,
		AlertDialogHeader,
		AlertDialogTitle,
		AlertDialogTrigger
	} from '$lib/components/ui/alert-dialog';
	import { IconTrash } from '@tabler/icons-svelte';

	let { sessionKey, ondelete }: { sessionKey: string; ondelete?: () => void } = $props();

	let deleting = $state(false);
	let error = $state<string | null>(null);

	async function confirm() {
		if (deleting) return;
		deleting = true;
		error = null;
		try {
			ondelete?.(); // abort the event stream before leaving the page
			await api.deleteSession(sessionKey);
			await goto('/sessions');
		} catch (err) {
			error = err instanceof RelayError ? err.message : 'Delete failed';
		} finally {
			deleting = false;
		}
	}
</script>

<AlertDialog>
	<AlertDialogTrigger>
		{#snippet child({ props })}
			<Button {...props} variant="destructive" size="sm">
				<IconTrash size={14} stroke={1.75} aria-hidden="true" />
				Delete session
			</Button>
		{/snippet}
	</AlertDialogTrigger>
	<AlertDialogContent>
		<AlertDialogHeader>
			<AlertDialogTitle>Delete session {sessionKey}?</AlertDialogTitle>
			<AlertDialogDescription>
				This permanently deletes the durable RelaySession
				<span class="font-mono">{sessionKey}</span> — its transcript and
				metadata are removed. This is not a stop; it cannot be undone.
			</AlertDialogDescription>
		</AlertDialogHeader>
		{#if error}
			<p class="text-xs text-destructive" role="alert">{error}</p>
		{/if}
		<AlertDialogFooter>
			<AlertDialogCancel>Cancel</AlertDialogCancel>
			<AlertDialogAction onclick={confirm} disabled={deleting}>
				{deleting ? 'Deleting…' : 'Delete session'}
			</AlertDialogAction>
		</AlertDialogFooter>
	</AlertDialogContent>
</AlertDialog>
