<script lang="ts">
	// Settings → Projects: CRUD for the Relay-local presentation catalog.
	// The server is the only authority — after EVERY mutation settle
	// (success OR error) the canonical catalog and session projection are
	// reloaded, because a post-commit 500 is not a rollback. Mutation and
	// load errors are separate channels. The root field is sent exactly
	// as typed: whitespace is part of a filesystem path.
	import AuthGate from '$lib/components/AuthGate.svelte';
	import { api } from '$lib/api/client';
	import type { ProjectInfo } from '$lib/api/types';
	import { sessionCountByProject } from '$lib/projects/group';
	import { buildCreateBody, buildPatchBody, rootLooksAbsolute } from '$lib/projects/form';
	import { ProjectsSettings } from '$lib/projects/settings.svelte';
	import { auth } from '$lib/auth/auth.svelte';
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
	import {
		Table,
		TableBody,
		TableCell,
		TableHead,
		TableHeader,
		TableRow
	} from '$lib/components/ui/table';
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
	import { Alert, AlertDescription, AlertTitle } from '$lib/components/ui/alert';
	import { MachineTokenPanel } from '$lib/settings/machine.svelte';
	import { onDestroy } from 'svelte';
	import {
		IconAlertTriangle,
		IconCheck,
		IconCopy,
		IconEye,
		IconEyeOff,
		IconKey,
		IconPencil,
		IconPlus,
		IconTrash
	} from '@tabler/icons-svelte';

	const ctl = new ProjectsSettings(api);
	// The revealed machine token lives only here — component memory,
	// cleared on dismiss, navigation (onDestroy), and logout.
	const tokenPanel = new MachineTokenPanel(api);

	// One form serves create and edit: when editingId is set the submit
	// PATCHes only the fields that changed; otherwise it POSTs.
	let editingId = $state<string | null>(null);
	let name = $state('');
	let root = $state('');
	let busy = $state(false);

	const counts = $derived(sessionCountByProject(ctl.sessions));
	const nameOk = $derived(name.trim() !== '');
	// Exact input: a leading space makes this false rather than being
	// silently trimmed into a different path.
	const rootOk = $derived(rootLooksAbsolute(root));
	const editing = $derived(ctl.projects.find((p) => p.id === editingId));

	$effect(() => {
		if (auth.authenticated) {
			void ctl.load();
			void tokenPanel.status();
		} else {
			// Unauthenticated — drop the entire reveal state.
			tokenPanel.clearReveal();
		}
	});
	// Navigation away destroys the component — clear secret state.
	onDestroy(() => tokenPanel.clearReveal());

	/**
	 * rotateToken is the page's single rotation entry point: the panel
	 * discards ALL previous reveal state (token, durability flag, mask,
	 * copy feedback) BEFORE issuing the request — a stale credential
	 * can never remain displayed through an ambiguous second rotation.
	 * One click → exactly one API call, never retried.
	 */
	async function rotateToken() {
		await tokenPanel.rotate();
	}

	function startEdit(p: ProjectInfo) {
		editingId = p.id;
		name = p.name;
		root = p.root;
		ctl.mutationError = null;
	}

	function resetForm() {
		editingId = null;
		name = '';
		root = '';
	}

	async function submit() {
		if (busy || !nameOk || !rootOk) return;
		busy = true;
		try {
			const ok = await ctl.mutate(async () => {
				if (editingId === null) {
					// Root verbatim — whitespace is part of the path.
					await api.createProject(buildCreateBody(name, root));
				} else if (editing) {
					const body = buildPatchBody(editing, name, root);
					if (body !== null) await api.patchProject(editingId, body);
				}
			});
			// Only a clean success resets the form; a failed (possibly
			// post-commit) mutation keeps the entered values and the error
			// while the reconciled canonical list already reflects reality.
			if (ok) resetForm();
		} finally {
			busy = false;
		}
	}

	async function removeProject(id: string) {
		if (busy) return;
		busy = true;
		try {
			await ctl.mutate(() => api.deleteProject(id));
		} finally {
			busy = false;
		}
	}
</script>

