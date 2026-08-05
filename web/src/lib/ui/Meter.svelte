<script lang="ts">
	// Capacity meter: the fill carries severity, the track is a lighter
	// step of the same hue so state reads across the whole bar.
	let { used, total }: { used: number; total: number } = $props();

	const pct = $derived(total > 0 ? Math.min(100, (used / total) * 100) : 0);
	const tone = $derived(pct >= 95 ? 'bad' : pct >= 80 ? 'warn' : 'ok');
	const fill: Record<string, string> = {
		ok: 'bg-sky-500',
		warn: 'bg-amber-500',
		bad: 'bg-red-500',
	};
	const track: Record<string, string> = {
		ok: 'bg-sky-500/15',
		warn: 'bg-amber-500/15',
		bad: 'bg-red-500/15',
	};
</script>

<div class="flex items-center gap-2">
	<div class="h-2 flex-1 overflow-hidden rounded-full {track[tone]}">
		<div class="h-full rounded-full {fill[tone]}" style="width: {pct}%"></div>
	</div>
	<span class="w-10 text-right text-xs tabular-nums text-slate-400">{pct.toFixed(0)}%</span>
</div>
