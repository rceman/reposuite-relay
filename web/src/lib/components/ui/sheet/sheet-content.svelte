<script lang="ts">
	import { Dialog as SheetPrimitive } from "bits-ui";
	import { cn } from "$lib/utils.js";
	import type { Snippet } from "svelte";
	import type { WithoutChildrenOrChild } from "$lib/utils.js";

	let {
		ref = $bindable(null),
		class: className,
		side = "right",
		children,
		...restProps
	}: WithoutChildrenOrChild<SheetPrimitive.ContentProps> & {
		side?: "top" | "right" | "bottom" | "left";
		children?: Snippet;
	} = $props();
</script>

<SheetPrimitive.Portal>
	<SheetPrimitive.Overlay
		class="data-open:animate-in data-open:fade-in-0 data-closed:animate-out data-closed:fade-out-0 fixed inset-0 z-50 bg-black/50"
	/>
	<SheetPrimitive.Content
		bind:ref
		data-slot="sheet-content"
		class={cn(
			"bg-background data-open:animate-in data-open:duration-300 data-closed:animate-out data-closed:duration-300 fixed z-50 flex flex-col gap-4 shadow-lg",
			side === "right" &&
				"data-open:slide-in-from-right data-closed:slide-out-to-right inset-y-0 right-0 h-full w-3/4 border-l sm:max-w-md",
			className
		)}
		{...restProps}
	>
		{@render children?.()}
	</SheetPrimitive.Content>
</SheetPrimitive.Portal>
