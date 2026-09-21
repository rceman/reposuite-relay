<script lang="ts">
	// A status pill that never relies on color alone: every state carries
	// a text label and a distinct icon, color is supplementary.
	import { Badge } from '$lib/components/ui/badge';
	import type { StatusPresentation } from '$lib/domain/status';

	let { status }: { status: StatusPresentation } = $props();
</script>

<Badge variant={status.variant} title={status.label}>
	{@const Icon = status.icon}
	<Icon size={12} stroke={2} aria-hidden="true" />
	{status.label}
</Badge>
