<script lang="ts">
	// One unresolved requested-input request. Options are a multi-select
	// checkbox set (the native payload has no single-select contract —
	// each question produces []string). isOther questions get a free-text
	// field; isSecret questions get a password input that is cleared on
	// submit and never persisted anywhere. The pending state reconciles
	// only through the canonical input.resolved / input.aborted events —
	// we never pretend the answer already landed.
	import { api } from '$lib/api/client';
	import { RelayError } from '$lib/api/errors';
	import type { InputRequestedPayload } from '$lib/api/types';
	import { buildAnswers, hasAnyAnswer } from '$lib/session/answers';
	import { Button } from '$lib/components/ui/button';
	import { Checkbox } from '$lib/components/ui/checkbox';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { IconAlertTriangle, IconLock } from '@tabler/icons-svelte';

	let {
		sessionKey,
		request
	}: {
		sessionKey: string;
		request: InputRequestedPayload;
	} = $props();

	// Per-question answer state: selected option labels + free text.
	let selected = $state<Record<string, string[]>>({});
	let freeText = $state<Record<string, string>>({});
	let submitting = $state(false);
	let error = $state<string | null>(null);

	function toggle(qid: string, label: string, on: boolean) {
		const cur = selected[qid] ?? [];
		selected = { ...selected, [qid]: on ? [...cur, label] : cur.filter((l) => l !== label) };
	}

	// Free text is forwarded verbatim — an isSecret value may legally
	// contain surrounding whitespace and must never be rewritten.
	const hasAnswer = $derived(hasAnyAnswer(request.questions, { selected, freeText }));

	async function submit() {
		if (!hasAnswer || submitting) return;
		submitting = true;
		error = null;
		try {
			await api.answerInput(sessionKey, {
				inputId: request.inputId,
				answers: buildAnswers(request.questions, { selected, freeText })
			});
			// Accepted — input.resolved reconciles pending state. Clear local
			// fields now so a secret never lingers in the page.
			selected = {};
			freeText = {};
		} catch (err) {
			error = err instanceof RelayError ? err.message : 'Answer failed';
		} finally {
			submitting = false;
		}
	}
</script>

<div class="rounded-lg border border-amber-500/50 bg-amber-500/5 p-4">
	<div class="mb-3 flex items-center gap-2 text-sm font-medium">
		<IconAlertTriangle size={15} stroke={1.75} class="text-amber-600" aria-hidden="true" />
		Input requested{request.isBlocking ? ' — blocking' : ''}
	</div>
	{#each request.questions as q (q.id)}
		<div class="mb-4">
			<p class="text-sm font-medium">{q.header}</p>
			<p class="mb-2 text-sm text-muted-foreground">{q.question}</p>
			{#if q.options && q.options.length > 0}
				<div class="flex flex-col gap-1.5">
					{#each q.options as opt (opt.label)}
						<Label class="flex items-center gap-2 text-sm font-normal">
							<Checkbox
								checked={selected[q.id]?.includes(opt.label) ?? false}
								onCheckedChange={(v) => toggle(q.id, opt.label, v === true)}
							/>
							<span>{opt.label}</span>
							{#if opt.description}
								<span class="text-xs text-muted-foreground">{opt.description}</span>
							{/if}
						</Label>
					{/each}
				</div>
			{/if}
			{#if q.isOther}
				<Input
					type={q.isSecret ? 'password' : 'text'}
					placeholder={q.isSecret ? 'Secret — never stored' : 'Other…'}
					class="mt-2 max-w-md"
					autocomplete="off"
					value={freeText[q.id] ?? ''}
					oninput={(e) => {
						freeText = { ...freeText, [q.id]: e.currentTarget.value };
					}}
				/>
				{#if q.isSecret}
					<p class="mt-1 flex items-center gap-1 text-[11px] text-muted-foreground">
						<IconLock size={11} stroke={1.75} aria-hidden="true" />
						Secret answers are sent to the harness but redacted from the durable transcript.
					</p>
				{/if}
			{/if}
		</div>
	{/each}
	{#if error}
		<p class="mb-2 text-xs text-destructive" role="alert">{error}</p>
	{/if}
	<Button size="sm" onclick={submit} disabled={!hasAnswer || submitting}>
		{submitting ? 'Submitting…' : 'Submit answer'}
	</Button>
</div>
