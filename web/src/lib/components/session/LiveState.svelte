<script lang="ts">
	// Event-stream connection state — always text + icon, never color alone.
	import { IconAlertTriangle, IconLoader, IconPlugConnected, IconRefresh } from '@tabler/icons-svelte';
	import type { LiveState } from '$lib/session/live.svelte';

	let { state }: { state: LiveState } = $props();

	const presentation = $derived(
		{
			connecting: { label: 'Connecting', icon: IconLoader, cls: 'text-muted-foreground' },
			live: { label: 'Live', icon: IconPlugConnected, cls: 'text-emerald-600' },
			reconnecting: { label: 'Reconnecting', icon: IconRefresh, cls: 'text-amber-600' },
			offline: { label: 'Offline', icon: IconAlertTriangle, cls: 'text-destructive' }
		}[state]
	);
</script>

<span class="flex items-center gap-1.5 text-xs {presentation.cls}" role="status">
	<presentation.icon size={14} stroke={1.75} aria-hidden="true" />
	{presentation.label}
</span>
