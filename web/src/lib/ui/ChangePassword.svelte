<script lang="ts">
	import Modal from './Modal.svelte';
	import { app } from '$lib/state.svelte';
	import { post } from '$lib/api';
	import { toasts } from '$lib/toast.svelte';

	let { onclose }: { onclose: () => void } = $props();

	let current = $state('');
	let next = $state('');
	let confirmed = $state('');
	let error = $state('');
	let busy = $state(false);

	const valid = $derived(next.length >= 8 && next === confirmed && current.length > 0);

	async function submit(e?: Event) {
		e?.preventDefault();
		if (!valid || busy) return;
		busy = true;
		error = '';
		const err = await post('/api/v1/session/password', { current, new: next });
		busy = false;
		if (err) error = err;
		else {
			app.mustChangePassword = false;
			toasts.show('Password changed', 'success');
			onclose();
		}
	}
</script>

<Modal title="Change password" {onclose}>
	<form class="space-y-2 px-5 py-4" onsubmit={submit}>
		{#if app.mustChangePassword}
			<p class="mb-2 text-sm text-amber-400">
				This account still uses the generated first-boot password; pick your own.
			</p>
		{/if}
		<input
			data-autofocus
			type="password"
			placeholder="Current password"
			autocomplete="current-password"
			bind:value={current}
			class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
		/>
		<input
			type="password"
			placeholder="New password (8+ characters)"
			autocomplete="new-password"
			bind:value={next}
			class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
		/>
		{#if next && next.length < 8}
			<p class="text-xs text-amber-400">at least 8 characters</p>
		{/if}
		<input
			type="password"
			placeholder="Repeat new password"
			autocomplete="new-password"
			bind:value={confirmed}
			class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
		/>
		{#if confirmed && next !== confirmed}
			<p class="text-xs text-amber-400">passwords do not match</p>
		{/if}
		{#if error}<p class="text-xs text-red-400">{error}</p>{/if}
	</form>
	{#snippet footer()}
		<button
			onclick={onclose}
			class="ml-auto rounded border border-slate-700 px-3 py-1 text-sm text-slate-300 hover:bg-slate-800"
		>
			Cancel
		</button>
		<button
			onclick={() => submit()}
			disabled={!valid || busy}
			class="rounded bg-sky-600 px-3 py-1 text-sm font-medium text-white hover:bg-sky-500 disabled:opacity-50"
		>
			Change password
		</button>
	{/snippet}
</Modal>
