<script lang="ts">
	// Rebuild (pools) and resync (volumes) share this exact fill-bar look.
	let { percent, width = 'w-56' }: { percent: number; width?: string } = $props();
</script>

<div class="h-1.5 {width} overflow-hidden rounded-full bg-sky-500/15">
	<div class="h-full rounded-full bg-sky-500" style="width:{percent}%"></div>
</div>
