<script lang="ts">
	import '../app.css';
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import {
		HardDrive,
		LayoutDashboard,
		Database,
		FolderOpen,
		Cable,
		Server,
		Bell,
		Users,
		LogOut,
	} from 'lucide-svelte';
	import { app, startStream, loadSession } from '$lib/state.svelte';
	import ToastHost from '$lib/ui/ToastHost.svelte';

	let { children } = $props();

	let gate = $state<'loading' | 'anon' | 'in'>('loading');
	let username = $state('');
	let password = $state('');
	let error = $state('');
	let stopStream: (() => void) | undefined;

	onMount(() => {
		fetch('/api/v1/session')
			.then((r) => {
				gate = r?.ok ? 'in' : 'anon';
				if (r?.ok) enter();
			})
			.catch(() => (gate = 'anon'));
		return () => stopStream?.();
	});

	function enter() {
		loadSession();
		stopStream = startStream();
	}

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
			enter();
		} else {
			error = 'Invalid credentials';
			password = '';
		}
	}

	async function logout() {
		await fetch('/api/v1/logout', { method: 'POST' }).catch(() => undefined);
		location.reload();
	}

	const nav = $derived([
		{ href: '/', label: 'Dashboard', icon: LayoutDashboard },
		{ href: '/pools', label: 'Pools', icon: Database },
		{ href: '/volumes', label: 'Volumes', icon: HardDrive },
		{ href: '/shares', label: 'Shares', icon: FolderOpen },
		{ href: '/targets', label: 'Targets', icon: Cable },
		{ href: '/nodes', label: 'Nodes', icon: Server },
		{ href: '/alerts', label: 'Alerts', icon: Bell, badge: app.snap.alerts.length },
		...(app.role === 'admin' ? [{ href: '/users', label: 'Users', icon: Users }] : []),
	]);
</script>

{#if gate === 'in'}
	<div class="flex min-h-screen bg-slate-950 text-slate-200">
		<aside class="fixed inset-y-0 flex w-52 flex-col border-r border-slate-800 bg-slate-900">
			<div class="flex items-center gap-2 px-4 py-4">
				<HardDrive size={20} class="text-sky-400" />
				<span class="text-lg font-semibold text-slate-100">stornas</span>
			</div>
			<nav class="flex-1 space-y-0.5 px-2">
				{#each nav as item (item.href)}
					<a
						href={item.href}
						class="flex items-center gap-2.5 rounded-md px-2.5 py-2 text-sm {page.url.pathname ===
						item.href
							? 'bg-slate-800 font-medium text-slate-100'
							: 'text-slate-400 hover:bg-slate-800/50 hover:text-slate-200'}"
					>
						<item.icon size={16} />
						{item.label}
						{#if item.badge}
							<span
								class="ml-auto rounded-full bg-amber-500/15 px-1.5 py-0.5 text-xs font-medium text-amber-400"
							>
								{item.badge}
							</span>
						{/if}
					</a>
				{/each}
			</nav>
			<div class="border-t border-slate-800 px-4 py-3 text-sm">
				{#if app.who}
					<p class="truncate text-slate-300">{app.who}</p>
					<p class="text-xs text-slate-500">{app.role}</p>
				{/if}
				<button
					class="mt-2 flex items-center gap-1.5 text-xs text-slate-400 hover:text-slate-200"
					onclick={logout}
				>
					<LogOut size={12} /> Sign out
				</button>
			</div>
		</aside>
		<main class="ml-52 min-w-0 flex-1 p-6">
			{@render children()}
		</main>
		<ToastHost />
	</div>
{:else if gate === 'anon'}
	<main class="flex min-h-screen items-center justify-center bg-slate-950 text-slate-200">
		<form
			onsubmit={login}
			class="w-72 space-y-4 rounded-lg border border-slate-800 bg-slate-900 p-6"
		>
			<div class="flex items-center justify-center gap-2">
				<HardDrive size={20} class="text-sky-400" />
				<span class="text-lg font-semibold text-slate-100">stornas</span>
			</div>
			<input
				class="w-full rounded-md border border-slate-700 bg-slate-800 px-3 py-2 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
				placeholder="Username"
				autocomplete="username"
				bind:value={username}
			/>
			<input
				class="w-full rounded-md border border-slate-700 bg-slate-800 px-3 py-2 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
				type="password"
				placeholder="Password"
				autocomplete="current-password"
				bind:value={password}
			/>
			{#if error}
				<p class="text-sm text-red-400">{error}</p>
			{/if}
			<button
				class="w-full rounded-md bg-sky-600 px-3 py-2 text-sm font-medium text-white hover:bg-sky-500"
				type="submit"
			>
				Sign in
			</button>
		</form>
	</main>
{:else}
	<main class="min-h-screen bg-slate-950"></main>
{/if}
