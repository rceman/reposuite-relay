<script lang="ts">
	// Settings → Projects: CRUD for the Relay-local presentation catalog.
	// Project membership is derived from session cwd — editing a root or
	// deleting a project re-groups sessions on the next read and never
	// touches a RelaySession. No browser persistence: presentation.json
	// on the daemon is the only authority.
	import AuthGate from '$lib/components/AuthGate.svelte';
	import { api } from '$lib/api/client';
	import { RelayError } from '$lib/api/errors';
	import type { ProjectInfo, SessionInfo } from '$lib/api/types';
	import { sessionCountByProject } from '$lib/projects/group';
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
	import { IconAlertTriangle, IconPencil, IconPlus, IconTrash } from '@tabler/icons-svelte';

	let projects = $state<ProjectInfo[]>([]);
	let sessions = $state<SessionInfo[]>([]);
	let loadError = $state<string | null>(null);

	// One form serves create and edit: when editingId is set the submit
	// PATCHes only the fields that changed; otherwise it POSTs.
	let editingId = $state<string | null>(null);
	let name = $state('');
	let root = $state('');
	let busy = $state(false);
	let formError = $state<string | null>(null);
	let deleting = $state(false);

	const counts = $derived(sessionCountByProject(sessions));
	const nameOk = $derived(name.trim() !== '');
	const rootOk = $derived(root.trim().startsWith('/'));
	const editing = $derived(projects.find((p) => p.id === editingId));

	async function load() {
		try {
			const [pl, sl] = await Promise.all([api.listProjects(), api.listSessions()]);
			projects = pl.projects;
			sessions = sl.sessions;
			loadError = null;
		} catch (err) {
			loadError = err instanceof RelayError ? err.message : 'Failed to load settings';
		}
	}

	$effect(() => {
		if (auth.authenticated) void load();
	});

	function startEdit(p: ProjectInfo) {
		editingId = p.id;
		name = p.name;
		root = p.root;
		formError = null;
	}

	function resetForm() {
		editingId = null;
		name = '';
		root = '';
		formError = null;
	}

	async function submit() {
		if (busy || !nameOk || !rootOk) return;
		busy = true;
		formError = null;
		try {
			if (editingId === null) {
				await api.createProject({ name: name.trim(), root: root.trim() });
			} else {
				// PATCH only the fields that changed — never an empty patch.
				const prev = editing;
				const body: { name?: string; root?: string } = {};
				if (prev && name.trim() !== prev.name) body.name = name.trim();
				if (prev && root.trim() !== prev.root) body.root = root.trim();
				if (body.name !== undefined || body.root !== undefined) {
					await api.patchProject(editingId, body);
				}
			}
			resetForm();
			await load();
		} catch (err) {
			// Failed mutations never invent committed authority — the list
			// only reflects a fresh server load.
			formError = err instanceof RelayError ? err.message : 'Save failed';
		} finally {
			busy = false;
		}
	}

	async function removeProject(id: string) {
		if (deleting) return;
		deleting = true;
		loadError = null;
		try {
			await api.deleteProject(id);
			await load();
		} catch (err) {
			loadError = err instanceof RelayError ? err.message : 'Delete failed';
		} finally {
			deleting = false;
		}
	}
</script>

<AuthGate onrefresh={load}>
	<h1 class="mb-4 text-lg font-semibold">Settings</h1>

	{#if loadError}
		<Alert variant="destructive" class="mb-4">
			<IconAlertTriangle size={16} aria-hidden="true" />
			<AlertTitle>Settings</AlertTitle>
			<AlertDescription>{loadError}</AlertDescription>
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
			{#if formError}
				<p class="mt-2 text-xs text-destructive" role="alert">{formError}</p>
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
					{#each projects as p (p.id)}
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
													disabled={deleting}
												>
													{deleting ? 'Deleting…' : 'Delete project'}
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
</AuthGate>
