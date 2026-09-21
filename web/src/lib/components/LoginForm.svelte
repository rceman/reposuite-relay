<script lang="ts">
	// Credential login. The server owns Argon2id verification and
	// throttling — this form only submits and renders the outcome,
	// including Retry-After when the server throttles.
	import { auth } from '$lib/auth/auth.svelte';
	import { RelayError } from '$lib/api/errors';
	import { Button } from '$lib/components/ui/button';
	import {
		Card,
		CardContent,
		CardDescription,
		CardHeader,
		CardTitle
	} from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Alert, AlertDescription, AlertTitle } from '$lib/components/ui/alert';
	import { IconAlertTriangle, IconServer } from '@tabler/icons-svelte';

	let username = $state('');
	let password = $state('');
	let busy = $state(false);
	let error = $state<string | null>(null);
	let retryAfter = $state<number | null>(null);

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		if (busy) return;
		error = null;
		retryAfter = null;
		busy = true;
		try {
			await auth.login(username, password);
		} catch (err) {
			if (err instanceof RelayError) {
				error = err.message;
				retryAfter = err.retryAfter ?? null;
			} else {
				error = 'Sign-in failed';
			}
			password = '';
		} finally {
			busy = false;
		}
	}
</script>

<div class="flex min-h-screen items-center justify-center bg-background p-6">
	<Card class="w-full max-w-sm">
		<CardHeader>
			<div class="flex items-center gap-2 text-muted-foreground">
				<IconServer size={18} stroke={1.5} aria-hidden="true" />
				<span class="text-xs font-medium tracking-wide uppercase">RepoSuite Relay</span>
			</div>
			<CardTitle class="text-lg">Sign in</CardTitle>
			<CardDescription>Local Web Admin for this Relay daemon.</CardDescription>
		</CardHeader>
		<CardContent>
			<form onsubmit={submit} class="flex flex-col gap-4">
				{#if error}
					<Alert variant="destructive">
						<IconAlertTriangle size={16} aria-hidden="true" />
						<AlertTitle>Sign-in failed</AlertTitle>
						<AlertDescription>
							{error}
							{#if retryAfter !== null}
								<span class="mt-1 block text-xs">Try again in ~{retryAfter}s.</span>
							{/if}
						</AlertDescription>
					</Alert>
				{/if}
				<div class="grid gap-2">
					<Label for="login-username">Username</Label>
					<Input
						id="login-username"
						name="username"
						autocomplete="username"
						required
						bind:value={username}
						disabled={busy}
					/>
				</div>
				<div class="grid gap-2">
					<Label for="login-password">Password</Label>
					<Input
						id="login-password"
						name="password"
						type="password"
						autocomplete="current-password"
						required
						bind:value={password}
						disabled={busy}
					/>
				</div>
				<Button type="submit" disabled={busy} class="w-full">
					{busy ? 'Signing in…' : 'Sign in'}
				</Button>
			</form>
		</CardContent>
	</Card>
</div>
