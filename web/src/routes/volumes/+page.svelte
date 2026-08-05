<script lang="ts">
	import { app } from '$lib/state.svelte';
	import { formatBytes } from '$lib/stream';
	import { post, del } from '$lib/api';
	import StatusBadge from '$lib/ui/StatusBadge.svelte';

	const snap = $derived(app.snap);
	const downNodes = $derived(new Set(snap.nodes.filter((n) => !n.ready).map((n) => n.name)));
	let actionError = $state('');

	let name = $state('');
	let size = $state('10Gi');
	let storageClass = $state('stornas-local');
	let block = $state(false);
	let notice = $state('');

	async function createVolume(e: Event) {
		e.preventDefault();
		actionError = '';
		notice = '';
		const err = await post('/api/v1/volumes', { name, size, storageClass, block });
		if (err) actionError = err;
		else {
			notice = `Volume ${name} created`;
			name = '';
		}
	}

	async function run(fn: () => Promise<string>) {
		actionError = (await fn()) ?? '';
	}
	function deleteVolume(n: string) {
		if (confirm(`Delete volume ${n}? Data is lost.`)) run(() => del(`/api/v1/volumes/${n}`));
	}
	function resizeVolume(n: string, current: number) {
		const s = prompt(`New size for ${n} (currently ${formatBytes(current)}):`, '');
		if (s) run(() => post(`/api/v1/volumes/${n}/resize`, { size: s }));
	}
	function snapshotVolume(n: string) {
		const sn = prompt('Snapshot name:', `${n}-${new Date().toISOString().slice(0, 10)}`);
		if (sn) run(() => post('/api/v1/snapshots', { name: sn, volume: n }));
	}
	function restoreSnapshot(n: string) {
		const vn = prompt('New volume name (restored from snapshot):', `${n}-restore`);
		if (vn) run(() => post('/api/v1/volumes', { name: vn, size: '', fromSnapshot: n }));
	}
	function deleteSnapshot(n: string) {
		if (confirm(`Delete snapshot ${n}?`)) run(() => del(`/api/v1/snapshots/${n}`));
	}
</script>

{#snippet actionButton(label: string, danger: boolean, onclick: () => void)}
	<button
		class="rounded px-1.5 py-0.5 text-xs {danger
			? 'bg-red-500/10 text-red-400 hover:bg-red-500/20'
			: 'bg-slate-800 text-slate-300 hover:bg-slate-700'}"
		{onclick}
	>
		{label}
	</button>
{/snippet}

