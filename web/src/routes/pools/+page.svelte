<script lang="ts">
	import { app } from '$lib/state.svelte';
	import { formatBytes } from '$lib/stream';
	import { post } from '$lib/api';
	import { toasts } from '$lib/toast.svelte';
	import Meter from '$lib/ui/Meter.svelte';
	import StatusBadge from '$lib/ui/StatusBadge.svelte';
	import Modal from '$lib/ui/Modal.svelte';

	const snap = $derived(app.snap);

	let name = $state('');
	let node = $state('');
	let raid = $state('none');
	let disks = $state<string[]>([]);
	let error = $state('');

	const freeDisks = $derived(
		(snap.nodes.find((n) => n.name === node)?.disks ?? []).filter((d) => !d.claimed),
	);

	async function createPool(e: Event) {
		e.preventDefault();
		error = '';
		const err = await post('/api/v1/pools', { name, node, devices: disks, raid });
		if (err) error = err;
		else {
			toasts.show(`Pool ${name} created`, 'success');
			name = '';
			disks = [];
		}
	}

	// Disk replace: swap one member for an unclaimed disk on the same node.
	let replacing = $state<{ pool: string; node: string; old: string } | null>(null);
	let replacement = $state('');
	let replaceError = $state('');
	let replaceBusy = $state(false);

	const replacementDisks = $derived(
		replacing
			? (snap.nodes.find((n) => n.name === replacing?.node)?.disks ?? []).filter((d) => !d.claimed)
			: [],
	);

	function openReplace(pool: string, poolNode: string, old: string) {
		replacing = { pool, node: poolNode, old };
		replacement = '';
		replaceError = '';
	}

	async function doReplace() {
		if (!replacing || !replacement) return;
		replaceBusy = true;
		replaceError = '';
		const err = await post(`/api/v1/pools/${replacing.pool}/replace`, {
			old: replacing.old,
			new: replacement,
		});
		replaceBusy = false;
		if (err) replaceError = err;
		else {
			toasts.show(`Replacing ${replacing.old} with ${replacement}`, 'success');
			replacing = null;
		}
	}

	function healthKind(h: string): 'ok' | 'warn' | 'bad' | 'neutral' {
		return h === 'Online' ? 'ok' : h === 'Degraded' ? 'warn' : h === 'Failed' ? 'bad' : 'neutral';
	}
	function deviceKind(s: string): 'ok' | 'warn' | 'bad' | 'neutral' {
		return s === 'InSync'
			? 'ok'
			: s === 'Rebuilding'
				? 'warn'
				: s === 'Failed' || s === 'Missing'
					? 'bad'
					: 'neutral';
	}
</script>

