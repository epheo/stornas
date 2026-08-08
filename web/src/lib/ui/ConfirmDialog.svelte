<script lang="ts">
	import type { Snippet } from 'svelte';
	import Modal from './Modal.svelte';

	// Danger confirm for quick destructive actions. Actions that destroy data
	// go through ConfirmDelete's type-to-confirm gate instead.
	let {
		title,
		confirmLabel = 'Delete',
		busy = false,
		error = '',
		onconfirm,
		onclose,
		children,
	}: {
		title: string;
		confirmLabel?: string;
		busy?: boolean;
		error?: string;
		onconfirm: () => void;
		onclose: () => void;
		children?: Snippet;
	} = $props();
</script>

<Modal {title} danger {onclose}>
	<div class="px-5 py-4 text-sm text-slate-300">
		{@render children?.()}
		{#if error}<p class="mt-2 text-xs text-red-400">{error}</p>{/if}
	</div>
	{#snippet footer()}
		<button
			data-autofocus
			onclick={onclose}
			class="ml-auto rounded border border-slate-700 px-3 py-1 text-sm text-slate-300 hover:bg-slate-800"
		>
			Cancel
		</button>
		<button
			onclick={onconfirm}
			disabled={busy}
			class="rounded bg-red-600 px-3 py-1 text-sm font-medium text-white hover:bg-red-500 disabled:opacity-50"
		>
			{confirmLabel}
		</button>
	{/snippet}
</Modal>
