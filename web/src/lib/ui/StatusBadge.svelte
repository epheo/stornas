<script lang="ts">
	// Status is never color-alone: the dot carries state, the label names it.
	let { kind, label }: { kind: 'ok' | 'warn' | 'bad' | 'neutral'; label: string } = $props();

	const styles: Record<string, string> = {
		ok: 'bg-emerald-500/10 text-emerald-400',
		warn: 'bg-amber-500/10 text-amber-400',
		bad: 'bg-red-500/10 text-red-400',
		neutral: 'bg-slate-500/10 text-slate-400',
	};
	const dots: Record<string, string> = {
		ok: 'bg-emerald-400',
		warn: 'bg-amber-400',
		bad: 'bg-red-400',
		neutral: 'bg-slate-500',
	};
</script>

<span
	class="inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium {styles[
		kind
	]}"
>
	<span class="h-1.5 w-1.5 rounded-full {dots[kind]}"></span>
	{label}
</span>
