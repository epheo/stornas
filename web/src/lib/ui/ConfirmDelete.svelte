<script lang="ts">
	import type { Snippet } from 'svelte';
	import Modal from './Modal.svelte';

	// Type-to-confirm delete for actions that destroy data; the exact name
	// must be typed before Delete arms.
	let {
		title,
		confirmWord,
		busy = false,
		error = '',
		onconfirm,
		onclose,
		children,
	}: {
		title: string;
		confirmWord: string;
		busy?: boolean;
		error?: string;
		onconfirm: () => void;
		onclose: () => void;
		children?: Snippet;
	} = $props();

	let text = $state('');
	const ready = $derived(text === confirmWord && !busy);
</script>

<Modal {title} danger {onclose}>
	<div class="px-5 py-4 text-sm text-slate-300">
		{@render children?.()}
		<label for="confirm-delete-input" class="mt-3 mb-1 block text-xs text-slate-400">
			Type <span class="font-mono text-slate-200">{confirmWord}</span> to confirm:
		</label>
		<input
			id="confirm-delete-input"
			data-autofocus
			bind:value={text}
			class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 font-mono text-sm placeholder:text-slate-600 focus:border-red-500 focus:outline-none"
			placeholder={confirmWord}
		/>
		{#if error}<p class="mt-2 text-xs text-red-400">{error}</p>{/if}
	</div>
	{#snippet footer()}
		<button
			onclick={onclose}
			class="ml-auto rounded border border-slate-700 px-3 py-1 text-sm text-slate-300 hover:bg-slate-800"
		>
			Cancel
		</button>
		<button
			onclick={onconfirm}
			disabled={!ready}
			class="rounded bg-red-600 px-3 py-1 text-sm font-medium text-white hover:bg-red-500 disabled:opacity-50"
		>
			Delete
		</button>
	{/snippet}
</Modal>
