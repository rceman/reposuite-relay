<script lang="ts">
	// Authenticated application frame: branding, primary navigation,
	// daemon connection state, signed-in identity, refresh, sign out.
	import { page } from '$app/state';
	import { auth } from '$lib/auth/auth.svelte';
	import { Button } from '$lib/components/ui/button';
	import { Separator } from '$lib/components/ui/separator';
	import {
		IconLayoutDashboard,
		IconLogout,
		IconRefresh,
		IconRobot,
		IconServer,
		IconSettings
	} from '@tabler/icons-svelte';
	import type { Snippet } from 'svelte';

	let {
		connected,
		children,
		onrefresh
	}: {
		connected: boolean;
		children: Snippet;
		onrefresh?: (() => void) | undefined;
	} = $props();

	let signingOut = $state(false);

	async function logout() {
		if (signingOut) return;
		signingOut = true;
		try {
			await auth.logout();
		} finally {
			signingOut = false;
		}
	}

	const nav = [
		{ href: '/', label: 'Overview', icon: IconLayoutDashboard },
		{ href: '/sessions', label: 'Sessions', icon: IconRobot },
		{ href: '/settings', label: 'Settings', icon: IconSettings }
	];
</script>

<div class="flex min-h-screen flex-col bg-background text-foreground">
	<header class="border-b">
		<div class="mx-auto flex h-12 w-full max-w-6xl items-center gap-4 px-4">
			<a href="/" class="flex items-center gap-2 font-semibold">
				<IconServer size={18} stroke={1.75} aria-hidden="true" />
				<span>RepoSuite Relay</span>
			</a>
			<Separator orientation="vertical" class="h-5" />
			<nav class="flex items-center gap-1">
				{#each nav as item (item.href)}
					{@const active = page.url.pathname === item.href}
					<Button
						href={item.href}
						variant={active ? 'secondary' : 'ghost'}
						size="sm"
						aria-current={active ? 'page' : undefined}
					>
						<item.icon size={15} stroke={1.75} aria-hidden="true" />
						{item.label}
					</Button>
				{/each}
			</nav>
			<div class="ml-auto flex items-center gap-3">
				<span
					class="flex items-center gap-1.5 text-xs text-muted-foreground"
					title={connected ? 'Connected to the daemon' : 'Daemon unreachable'}
				>
					<span
						class="inline-block size-2 rounded-full {connected
							? 'bg-emerald-600'
							: 'bg-muted-foreground'}"
						aria-hidden="true"
					></span>
					{connected ? 'Connected' : 'Disconnected'}
				</span>
				{#if auth.state?.username}
					<span class="text-xs text-muted-foreground">{auth.state.username}</span>
				{/if}
				{#if onrefresh}
					<Button variant="ghost" size="icon-sm" onclick={onrefresh} title="Refresh">
						<IconRefresh size={15} stroke={1.75} aria-hidden="true" />
					</Button>
				{/if}
				<Button variant="ghost" size="sm" onclick={logout} disabled={signingOut}>
					<IconLogout size={15} stroke={1.75} aria-hidden="true" />
					{signingOut ? 'Signing out…' : 'Sign out'}
				</Button>
			</div>
		</div>
	</header>
	<main class="mx-auto w-full max-w-6xl flex-1 px-4 py-6">
		{@render children()}
	</main>
</div>