<div class="space-y-6">
	<h1 class="text-xl font-semibold text-slate-100">Storage pools</h1>

	{#if snap.pools.length === 0}
		<p class="text-sm text-slate-500">No storage pools yet.</p>
	{:else}
		<div class="space-y-3">
			{#each snap.pools as pool (pool.name)}
				<section class="rounded-lg border border-slate-800 bg-slate-900">
					<div class="flex flex-wrap items-center gap-3 px-4 py-3">
						<span class="font-medium text-slate-100">{pool.name}</span>
						<span class="text-sm text-slate-400">on {pool.node}</span>
						<span class="rounded bg-slate-800 px-1.5 py-0.5 text-xs text-slate-400">
							{pool.raid || 'none'}
						</span>
						<StatusBadge kind={healthKind(pool.health)} label={pool.health} />
						{#if !pool.available && pool.reason}
							<span class="text-xs text-amber-400">{pool.reason}</span>
						{/if}
						<div class="ml-auto w-56">
							<Meter used={pool.capacityBytes - pool.freeBytes} total={pool.capacityBytes} />
						</div>
						<span class="text-xs text-slate-400">
							{formatBytes(pool.capacityBytes - pool.freeBytes)} of {formatBytes(
								pool.capacityBytes,
							)}
						</span>
					</div>
					{#if pool.rebuildPercent != null}
						<div class="flex items-center gap-3 border-t border-slate-800 px-4 py-2.5">
							<span class="text-xs text-slate-400">Rebuilding</span>
							<div class="h-1.5 w-56 overflow-hidden rounded-full bg-sky-500/15">
								<div
									class="h-full rounded-full bg-sky-500"
									style="width:{pool.rebuildPercent}%"
								></div>
							</div>
							<span class="text-xs tabular-nums text-slate-400">{pool.rebuildPercent}%</span>
						</div>
					{/if}
					<div class="flex flex-wrap gap-2 border-t border-slate-800 px-4 py-2.5">
						{#each pool.devices ?? [] as d (d.path)}
							<span class="inline-flex items-center gap-2 rounded bg-slate-800/60 px-2 py-1">
								<span class="font-mono text-xs text-slate-300">{d.path}</span>
								<StatusBadge kind={deviceKind(d.state)} label={d.state || 'pending'} />
								{#if d.smart && d.smart !== 'PASSED'}
									<span class="text-xs text-red-400">SMART: {d.smart}</span>
								{/if}
								{#if app.role === 'admin'}
									<button
										class="rounded px-1 py-0.5 text-xs {d.state === 'Missing' ||
										d.state === 'Failed'
											? 'bg-red-500/10 text-red-400 hover:bg-red-500/20'
											: 'bg-slate-800 text-slate-400 hover:bg-slate-700'}"
										onclick={() => openReplace(pool.name, pool.node, d.path)}
									>
										replace
									</button>
								{/if}
							</span>
						{/each}
					</div>
				</section>
			{/each}
		</div>
	{/if}

	{#if app.role === 'admin'}
		<section class="max-w-md rounded-lg border border-slate-800 bg-slate-900 p-4">
			<h2 class="mb-3 text-sm font-medium text-slate-300">New pool</h2>
			{#if error}<p class="mb-2 text-sm text-red-400">{error}</p>{/if}
			<form onsubmit={createPool} class="space-y-2">
				<input
					class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
					placeholder="Name"
					bind:value={name}
				/>
				<select
					class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm focus:border-sky-500 focus:outline-none"
					bind:value={node}
				>
					<option value="" disabled>Node</option>
					{#each snap.nodes as n (n.name)}<option value={n.name}>{n.name}</option>{/each}
				</select>
				<select
					class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm focus:border-sky-500 focus:outline-none"
					bind:value={raid}
				>
					{#each ['none', 'raid1', 'raid5', 'raid10'] as r (r)}<option value={r}>{r}</option>{/each}
				</select>
				<div class="max-h-32 space-y-1 overflow-y-auto text-sm">
					{#each freeDisks as d (d.path)}
						<label class="flex items-center gap-2">
							<input type="checkbox" value={d.path} bind:group={disks} />
							<span class="truncate">{d.model || d.path}</span>
							<span class="ml-auto text-xs text-slate-500">{formatBytes(d.sizeBytes)}</span>
						</label>
					{:else}
						<p class="text-xs text-slate-500">
							No unclaimed disks{node ? '' : ' (pick a node)'}.
						</p>
					{/each}
				</div>
				<button
					class="w-full rounded-md bg-sky-600 px-2 py-1.5 text-sm font-medium text-white hover:bg-sky-500"
					type="submit"
				>
					Create pool
				</button>
			</form>
		</section>
	{/if}
</div>

{#if replacing}
	{@const rep = replacing}
	<Modal title="Replace disk in {rep.pool}" onclose={() => (replacing = null)}>
		<div class="px-5 py-4">
			<p class="mb-3 text-sm text-slate-400">
				<span class="font-mono text-xs text-slate-300">{rep.old}</span> leaves the pool. A dead member
				is repaired around; a live one is evacuated first, so data stays available either way. The rebuild
				runs in the background.
			</p>
			<span class="mb-1 block text-xs text-slate-400">Replacement disk on {rep.node}</span>
			<div class="max-h-40 space-y-1 overflow-y-auto text-sm">
				{#each replacementDisks as d (d.path)}
					<label class="flex items-center gap-2">
						<input type="radio" name="replacement" value={d.path} bind:group={replacement} />
						<span class="truncate">{d.model || d.path}</span>
						<span class="ml-auto text-xs text-slate-500">{formatBytes(d.sizeBytes)}</span>
					</label>
				{:else}
					<p class="text-xs text-amber-400">
						No unclaimed disks on {rep.node}; attach one first.
					</p>
				{/each}
			</div>
			{#if replaceError}<p class="mt-2 text-xs text-red-400">{replaceError}</p>{/if}
		</div>
		{#snippet footer()}
			<button
				onclick={() => (replacing = null)}
				class="ml-auto rounded border border-slate-700 px-3 py-1 text-sm text-slate-300 hover:bg-slate-800"
			>
				Cancel
			</button>
			<button
				onclick={doReplace}
				disabled={!replacement || replaceBusy}
				class="rounded bg-sky-600 px-3 py-1 text-sm font-medium text-white hover:bg-sky-500 disabled:opacity-50"
			>
				Replace disk
			</button>
		{/snippet}
	</Modal>
{/if}
