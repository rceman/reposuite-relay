<script lang="ts">
	// Create session: POST /v1/sessions/{provider}. The key constraint and
	// absolute-cwd check mirror the backend for UX only — the daemon is
	// authoritative. Success lands on the new session's detail page.
	import AuthGate from '$lib/components/AuthGate.svelte';
	import { api } from '$lib/api/client';
	import { RelayError } from '$lib/api/errors';
	import { goto } from '$app/navigation';
	import { Button } from '$lib/components/ui/button';
	import { Card, CardContent, CardHeader, CardTitle } from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import {
		Select,
		SelectContent,
		SelectItem,
		SelectTrigger
	} from '$lib/components/ui/select';
	import { IconArrowLeft } from '@tabler/icons-svelte';
	import type { ProjectInfo } from '$lib/api/types';
	import { projectRootFor } from '$lib/projects/group';
	import { auth } from '$lib/auth/auth.svelte';

	const PROVIDERS = [
		{ value: 'codex', label: 'Codex' },
		{ value: 'devin', label: 'Devin' },
		{ value: 'opencode', label: 'OpenCode' }
	] as const;
	const KEY_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

	let provider = $state<string>('codex');
	let key = $state('');
	let cwd = $state('');
	let model = $state('');
	let mode = $state('');
	let busy = $state(false);
	let error = $state<string | null>(null);
	let projects = $state<ProjectInfo[]>([]);
	let projectId = $state<string>('');

	// The project catalog is a convenience only — it presets cwd. The
	// request still carries cwd alone; grouping stays derived.
	$effect(() => {
		if (auth.authenticated) {
			api.listProjects()
				.then((pl) => (projects = pl.projects))
				.catch(() => {});
		}
	});

	function onProjectChange(id: string) {
		projectId = id;
		// Selecting a project presets cwd once — a later hand edit is
		// never forced back to the root.
		const r = projectRootFor(id, projects);
		if (r !== undefined) cwd = r;
	}

	const keyOk = $derived(KEY_RE.test(key));
	const cwdOk = $derived(cwd.startsWith('/') && cwd.length > 1);
	const canSubmit = $derived(keyOk && cwdOk && !busy);

	async function submit() {
		if (!canSubmit) return;
		busy = true;
		error = null;
		try {
			const body: { key: string; cwd: string; model?: string; mode?: string } = {
				key,
				cwd
			};
			const m = model.trim();
			const md = mode.trim();
			if (m !== '') body.model = m;
			if (md !== '') body.mode = md;
			const resp = await api.createSession(provider, body);
			await goto(`/sessions/${encodeURIComponent(resp.session.key)}`);
		} catch (err) {
			error = err instanceof RelayError ? err.message : 'Create failed';
		} finally {
			busy = false;
		}
	}
</script>

<AuthGate>
	<div class="mx-auto max-w-xl">
		<Button href="/sessions" variant="ghost" size="sm" class="mb-4 -ml-2">
			<IconArrowLeft size={14} stroke={1.75} aria-hidden="true" />
			Sessions
		</Button>
		<Card>
			<CardHeader>
				<CardTitle>New session</CardTitle>
			</CardHeader>
			<CardContent>
				<form
					class="flex flex-col gap-4"
					onsubmit={(e) => {
						e.preventDefault();
						void submit();
					}}
				>
					<div>
						<Label for="new-provider">Provider</Label>
						<Select type="single" bind:value={provider}>
							<SelectTrigger id="new-provider" class="mt-1 w-full">
								{PROVIDERS.find((p) => p.value === provider)?.label ?? provider}
							</SelectTrigger>
							<SelectContent>
								{#each PROVIDERS as p (p.value)}
									<SelectItem value={p.value} label={p.label}>{p.label}</SelectItem>
								{/each}
							</SelectContent>
						</Select>
					</div>
					<div>
						<Label for="new-key">Key</Label>
						<Input
							id="new-key"
							bind:value={key}
							class="mt-1 font-mono"
							placeholder="my-session"
							autocomplete="off"
						/>
						{#if key !== '' && !keyOk}
							<p class="mt-1 text-xs text-destructive">
								Letters, digits, dot, dash, underscore — max 64 chars, starting alphanumeric.
							</p>
						{/if}
					</div>
					{#if projects.length > 0}
						<div>
							<Label for="new-project">Project <span class="text-muted-foreground">(optional)</span></Label>
							<Select type="single" bind:value={projectId} onValueChange={onProjectChange}>
								<SelectTrigger id="new-project" class="mt-1 w-full">
									{projects.find((p) => p.id === projectId)?.name ?? 'Select a project…'}
								</SelectTrigger>
								<SelectContent>
									{#each projects as p (p.id)}
										<SelectItem value={p.id} label={p.name}>{p.name}</SelectItem>
									{/each}
								</SelectContent>
							</Select>
							<p class="mt-1 text-xs text-muted-foreground">
								Picking a project presets the working directory to its root. Grouping is
								derived from the directory itself — you can still edit it below.
							</p>
						</div>
					{/if}
					<div>
						<Label for="new-cwd">Working directory</Label>
						<Input
							id="new-cwd"
							bind:value={cwd}
							class="mt-1 font-mono"
							placeholder="/absolute/path"
							autocomplete="off"
						/>
						{#if cwd !== '' && !cwdOk}
							<p class="mt-1 text-xs text-destructive">Must be an absolute path.</p>
						{/if}
					</div>
					<div class="grid grid-cols-2 gap-3">
						<div>
							<Label for="new-model">Model <span class="text-muted-foreground">(optional)</span></Label>
							<Input id="new-model" bind:value={model} class="mt-1 font-mono" autocomplete="off" />
						</div>
						<div>
							<Label for="new-mode">Mode <span class="text-muted-foreground">(optional)</span></Label>
							<Input id="new-mode" bind:value={mode} class="mt-1 font-mono" autocomplete="off" />
						</div>
					</div>
					{#if error}
						<p class="text-xs text-destructive" role="alert">{error}</p>
					{/if}
					<p class="text-xs text-muted-foreground">
						A new session is durable but cold — no harness process starts until the first
						prompt.
					</p>
					<Button type="submit" disabled={!canSubmit}>
						{busy ? 'Creating…' : 'Create session'}
					</Button>
				</form>
			</CardContent>
		</Card>
	</div>
</AuthGate>