<div class="space-y-6">
	<h1 class="text-xl font-semibold text-slate-100">Volumes</h1>

	{#if actionError}<p class="text-sm text-red-400">{actionError}</p>{/if}
	{#if notice}<p class="text-sm text-emerald-400">{notice}</p>{/if}

	{#if snap.volumes.length === 0}
		<p class="text-sm text-slate-500">No volumes yet.</p>
	{:else}
		<div class="overflow-x-auto rounded-lg border border-slate-800 bg-slate-900">
			<table class="w-full text-sm">
				<thead class="text-left text-xs text-slate-400">
					<tr class="border-b border-slate-800">
						<th class="px-3 py-2 font-medium">Name</th>
						<th class="px-3 py-2 font-medium">Namespace</th>
						<th class="px-3 py-2 font-medium">Class</th>
						<th class="px-3 py-2 font-medium">Mode</th>
						<th class="px-3 py-2 font-medium">Size</th>
						<th class="px-3 py-2 font-medium">Phase</th>
						<th class="px-3 py-2 font-medium">Replication</th>
						{#if app.role === 'admin'}<th class="px-3 py-2 font-medium">Actions</th>{/if}
					</tr>
				</thead>
				<tbody>
					{#each snap.volumes as vol (vol.namespace + '/' + vol.name)}
						<tr class="border-t border-slate-800/60">
							<td class="px-3 py-2 font-medium text-slate-200">{vol.name}</td>
							<td class="px-3 py-2 text-slate-400">{vol.namespace}</td>
							<td class="px-3 py-2 text-slate-400">{vol.storageClass}</td>
							<td class="px-3 py-2 text-slate-400">{vol.block ? 'Block' : 'Filesystem'}</td>
							<td class="px-3 py-2 tabular-nums text-slate-300">
								{formatBytes(vol.capacityBytes)}
							</td>
							<td class="px-3 py-2">
								<StatusBadge kind={vol.phase === 'Bound' ? 'ok' : 'warn'} label={vol.phase} />
							</td>
							<td class="px-3 py-2 text-xs">
								{#if vol.replication}
									{#each vol.replication.replicas ?? [] as r (r.node)}
										<span class="mr-1 inline-block">
											<StatusBadge
												kind={downNodes.has(r.node)
													? 'bad'
													: r.diskState === 'UpToDate'
														? 'ok'
														: 'warn'}
												label="{r.node}: {downNodes.has(r.node) ? 'node down' : r.diskState}{r.inUse
													? ' *'
													: ''}"
											/>
										</span>
									{/each}
								{:else}
									<span class="text-slate-500">local</span>
								{/if}
							</td>
							{#if app.role === 'admin'}
								<td class="space-x-1 px-3 py-2 whitespace-nowrap">
									{@render actionButton('snapshot', false, () => snapshotVolume(vol.name))}
									{@render actionButton('resize', false, () =>
										resizeVolume(vol.name, vol.capacityBytes),
									)}
									{@render actionButton('delete', true, () => deleteVolume(vol.name))}
								</td>
							{/if}
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}

	<section>
		<h2 class="mb-2 text-sm font-medium text-slate-300">Snapshots</h2>
		{#if snap.snapshots.length === 0}
			<p class="text-sm text-slate-500">No snapshots yet.</p>
		{:else}
			<div class="overflow-x-auto rounded-lg border border-slate-800 bg-slate-900">
				<table class="w-full text-sm">
					<thead class="text-left text-xs text-slate-400">
						<tr class="border-b border-slate-800">
							<th class="px-3 py-2 font-medium">Name</th>
							<th class="px-3 py-2 font-medium">Volume</th>
							<th class="px-3 py-2 font-medium">Size</th>
							<th class="px-3 py-2 font-medium">Created</th>
							<th class="px-3 py-2 font-medium">State</th>
							{#if app.role === 'admin'}<th class="px-3 py-2 font-medium">Actions</th>{/if}
						</tr>
					</thead>
					<tbody>
						{#each snap.snapshots as s (s.namespace + '/' + s.name)}
							<tr class="border-t border-slate-800/60">
								<td class="px-3 py-2 font-medium text-slate-200">{s.name}</td>
								<td class="px-3 py-2 text-slate-400">{s.source}</td>
								<td class="px-3 py-2 tabular-nums text-slate-300">{formatBytes(s.sizeBytes)}</td>
								<td class="px-3 py-2 text-xs tabular-nums text-slate-400">
									{s.createdAt ? s.createdAt.slice(0, 19).replace('T', ' ') : '-'}
								</td>
								<td class="px-3 py-2">
									<StatusBadge
										kind={s.ready ? 'ok' : 'warn'}
										label={s.ready ? 'ready' : 'pending'}
									/>
								</td>
								{#if app.role === 'admin'}
									<td class="space-x-1 px-3 py-2 whitespace-nowrap">
										{@render actionButton('restore', false, () => restoreSnapshot(s.name))}
										{@render actionButton('delete', true, () => deleteSnapshot(s.name))}
									</td>
								{/if}
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
	</section>

	{#if app.role === 'admin'}
		<section class="max-w-md rounded-lg border border-slate-800 bg-slate-900 p-4">
			<h2 class="mb-3 text-sm font-medium text-slate-300">New volume</h2>
			<form onsubmit={createVolume} class="space-y-2">
				<input
					class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
					placeholder="Name"
					bind:value={name}
				/>
				<input
					class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
					placeholder="Size (10Gi)"
					bind:value={size}
				/>
				<select
					class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm focus:border-sky-500 focus:outline-none"
					bind:value={storageClass}
				>
					<option value="stornas-local">stornas-local</option>
					<option value="stornas-replicated">stornas-replicated</option>
				</select>
				<label class="flex items-center gap-2 text-sm text-slate-300">
					<input type="checkbox" bind:checked={block} /> Block mode (VM/iSCSI)
				</label>
				<button
					class="w-full rounded-md bg-sky-600 px-2 py-1.5 text-sm font-medium text-white hover:bg-sky-500"
					type="submit"
				>
					Create volume
				</button>
			</form>
		</section>
	{/if}
</div>
