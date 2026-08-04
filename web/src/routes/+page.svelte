<script lang="ts">
	import { onMount } from 'svelte';
	import { HardDrive, Server, Database } from 'lucide-svelte';
	import type { Snapshot } from '$lib/model.gen';
	import { connectState, formatBytes } from '$lib/stream';
	import CreateForms from '$lib/CreateForms.svelte';

	let snap = $state<Snapshot>({ pools: [], nodes: [], volumes: [], shares: [] });
	let role = $state('');

	onMount(() => {
		fetch('/api/v1/session')
			.then((r) => (r.ok ? r.json() : undefined))
			.then((s) => (role = s?.role ?? ''));
		return connectState((s) => (snap = s));
	});

	const healthClass: Record<string, string> = {
		Online: 'bg-emerald-100 text-emerald-800',
		Degraded: 'bg-amber-100 text-amber-800',
		Failed: 'bg-red-100 text-red-800',
		Unknown: 'bg-gray-100 text-gray-600',
	};
</script>

<main class="mx-auto max-w-5xl space-y-8 p-6">
	<header class="flex items-center gap-2">
		<HardDrive size={22} />
		<h1 class="text-xl font-semibold">stornas</h1>
	</header>

	{#if role === 'admin'}
		<CreateForms {snap} />
	{/if}

	<section>
		<h2 class="mb-2 flex items-center gap-2 text-sm font-medium uppercase tracking-wide opacity-60">
			<Database size={14} /> Pools
		</h2>
		{#if snap.pools.length === 0}
			<p class="text-sm opacity-60">No storage pools yet.</p>
		{:else}
			<div class="overflow-x-auto rounded border border-gray-200">
				<table class="w-full text-sm">
					<thead class="bg-gray-50 text-left">
						<tr>
							<th class="px-3 py-2">Name</th>
							<th class="px-3 py-2">Node</th>
							<th class="px-3 py-2">Raid</th>
							<th class="px-3 py-2">Health</th>
							<th class="px-3 py-2">Capacity</th>
							<th class="px-3 py-2">Free</th>
							<th class="px-3 py-2">Status</th>
						</tr>
					</thead>
					<tbody>
						{#each snap.pools as pool (pool.name)}
							<tr class="border-t border-gray-100">
								<td class="px-3 py-2 font-medium">{pool.name}</td>
								<td class="px-3 py-2">{pool.node}</td>
								<td class="px-3 py-2">{pool.raid || 'none'}</td>
								<td class="px-3 py-2">
									<span
										class="rounded px-1.5 py-0.5 text-xs {healthClass[pool.health] ??
											healthClass.Unknown}"
									>
										{pool.health}
									</span>
								</td>
								<td class="px-3 py-2">{formatBytes(pool.capacityBytes)}</td>
								<td class="px-3 py-2">{formatBytes(pool.freeBytes)}</td>
								<td class="px-3 py-2 text-xs opacity-70"
									>{pool.available ? 'Available' : pool.reason}</td
								>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
	</section>

	<section>
		<h2 class="mb-2 flex items-center gap-2 text-sm font-medium uppercase tracking-wide opacity-60">
			<Server size={14} /> Nodes
		</h2>
		<ul class="flex flex-wrap gap-2">
			{#each snap.nodes as node (node.name)}
				<li class="rounded border border-gray-200 px-3 py-2 text-sm">
					<span
						class="mr-2 inline-block h-2 w-2 rounded-full {node.ready
							? 'bg-emerald-500'
							: 'bg-red-500'}"
					></span>
					<span class="font-medium">{node.name}</span>
					<span class="ml-2 opacity-60">{node.roles.join(', ')}</span>
					<span class="ml-2 opacity-60">{node.addresses.join(' ')}</span>
				</li>
			{:else}
				<li class="text-sm opacity-60">No nodes.</li>
			{/each}
		</ul>
	</section>

	<section>
		<h2 class="mb-2 text-sm font-medium uppercase tracking-wide opacity-60">Shares</h2>
		{#if snap.shares.length === 0}
			<p class="text-sm opacity-60">No shares yet.</p>
		{:else}
			<div class="overflow-x-auto rounded border border-gray-200">
				<table class="w-full text-sm">
					<thead class="bg-gray-50 text-left">
						<tr>
							<th class="px-3 py-2">Name</th>
							<th class="px-3 py-2">Claim</th>
							<th class="px-3 py-2">Protocols</th>
							<th class="px-3 py-2">Node</th>
							<th class="px-3 py-2">State</th>
							<th class="px-3 py-2">Status</th>
						</tr>
					</thead>
					<tbody>
						{#each snap.shares as s (s.namespace + '/' + s.name)}
							<tr class="border-t border-gray-100">
								<td class="px-3 py-2 font-medium">{s.name}</td>
								<td class="px-3 py-2">{s.claim}</td>
								<td class="px-3 py-2"
									>{[s.nfs && 'NFS', s.smb && 'SMB'].filter(Boolean).join(' + ')}</td
								>
								<td class="px-3 py-2">{s.node || '-'}</td>
								<td class="px-3 py-2">{s.state}</td>
								<td class="px-3 py-2 text-xs opacity-70">{s.available ? 'Available' : s.reason}</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
	</section>

	<section>
		<h2 class="mb-2 text-sm font-medium uppercase tracking-wide opacity-60">Volumes</h2>
		{#if snap.volumes.length === 0}
			<p class="text-sm opacity-60">No volumes yet.</p>
		{:else}
			<div class="overflow-x-auto rounded border border-gray-200">
				<table class="w-full text-sm">
					<thead class="bg-gray-50 text-left">
						<tr>
							<th class="px-3 py-2">Namespace</th>
							<th class="px-3 py-2">Name</th>
							<th class="px-3 py-2">Class</th>
							<th class="px-3 py-2">Mode</th>
							<th class="px-3 py-2">Size</th>
							<th class="px-3 py-2">Phase</th>
						</tr>
					</thead>
					<tbody>
						{#each snap.volumes as vol (vol.namespace + '/' + vol.name)}
							<tr class="border-t border-gray-100">
								<td class="px-3 py-2">{vol.namespace}</td>
								<td class="px-3 py-2 font-medium">{vol.name}</td>
								<td class="px-3 py-2">{vol.storageClass}</td>
								<td class="px-3 py-2">{vol.block ? 'Block' : 'Filesystem'}</td>
								<td class="px-3 py-2">{formatBytes(vol.capacityBytes)}</td>
								<td class="px-3 py-2">{vol.phase}</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
	</section>
</main>
