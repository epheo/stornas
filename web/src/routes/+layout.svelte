<script lang="ts">
	import '../app.css';
	import { onMount } from 'svelte';
	import { HardDrive } from 'lucide-svelte';

	let { children } = $props();

	let gate = $state<'loading' | 'anon' | 'in'>('loading');
	let username = $state('');
	let password = $state('');
	let error = $state('');

	onMount(async () => {
		const r = await fetch('/api/v1/session').catch(() => undefined);
		gate = r?.ok ? 'in' : 'anon';
	});

	async function login(e: Event) {
		e.preventDefault();
		error = '';
		const r = await fetch('/api/v1/login', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ username, password }),
		}).catch(() => undefined);
		if (r?.ok) {
			gate = 'in';
		} else {
			error = 'Invalid credentials';
			password = '';
		}
	}
</script>

{#if gate === 'in'}
	{@render children()}
{:else if gate === 'anon'}
	<main class="flex min-h-screen items-center justify-center">
		<form onsubmit={login} class="w-72 space-y-4 rounded border border-gray-200 p-6">
			<div class="flex items-center justify-center gap-2">
				<HardDrive size={20} />
				<span class="text-lg font-semibold">stornas</span>
			</div>
			<input
				class="w-full rounded border border-gray-300 px-3 py-2 text-sm"
				placeholder="Username"
				autocomplete="username"
				bind:value={username}
			/>
			<input
				class="w-full rounded border border-gray-300 px-3 py-2 text-sm"
				type="password"
				placeholder="Password"
				autocomplete="current-password"
				bind:value={password}
			/>
			{#if error}
				<p class="text-sm text-red-600">{error}</p>
			{/if}
			<button class="w-full rounded bg-gray-900 px-3 py-2 text-sm text-white" type="submit">
				Sign in
			</button>
		</form>
	</main>
{/if}