<AuthGate onrefresh={() => ctl.load()}>
	<h1 class="mb-4 text-lg font-semibold">Settings</h1>

	{#if ctl.loadError}
		<Alert variant="destructive" class="mb-4">
			<IconAlertTriangle size={16} aria-hidden="true" />
			<AlertTitle>Load failed</AlertTitle>
			<AlertDescription>{ctl.loadError}</AlertDescription>
		</Alert>
	{/if}

	<Card class="mb-6">
		<CardHeader>
			<CardTitle class="text-sm">{editingId ? 'Edit project' : 'Add project'}</CardTitle>
			<CardDescription>
				A project groups sessions whose working directory is at or under its root.
				The most specific matching root wins; editing never touches sessions.
			</CardDescription>
		</CardHeader>
		<CardContent>
			<form
				class="flex flex-col gap-3 sm:flex-row sm:items-end"
				onsubmit={(e) => {
					e.preventDefault();
					void submit();
				}}
			>
				<div class="flex-1">
					<Label for="project-name">Name</Label>
					<Input
						id="project-name"
						bind:value={name}
						class="mt-1"
						placeholder="My project"
						autocomplete="off"
					/>
				</div>
				<div class="flex-[2]">
					<Label for="project-root">Root</Label>
					<Input
						id="project-root"
						bind:value={root}
						class="mt-1 font-mono"
						placeholder="/absolute/path"
						autocomplete="off"
					/>
				</div>
				<div class="flex gap-2">
					<Button type="submit" disabled={busy || !nameOk || !rootOk}>
						{#if editingId === null}
							<IconPlus size={14} stroke={1.75} aria-hidden="true" />
							{busy ? 'Adding…' : 'Add project'}
						{:else}
							{busy ? 'Saving…' : 'Save'}
						{/if}
					</Button>
					{#if editingId !== null}
						<Button type="button" variant="ghost" onclick={resetForm}>Cancel</Button>
					{/if}
				</div>
			</form>
			{#if ctl.mutationError}
				<p class="mt-2 text-xs text-destructive" role="alert">{ctl.mutationError}</p>
			{/if}
		</CardContent>
	</Card>

	<Card>
		<CardHeader><CardTitle class="text-sm">Projects</CardTitle></CardHeader>
		<CardContent class="p-0">
			<Table>
				<TableHeader>
					<TableRow>
						<TableHead>Name</TableHead>
						<TableHead>Root</TableHead>
						<TableHead class="text-right">Sessions</TableHead>
						<TableHead class="w-28"></TableHead>
					</TableRow>
				</TableHeader>
				<TableBody>
					{#each ctl.projects as p (p.id)}
						<TableRow>
							<TableCell class="max-w-40 truncate font-medium" title={p.name}
								>{p.name}</TableCell
							>
							<TableCell
								class="max-w-64 truncate font-mono text-xs text-muted-foreground"
								title={p.root}>{p.root}</TableCell
							>
							<TableCell class="text-right tabular-nums"
								>{counts.get(p.id) ?? 0}</TableCell
							>
							<TableCell>
								<div class="flex justify-end gap-1">
									<Button variant="ghost" size="icon-sm" onclick={() => startEdit(p)} title="Edit project">
										<IconPencil size={14} stroke={1.75} aria-hidden="true" />
									</Button>
									<AlertDialog>
										<AlertDialogTrigger>
											{#snippet child({ props })}
												<Button {...props} variant="ghost" size="icon-sm" title="Delete project">
													<IconTrash size={14} stroke={1.75} aria-hidden="true" />
												</Button>
											{/snippet}
										</AlertDialogTrigger>
										<AlertDialogContent>
											<AlertDialogHeader>
												<AlertDialogTitle>Delete project {p.name}?</AlertDialogTitle>
												<AlertDialogDescription>
													This removes the grouping for
													<span class="font-mono">{p.root}</span>. Sessions are not
													deleted or stopped — they become Ungrouped or match
													another project.
												</AlertDialogDescription>
											</AlertDialogHeader>
											<AlertDialogFooter>
												<AlertDialogCancel>Cancel</AlertDialogCancel>
												<AlertDialogAction
													onclick={() => removeProject(p.id)}
													disabled={busy}
												>
													{busy ? 'Deleting…' : 'Delete project'}
												</AlertDialogAction>
											</AlertDialogFooter>
										</AlertDialogContent>
									</AlertDialog>
								</div>
							</TableCell>
						</TableRow>
					{:else}
						<TableRow>
							<TableCell colspan={4} class="py-8 text-center text-sm text-muted-foreground">
								No projects yet — add one above to group sessions by directory.
							</TableCell>
						</TableRow>
					{/each}
				</TableBody>
			</Table>
		</CardContent>
	</Card>

	<Card class="mt-6">
		<CardHeader>
			<CardTitle class="text-sm">Machine API access</CardTitle>
			<CardDescription>
				The machine token authenticates trusted machine integrations against the
				/v1 API. Rotating invalidates the old token immediately for new requests —
				browser and descriptor credentials are unaffected. The current token
				cannot be revealed; rotate to obtain a new value.
			</CardDescription>
		</CardHeader>
		<CardContent>
			<div class="flex items-center gap-3">
				<span class="text-sm text-muted-foreground">
					Status: {tokenPanel.configured === null
						? '—'
						: tokenPanel.configured
							? 'Configured'
							: 'Not configured'}
				</span>
				<AlertDialog>
					<AlertDialogTrigger>
						{#snippet child({ props })}
							<Button {...props} variant="outline" size="sm" disabled={tokenPanel.busy}>
								<IconKey size={14} stroke={1.75} aria-hidden="true" />
								Rotate token
							</Button>
						{/snippet}
					</AlertDialogTrigger>
					<AlertDialogContent>
						<AlertDialogHeader>
							<AlertDialogTitle>Rotate machine API token?</AlertDialogTitle>
							<AlertDialogDescription>
								Existing machine clients using the old token will stop
								authenticating immediately. Your browser session and this
								daemon's descriptor credentials are unaffected. The new token is
								shown once — copy it before dismissing.
							</AlertDialogDescription>
						</AlertDialogHeader>
						<AlertDialogFooter>
							<AlertDialogCancel>Cancel</AlertDialogCancel>
							<AlertDialogAction
								onclick={() => void rotateToken()}
								disabled={tokenPanel.busy}
							>
								{tokenPanel.busy ? 'Rotating…' : 'Rotate token'}
							</AlertDialogAction>
						</AlertDialogFooter>
					</AlertDialogContent>
				</AlertDialog>
			</div>

			{#if tokenPanel.error}
				<Alert variant="destructive" class="mt-4">
					<IconAlertTriangle size={16} aria-hidden="true" />
					<AlertTitle>Rotation failed</AlertTitle>
					<AlertDescription>{tokenPanel.error}</AlertDescription>
				</Alert>
			{/if}
			{#if tokenPanel.statusError}
				<p class="mt-3 text-xs text-muted-foreground" role="status">
					{tokenPanel.statusError}
				</p>
			{/if}

			{#if tokenPanel.token !== null}
				{#if !tokenPanel.durabilityConfirmed}
					<Alert class="mt-4">
						<IconAlertTriangle size={16} aria-hidden="true" />
						<AlertTitle>Token active — durability unconfirmed</AlertTitle>
						<AlertDescription>
							The new token is active, but directory durability could not be
							confirmed. Copy it now; a system crash could require recovery.
						</AlertDescription>
					</Alert>
				{/if}
				<div class="mt-4 rounded-md border p-3">
					<Label for="machine-token-value">New machine token — shown once</Label>
					<div class="mt-2 flex items-center gap-2">
						<Input
							id="machine-token-value"
							class="font-mono text-xs"
							type={tokenPanel.shown ? 'text' : 'password'}
							value={tokenPanel.token}
							readonly
						/>
						<Button
							variant="outline"
							size="icon-sm"
							title={tokenPanel.shown ? 'Hide token' : 'Show token'}
							onclick={() => (tokenPanel.shown = !tokenPanel.shown)}
						>
							{#if tokenPanel.shown}
								<IconEyeOff size={14} stroke={1.75} aria-hidden="true" />
							{:else}
								<IconEye size={14} stroke={1.75} aria-hidden="true" />
							{/if}
						</Button>
						<Button
							variant="outline"
							size="icon-sm"
							title="Copy token"
							onclick={() => void tokenPanel.copyToken()}
						>
							{#if tokenPanel.copyState === 'copied'}
								<IconCheck size={14} stroke={1.75} aria-hidden="true" />
							{:else}
								<IconCopy size={14} stroke={1.75} aria-hidden="true" />
							{/if}
						</Button>
						<Button
							variant="ghost"
							size="sm"
							onclick={() => tokenPanel.clearReveal()}
						>
							Dismiss
						</Button>
					</div>
					{#if tokenPanel.copyState === 'failed'}
						<p class="mt-2 text-xs text-destructive" role="alert">
							Copy failed — select and copy the token manually.
						</p>
					{/if}
				</div>
			{/if}
		</CardContent>
	</Card>
</AuthGate>
