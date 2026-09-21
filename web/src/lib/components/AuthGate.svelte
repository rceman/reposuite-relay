<script lang="ts">
	// The single auth gate every UI route renders through: bootstrap
	// /auth/session once, then choose between first-run setup, login, and
	// the authenticated shell. The session cookie itself never touches JS.
	import { auth } from '$lib/auth/auth.svelte';
	import AppShell from './AppShell.svelte';
	import LoginForm from './LoginForm.svelte';
	import SetupForm from './SetupForm.svelte';
	import { Alert, AlertDescription, AlertTitle } from '$lib/components/ui/alert';
	import { Button } from '$lib/components/ui/button';
	import { IconAlertTriangle } from '@tabler/icons-svelte';
	import type { Snippet } from 'svelte';
	import { onMount } from 'svelte';

	let { children, onrefresh }: { children: Snippet; onrefresh?: (() => void) | undefined } =
		$props();

	onMount(() => {
		if (auth.state === null) void auth.refresh();
	});
</script>

{#if auth.loading}
	<div class="flex min-h-screen items-center justify-center bg-background">
		<p class="text-sm text-muted-foreground">Connecting to Relay…</p>
	</div>
{:else if auth.state === null}
	<div class="flex min-h-screen items-center justify-center bg-background p-6">
		<Alert variant="destructive" class="max-w-md">
			<IconAlertTriangle size={16} aria-hidden="true" />
			<AlertTitle>Daemon unreachable</AlertTitle>
			<AlertDescription>
				{auth.bootError ?? 'The Relay daemon did not answer.'}
				<Button variant="outline" size="sm" class="mt-3" onclick={() => auth.refresh()}>
					Retry
				</Button>
			</AlertDescription>
		</Alert>
	</div>
{:else if !auth.state.configured}
	<SetupForm />
{:else if !auth.state.authenticated}
	<LoginForm />
{:else}
	<AppShell connected={auth.bootError === null} {onrefresh}>
		{@render children()}
	</AppShell>
{/if}
