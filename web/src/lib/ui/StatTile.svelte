<script lang="ts">
	// lucide-svelte still exports legacy component classes; ComponentType
	// is the type that accepts them.
	import type { ComponentType } from 'svelte';

	let {
		label,
		value,
		note = '',
		noteTone = 'neutral',
		icon: Icon,
		href,
	}: {
		label: string;
		value: string | number;
		note?: string;
		noteTone?: 'ok' | 'warn' | 'bad' | 'neutral';
		icon: ComponentType;
		href: string;
	} = $props();

	const noteColor: Record<string, string> = {
		ok: 'text-emerald-400',
		warn: 'text-amber-400',
		bad: 'text-red-400',
		neutral: 'text-slate-500',
	};
</script>

<a
	{href}
	class="block rounded-lg border border-slate-800 bg-slate-900 p-4 transition-colors hover:border-slate-700"
>
	<div class="flex items-center gap-2 text-xs text-slate-400">
		<Icon size={14} />
		{label}
	</div>
	<div class="mt-1 text-2xl font-semibold text-slate-100">{value}</div>
	{#if note}
		<div class="mt-0.5 text-xs {noteColor[noteTone]}">{note}</div>
	{/if}
</a>
