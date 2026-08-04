<script lang="ts">
	import type { Snapshot } from '$lib/model.gen';
	import { post } from '$lib/api';

	let { snap }: { snap: Snapshot } = $props();

	let poolName = $state('');
	let poolNode = $state('');
	let poolRaid = $state('none');
	let poolDisks = $state<string[]>([]);
	let volName = $state('');
	let volSize = $state('10Gi');
	let volClass = $state('stornas-local');
	let volBlock = $state(false);
	let shareName = $state('');
	let shareClaim = $state('');
	let shareNFS = $state('');
	let shareSMB = $state(false);
	let targetName = $state('');
	let targetVIP = $state('');
	let targetLUNs = $state<string[]>([]);
	let targetInitiators = $state('');
	let error = $state('');
	let notice = $state('');

	const freeDisks = $derived(
		(snap.nodes.find((n) => n.name === poolNode)?.disks ?? []).filter((d) => !d.claimed),
	);
	const fsVolumes = $derived(snap.volumes.filter((v) => !v.block));
	const blockVolumes = $derived(snap.volumes.filter((v) => v.block));

	async function submit(path: string, body: unknown, what: string) {
		error = '';
		notice = '';
		const e = await post(path, body);
		if (e) error = e;
		else notice = `${what} created`;
	}

	function createPool(e: Event) {
		e.preventDefault();
		submit(
			'/api/v1/pools',
			{ name: poolName, node: poolNode, devices: poolDisks, raid: poolRaid },
			'Pool',
		);
	}
	function createVolume(e: Event) {
		e.preventDefault();
		submit(
			'/api/v1/volumes',
			{ name: volName, size: volSize, storageClass: volClass, block: volBlock },
			'Volume',
		);
	}
	function createShare(e: Event) {
		e.preventDefault();
		submit(
			'/api/v1/shares',
			{
				name: shareName,
				claim: shareClaim,
				nfsClients: shareNFS ? shareNFS.split(',').map((s) => s.trim()) : [],
				smb: shareSMB,
			},
			'Share',
		);
	}
	function createTarget(e: Event) {
		e.preventDefault();
		submit(
			'/api/v1/targets',
			{
				name: targetName,
				vip: targetVIP,
				luns: targetLUNs.map((claim, i) => ({ id: i, claim })),
				initiators: targetInitiators ? targetInitiators.split(',').map((s) => s.trim()) : [],
			},
			'Target',
		);
	}
</script>

<section class="space-y-3">
	<h2 class="text-sm font-medium uppercase tracking-wide opacity-60">Create</h2>
	{#if error}<p class="text-sm text-red-600">{error}</p>{/if}
	{#if notice}<p class="text-sm text-emerald-700">{notice}</p>{/if}

	<div class="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
		<form onsubmit={createPool} class="space-y-2 rounded border border-gray-200 p-4">
			<h3 class="text-sm font-semibold">Pool</h3>
			<input
				class="w-full rounded border px-2 py-1 text-sm"
				placeholder="Name"
				bind:value={poolName}
			/>
			<select class="w-full rounded border px-2 py-1 text-sm" bind:value={poolNode}>
				<option value="" disabled>Node</option>
				{#each snap.nodes as n (n.name)}<option value={n.name}>{n.name}</option>{/each}
			</select>
			<select class="w-full rounded border px-2 py-1 text-sm" bind:value={poolRaid}>
				{#each ['none', 'raid1', 'raid5', 'raid10'] as r (r)}<option value={r}>{r}</option>{/each}
			</select>
			<div class="max-h-28 space-y-1 overflow-y-auto text-sm">
				{#each freeDisks as d (d.path)}
					<label class="flex items-center gap-2">
						<input type="checkbox" value={d.path} bind:group={poolDisks} />
						<span class="truncate">{d.model || d.path}</span>
					</label>
				{:else}
					<p class="text-xs opacity-60">No unclaimed disks{poolNode ? '' : ' (pick a node)'}.</p>
				{/each}
			</div>
			<button class="w-full rounded bg-gray-900 px-2 py-1 text-sm text-white" type="submit"
				>Create pool</button
			>
		</form>

		<form onsubmit={createVolume} class="space-y-2 rounded border border-gray-200 p-4">
			<h3 class="text-sm font-semibold">Volume</h3>
			<input
				class="w-full rounded border px-2 py-1 text-sm"
				placeholder="Name"
				bind:value={volName}
			/>
			<input
				class="w-full rounded border px-2 py-1 text-sm"
				placeholder="Size (10Gi)"
				bind:value={volSize}
			/>
			<select class="w-full rounded border px-2 py-1 text-sm" bind:value={volClass}>
				<option value="stornas-local">stornas-local</option>
				<option value="stornas-replicated">stornas-replicated</option>
			</select>
			<label class="flex items-center gap-2 text-sm">
				<input type="checkbox" bind:checked={volBlock} /> Block mode (VM/iSCSI)
			</label>
			<button class="w-full rounded bg-gray-900 px-2 py-1 text-sm text-white" type="submit"
				>Create volume</button
			>
		</form>

		<form onsubmit={createShare} class="space-y-2 rounded border border-gray-200 p-4">
			<h3 class="text-sm font-semibold">Share</h3>
			<input
				class="w-full rounded border px-2 py-1 text-sm"
				placeholder="Name"
				bind:value={shareName}
			/>
			<select class="w-full rounded border px-2 py-1 text-sm" bind:value={shareClaim}>
				<option value="" disabled>Volume</option>
				{#each fsVolumes as v (v.namespace + '/' + v.name)}
					<option value={v.name}>{v.name}</option>
				{/each}
			</select>
			<input
				class="w-full rounded border px-2 py-1 text-sm"
				placeholder="NFS clients (comma separated)"
				bind:value={shareNFS}
			/>
			<label class="flex items-center gap-2 text-sm">
				<input type="checkbox" bind:checked={shareSMB} /> SMB
			</label>
			<button class="w-full rounded bg-gray-900 px-2 py-1 text-sm text-white" type="submit"
				>Create share</button
			>
		</form>

		<form onsubmit={createTarget} class="space-y-2 rounded border border-gray-200 p-4">
			<h3 class="text-sm font-semibold">iSCSI target</h3>
			<input
				class="w-full rounded border px-2 py-1 text-sm"
				placeholder="Name"
				bind:value={targetName}
			/>
			<input
				class="w-full rounded border px-2 py-1 text-sm"
				placeholder="VIP CIDR (for replicated LUNs)"
				bind:value={targetVIP}
			/>
			<div class="max-h-28 space-y-1 overflow-y-auto text-sm">
				{#each blockVolumes as v (v.namespace + '/' + v.name)}
					<label class="flex items-center gap-2">
						<input type="checkbox" value={v.name} bind:group={targetLUNs} />
						<span class="truncate">{v.name}</span>
					</label>
				{:else}
					<p class="text-xs opacity-60">No block volumes; create one first.</p>
				{/each}
			</div>
			<input
				class="w-full rounded border px-2 py-1 text-sm"
				placeholder="Initiator IQNs (comma separated)"
				bind:value={targetInitiators}
			/>
			<button class="w-full rounded bg-gray-900 px-2 py-1 text-sm text-white" type="submit"
				>Create target</button
			>
		</form>
	</div>
</section>
