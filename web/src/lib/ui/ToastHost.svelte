<script lang="ts">
	import { CircleAlert, CircleCheck, Info, X } from 'lucide-svelte';
	import { toasts } from '$lib/toast.svelte';
</script>

{#if toasts.list.length}
	<div class="fixed bottom-4 left-1/2 z-50 flex -translate-x-1/2 flex-col items-center gap-2">
		{#each toasts.list as t (t.id)}
			<div
				class="flex items-center gap-2.5 rounded-md border border-slate-700 bg-slate-800 px-4 py-2 text-sm text-slate-100 shadow-lg"
			>
				{#if t.kind === 'success'}<CircleCheck size={15} class="shrink-0 text-emerald-400" />
				{:else if t.kind === 'error'}<CircleAlert size={15} class="shrink-0 text-red-400" />
				{:else}<Info size={15} class="shrink-0 text-slate-400" />{/if}
				{t.msg}
				<button
					onclick={() => toasts.dismiss(t.id)}
					aria-label="Dismiss"
					class="shrink-0 text-slate-400 hover:text-slate-100"><X size={14} /></button
				>
			</div>
		{/each}
	</div>
{/if}
