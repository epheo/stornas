<script lang="ts">
	import { app } from '#lib/state.svelte.js';
	import { formatBytes } from '#lib/stream.js';
	import { post, del } from '#lib/api.js';
	import { toasts } from '#lib/toast.svelte.js';
	import { validName, NAME_HINT, sizeBytes, SIZE_HINT } from '#lib/validate.js';
	import StatusBadge from '#lib/ui/StatusBadge.svelte';
	import Modal from '#lib/ui/Modal.svelte';
	import ConfirmDialog from '#lib/ui/ConfirmDialog.svelte';
	import ConfirmDelete from '#lib/ui/ConfirmDelete.svelte';
	import ProgressBar from '#lib/ui/ProgressBar.svelte';
	import type { Replica } from '#lib/model.gen.js';

	const snap = $derived(app.snap);
	// Matches the API: two data replicas need a third node as tiebreaker.
	const canReplicate = $derived(snap.nodes.length >= 3);
	// Tier name to the nodes holding it; a replicated volume needs two.
	const tiers = $derived.by(() => {
		const m = new Map<string, Set<string>>([['default', new Set()]]);
		for (const p of snap.pools) {
			const t = p.tier || 'default';
			if (!m.has(t)) m.set(t, new Set());
			m.get(t)!.add(p.node);
		}
		return [...m].map(([tier, nodes]) => ({ tier, nodes: nodes.size }));
	});
	const replicatedClass = (tier: string) =>
		tier === 'default' ? 'stornas-replicated' : `stornas-replicated-${tier}`;
	const downNodes = $derived(new Set(snap.nodes.filter((n) => !n.ready).map((n) => n.name)));
	let actionError = $state('');

	let name = $state('');
	let size = $state('10Gi');
	// A class name, or pool:<name> for a local volume pinned to one pool.
	let placement = $state('stornas-local');
	let block = $state(false);
	let createBusy = $state(false);

	async function createVolume(e: Event) {
		e.preventDefault();
		actionError = '';
		createBusy = true;
		const target = placement.startsWith('pool:')
			? { pool: placement.slice('pool:'.length) }
			: { storageClass: placement };
		const err = await post(`/api/v1/namespaces/${snap.systemNamespace}/volumes`, {
			name,
			size,
			block,
			...target,
		});
		createBusy = false;
		if (err) actionError = err;
		else {
			toasts.show(`Volume ${name} created`, 'success');
			name = '';
		}
	}

	// Every volume and snapshot action carries its object's namespace: the
	// page lists every namespace, and names only identify within one.
	type ModalState =
		| { kind: 'delete-volume'; ns: string; name: string }
		| { kind: 'resize'; ns: string; name: string; current: number }
		| { kind: 'snapshot'; ns: string; name: string }
		| { kind: 'restore'; ns: string; snapshot: string }
		| { kind: 'delete-snapshot'; ns: string; name: string }
		| { kind: 'resolve-split'; ns: string; name: string; replicas: Replica[] };
	const volumePath = (ns: string, name: string) => `/api/v1/namespaces/${ns}/volumes/${name}`;
	let modal = $state<ModalState | null>(null);
	let modalError = $state('');
	let busy = $state(false);

	// Per-modal form fields, seeded on open.
	let newSize = $state('');
	let snapName = $state('');
	let restoreName = $state('');
	let survivor = $state('');

	function open(m: ModalState) {
		modal = m;
		modalError = '';
		if (m.kind === 'resize') newSize = '';
		if (m.kind === 'snapshot') snapName = `${m.name}-${new Date().toISOString().slice(0, 10)}`;
		if (m.kind === 'restore') restoreName = `${m.snapshot}-restore`;
		if (m.kind === 'resolve-split') survivor = '';
	}

	async function perform(fn: () => Promise<string>, done: string) {
		busy = true;
		modalError = '';
		const err = await fn();
		busy = false;
		if (err) modalError = err;
		else {
			modal = null;
			toasts.show(done, 'success');
		}
	}

	const resizeValid = $derived.by(() => {
		if (modal?.kind !== 'resize') return false;
		const b = sizeBytes(newSize);
		return b != null && b > modal.current;
	});
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
									{#if vol.replication.splitBrain}
										<span class="mr-1 inline-block"
											><StatusBadge kind="bad" label="split brain" /></span
										>
									{/if}
									{#if vol.replication.peerUnreachable}
										<span class="mr-1 inline-block"
											><StatusBadge
												kind="bad"
												label="peer unreachable: {vol.replication.peerUnreachable.reason}"
											/></span
										>
										<p class="mt-1 text-red-400">{vol.replication.peerUnreachable.message}</p>
									{/if}
									<!-- The matrix promise: every diskful node down is said
									     plainly, not left to badge archaeology. -->
									{#if (vol.replication.replicas ?? []).length > 0 && (vol.replication.replicas ?? []).every( (r) => downNodes.has(r.node) )}
										<span class="mr-1 inline-block"
											><StatusBadge kind="bad" label="unavailable, node down" /></span
										>
									{/if}
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
									{#each (vol.replication.replicas ?? []).filter((r) => r.syncPercent != null) as r (r.node)}
										<div class="mt-1 flex items-center gap-2">
											<ProgressBar percent={r.syncPercent ?? 0} width="w-28" />
											<span class="tabular-nums text-slate-400">{r.node} {r.syncPercent}%</span>
										</div>
									{/each}
								{:else}
									<span class="text-slate-500">local</span>
								{/if}
							</td>
							{#if app.role === 'admin'}
								<td class="space-x-1 px-3 py-2 whitespace-nowrap">
									{#if vol.replication?.splitBrain}
										{@render actionButton('resolve', true, () =>
											open({
												kind: 'resolve-split',
												ns: vol.namespace,
												name: vol.name,
												replicas: vol.replication?.replicas ?? [],
											}),
										)}
									{/if}
									{@render actionButton('snapshot', false, () =>
										open({ kind: 'snapshot', ns: vol.namespace, name: vol.name }),
									)}
									{@render actionButton('resize', false, () =>
										open({
											kind: 'resize',
											ns: vol.namespace,
											name: vol.name,
											current: vol.capacityBytes,
										}),
									)}
									{@render actionButton('delete', true, () =>
										open({ kind: 'delete-volume', ns: vol.namespace, name: vol.name }),
									)}
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
							<th class="px-3 py-2 font-medium">Namespace</th>
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
								<td class="px-3 py-2 text-slate-400">{s.namespace}</td>
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
										{@render actionButton('restore', false, () =>
											open({ kind: 'restore', ns: s.namespace, snapshot: s.name }),
										)}
										{@render actionButton('delete', true, () =>
											open({ kind: 'delete-snapshot', ns: s.namespace, name: s.name }),
										)}
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
				{#if name && !validName(name)}<p class="text-xs text-amber-400">{NAME_HINT}</p>{/if}
				<input
					class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
					placeholder="Size (10Gi)"
					bind:value={size}
				/>
				{#if size && sizeBytes(size) == null}<p class="text-xs text-amber-400">{SIZE_HINT}</p>{/if}
				<select
					class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm focus:border-sky-500 focus:outline-none"
					bind:value={placement}
				>
					<optgroup label="Local">
						<option value="stornas-local">any pool in tier default</option>
						{#each snap.pools as p (p.name)}
							<option value="pool:{p.name}">
								{p.name} on {p.node} (tier {p.tier || 'default'})
							</option>
						{/each}
					</optgroup>
					<optgroup label="Replicated">
						{#each tiers as t (t.tier)}
							<option value={replicatedClass(t.tier)} disabled={!canReplicate || t.nodes < 2}>
								tier {t.tier}
							</option>
						{/each}
					</optgroup>
				</select>
				{#if !canReplicate}
					<p class="text-xs text-slate-500">Replication needs three nodes.</p>
				{/if}
				<label class="flex items-center gap-2 text-sm text-slate-300">
					<input type="checkbox" bind:checked={block} /> Block mode (VM/iSCSI)
				</label>
				{#if actionError}<p class="text-sm text-red-400">{actionError}</p>{/if}
				<button
					class="w-full rounded-md bg-sky-600 px-2 py-1.5 text-sm font-medium text-white hover:bg-sky-500 disabled:opacity-50"
					type="submit"
					disabled={!validName(name) ||
						sizeBytes(size) == null ||
						createBusy ||
						!snap.systemNamespace}
				>
					Create volume
				</button>
			</form>
		</section>
	{/if}
</div>

{#if modal?.kind === 'delete-volume'}
	{@const m = modal}
	<ConfirmDelete
		title="Delete volume"
		confirmWord={m.name}
		{busy}
		error={modalError}
		onconfirm={() => perform(() => del(volumePath(m.ns, m.name)), `Volume ${m.name} deleted`)}
		onclose={() => (modal = null)}
	>
		<p>
			Volume <span class="font-mono text-slate-200">{m.name}</span> and all data on it will be destroyed.
			Snapshots of it survive.
		</p>
	</ConfirmDelete>
{:else if modal?.kind === 'resize'}
	{@const m = modal}
	<Modal title="Resize {m.name}" onclose={() => (modal = null)}>
		<form
			class="px-5 py-4"
			onsubmit={(e) => {
				e.preventDefault();
				if (resizeValid)
					perform(
						() => post(`${volumePath(m.ns, m.name)}/resize`, { size: newSize }),
						`Volume ${m.name} resized to ${newSize}`,
					);
			}}
		>
			<p class="mb-3 text-sm text-slate-400">
				Currently {formatBytes(m.current)}. Volumes only grow; shrinking is not supported.
			</p>
			<label class="mb-1 block text-xs text-slate-400" for="resize-input">New size</label>
			<input
				id="resize-input"
				data-autofocus
				bind:value={newSize}
				placeholder="20Gi"
				class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
			/>
			{#if newSize && !resizeValid}
				<p class="mt-1 text-xs text-amber-400">
					{sizeBytes(newSize) == null ? SIZE_HINT : 'must be larger than the current size'}
				</p>
			{/if}
			{#if modalError}<p class="mt-2 text-xs text-red-400">{modalError}</p>{/if}
		</form>
		{#snippet footer()}
			<button
				onclick={() => (modal = null)}
				class="ml-auto rounded border border-slate-700 px-3 py-1 text-sm text-slate-300 hover:bg-slate-800"
			>
				Cancel
			</button>
			<button
				onclick={() =>
					perform(
						() => post(`${volumePath(m.ns, m.name)}/resize`, { size: newSize }),
						`Volume ${m.name} resized to ${newSize}`,
					)}
				disabled={!resizeValid || busy}
				class="rounded bg-sky-600 px-3 py-1 text-sm font-medium text-white hover:bg-sky-500 disabled:opacity-50"
			>
				Resize
			</button>
		{/snippet}
	</Modal>
{:else if modal?.kind === 'snapshot'}
	{@const m = modal}
	<Modal title="Snapshot {m.name}" onclose={() => (modal = null)}>
		<div class="px-5 py-4">
			<label class="mb-1 block text-xs text-slate-400" for="snap-input">Snapshot name</label>
			<input
				id="snap-input"
				data-autofocus
				bind:value={snapName}
				class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
			/>
			{#if snapName && !validName(snapName)}
				<p class="mt-1 text-xs text-amber-400">{NAME_HINT}</p>
			{/if}
			{#if modalError}<p class="mt-2 text-xs text-red-400">{modalError}</p>{/if}
		</div>
		{#snippet footer()}
			<button
				onclick={() => (modal = null)}
				class="ml-auto rounded border border-slate-700 px-3 py-1 text-sm text-slate-300 hover:bg-slate-800"
			>
				Cancel
			</button>
			<button
				onclick={() =>
					perform(
						() => post(`/api/v1/namespaces/${m.ns}/snapshots`, { name: snapName, volume: m.name }),
						`Snapshot ${snapName} created`,
					)}
				disabled={!validName(snapName) || busy}
				class="rounded bg-sky-600 px-3 py-1 text-sm font-medium text-white hover:bg-sky-500 disabled:opacity-50"
			>
				Create snapshot
			</button>
		{/snippet}
	</Modal>
{:else if modal?.kind === 'restore'}
	{@const m = modal}
	<Modal title="Restore from {m.snapshot}" onclose={() => (modal = null)}>
		<div class="px-5 py-4">
			<p class="mb-3 text-sm text-slate-400">
				Creates a new volume from the snapshot; the original volume is untouched.
			</p>
			<label class="mb-1 block text-xs text-slate-400" for="restore-input">New volume name</label>
			<input
				id="restore-input"
				data-autofocus
				bind:value={restoreName}
				class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
			/>
			{#if restoreName && !validName(restoreName)}
				<p class="mt-1 text-xs text-amber-400">{NAME_HINT}</p>
			{/if}
			{#if modalError}<p class="mt-2 text-xs text-red-400">{modalError}</p>{/if}
		</div>
		{#snippet footer()}
			<button
				onclick={() => (modal = null)}
				class="ml-auto rounded border border-slate-700 px-3 py-1 text-sm text-slate-300 hover:bg-slate-800"
			>
				Cancel
			</button>
			<button
				onclick={() =>
					perform(
						() =>
							post(`/api/v1/namespaces/${m.ns}/volumes`, {
								name: restoreName,
								size: '',
								fromSnapshot: m.snapshot,
							}),
						`Volume ${restoreName} restored from ${m.snapshot}`,
					)}
				disabled={!validName(restoreName) || busy}
				class="rounded bg-sky-600 px-3 py-1 text-sm font-medium text-white hover:bg-sky-500 disabled:opacity-50"
			>
				Restore
			</button>
		{/snippet}
	</Modal>
{:else if modal?.kind === 'resolve-split'}
	{@const m = modal}
	<Modal title="Resolve split brain on {m.name}" danger onclose={() => (modal = null)}>
		<div class="px-5 py-4">
			<p class="mb-3 text-sm text-slate-400">
				The replicas diverged and refuse to reconnect. Pick the copy to keep: every other replica is
				discarded and rebuilt from it. Writes that only reached a discarded replica are lost.
			</p>
			<div class="space-y-1.5 text-sm">
				<!-- A diskless replica (the quorum tiebreaker) holds no data to keep. -->
				{#each m.replicas.filter((r) => r.diskState !== 'Diskless') as r (r.node)}
					<label class="flex items-center gap-2.5 rounded border border-slate-800 px-3 py-2">
						<input type="radio" name="survivor" value={r.node} bind:group={survivor} />
						<span class="font-medium text-slate-200">{r.node}</span>
						<span class="text-xs text-slate-400">{r.diskState}</span>
						{#if r.inUse}
							<span class="rounded bg-sky-500/10 px-1.5 py-0.5 text-xs text-sky-400"> in use </span>
						{/if}
					</label>
				{/each}
			</div>
			{#if modalError}<p class="mt-2 text-xs text-red-400">{modalError}</p>{/if}
		</div>
		{#snippet footer()}
			<button
				onclick={() => (modal = null)}
				class="ml-auto rounded border border-slate-700 px-3 py-1 text-sm text-slate-300 hover:bg-slate-800"
			>
				Cancel
			</button>
			<button
				onclick={() =>
					perform(
						() => post(`${volumePath(m.ns, m.name)}/resolve-split`, { survivor }),
						`Keeping ${survivor}; other replicas resync from it`,
					)}
				disabled={!survivor || busy}
				class="rounded bg-red-600 px-3 py-1 text-sm font-medium text-white hover:bg-red-500 disabled:opacity-50"
			>
				Keep {survivor || 'selected'} and discard others
			</button>
		{/snippet}
	</Modal>
{:else if modal?.kind === 'delete-snapshot'}
	{@const m = modal}
	<ConfirmDialog
		title="Delete snapshot"
		{busy}
		error={modalError}
		onconfirm={() =>
			perform(
				() => del(`/api/v1/namespaces/${m.ns}/snapshots/${m.name}`),
				`Snapshot ${m.name} deleted`,
			)}
		onclose={() => (modal = null)}
	>
		<p>
			Snapshot <span class="font-mono text-slate-200">{m.name}</span> will be deleted. Volumes restored
			from it are unaffected.
		</p>
	</ConfirmDialog>
{/if}
