<script lang="ts">
	// First-run admin setup. Credentials go to POST /auth/setup as a
	// urlencoded form with the one-time form token — nothing is stored in
	// browser storage, and the password fields are cleared on failure.
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
	let confirm = $state('');
	let busy = $state(false);
	let error = $state<string | null>(null);

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		if (busy) return;
		error = null;
		if (password !== confirm) {
			error = 'Passwords do not match';
			return;
		}
		busy = true;
		try {
			await auth.setup(username, password, confirm);
		} catch (err) {
			error = err instanceof RelayError ? err.message : 'Setup failed';
			password = '';
			confirm = '';
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
			<CardTitle class="text-lg">Create administrator</CardTitle>
			<CardDescription>
				First-run setup — choose the username and password for the single Web Admin
				account. The password is stored only as an Argon2id hash.
			</CardDescription>
		</CardHeader>
		<CardContent>
			<form onsubmit={submit} class="flex flex-col gap-4">
				{#if error}
					<Alert variant="destructive">
						<IconAlertTriangle size={16} aria-hidden="true" />
						<AlertTitle>Setup failed</AlertTitle>
						<AlertDescription>{error}</AlertDescription>
					</Alert>
				{/if}
				<div class="grid gap-2">
					<Label for="setup-username">Username</Label>
					<Input
						id="setup-username"
						name="username"
						autocomplete="username"
						required
						bind:value={username}
						disabled={busy}
					/>
				</div>
				<div class="grid gap-2">
					<Label for="setup-password">Password</Label>
					<Input
						id="setup-password"
						name="password"
						type="password"
						autocomplete="new-password"
						required
						bind:value={password}
						disabled={busy}
					/>
				</div>
				<div class="grid gap-2">
					<Label for="setup-confirm">Confirm password</Label>
					<Input
						id="setup-confirm"
						name="confirm"
						type="password"
						autocomplete="new-password"
						required
						bind:value={confirm}
						disabled={busy}
					/>
				</div>
				<Button type="submit" disabled={busy} class="w-full">
					{busy ? 'Creating…' : 'Create admin'}
				</Button>
			</form>
		</CardContent>
	</Card>
</div>
