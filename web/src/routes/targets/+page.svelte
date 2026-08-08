<script lang="ts">
	import { app } from '$lib/state.svelte';
	import { post, del } from '$lib/api';
	import { toasts } from '$lib/toast.svelte';
	import StatusBadge from '$lib/ui/StatusBadge.svelte';
	import ConfirmDialog from '$lib/ui/ConfirmDialog.svelte';

	const snap = $derived(app.snap);
	const blockVolumes = $derived(snap.volumes.filter((v) => v.block));

	// What an initiator discovers against: the VIP when the target has
	// one, else the active node's address.
	function portal(t: { vip: string; activeNode: string }): string {
		if (t.vip) return t.vip.split('/')[0];
		const n = snap.nodes.find((n) => n.name === t.activeNode);
		return n?.addresses?.[0] ?? '-';
	}

	let actionError = $state('');
	let name = $state('');
	let vip = $state('');
	let luns = $state<string[]>([]);
	let initiators = $state('');

	async function createTarget(e: Event) {
		e.preventDefault();
		actionError = '';
		const err = await post('/api/v1/targets', {
			name,
			vip,
			luns: luns.map((claim, i) => ({ id: i, claim })),
			initiators: initiators ? initiators.split(',').map((s) => s.trim()) : [],
		});
		if (err) actionError = err;
		else {
			toasts.show(`Target ${name} created`, 'success');
			name = '';
			luns = [];
		}
	}

	let deleting = $state('');
	let deleteError = $state('');
	let busy = $state(false);

	async function deleteTarget() {
		busy = true;
		deleteError = '';
		const err = await del(`/api/v1/targets/${deleting}`);
		busy = false;
		if (err) deleteError = err;
		else {
			toasts.show(`Target ${deleting} deleted`, 'success');
			deleting = '';
		}
	}
</script>

<div class="space-y-6">
	<h1 class="text-xl font-semibold text-slate-100">iSCSI targets</h1>

	{#if snap.targets.length === 0}
		<p class="text-sm text-slate-500">No targets yet.</p>
	{:else}
		<div class="overflow-x-auto rounded-lg border border-slate-800 bg-slate-900">
			<table class="w-full text-sm">
				<thead class="text-left text-xs text-slate-400">
					<tr class="border-b border-slate-800">
						<th class="px-3 py-2 font-medium">Name</th>
						<th class="px-3 py-2 font-medium">IQN</th>
						<th class="px-3 py-2 font-medium">LUNs</th>
						<th class="px-3 py-2 font-medium">Active node</th>
						<th class="px-3 py-2 font-medium">Portal</th>
						<th class="px-3 py-2 font-medium">Sessions</th>
						<th class="px-3 py-2 font-medium">State</th>
						{#if app.role === 'admin'}<th class="px-3 py-2 font-medium">Actions</th>{/if}
					</tr>
				</thead>
				<tbody>
					{#each snap.targets as t (t.namespace + '/' + t.name)}
						<tr class="border-t border-slate-800/60">
							<td class="px-3 py-2 font-medium text-slate-200">{t.name}</td>
							<td class="px-3 py-2 font-mono text-xs text-slate-400">{t.iqn || '-'}</td>
							<td class="px-3 py-2 text-xs">
								{#each t.luns ?? [] as l (l.id)}
									<span class="mr-1 rounded bg-slate-800 px-1.5 py-0.5 text-slate-300">
										{l.id}: {l.claim}
									</span>
								{/each}
							</td>
							<td class="px-3 py-2 text-slate-400">{t.activeNode || '-'}</td>
							<td class="px-3 py-2 font-mono text-xs text-slate-400">{portal(t)}</td>
							<td class="px-3 py-2 tabular-nums text-slate-300">{t.sessions}</td>
							<td class="px-3 py-2">
								<StatusBadge
									kind={t.available ? 'ok' : t.state === 'Failed' ? 'bad' : 'warn'}
									label={t.available ? t.state : t.reason || t.state}
								/>
							</td>
							{#if app.role === 'admin'}
								<td class="px-3 py-2">
									<button
										class="rounded bg-red-500/10 px-1.5 py-0.5 text-xs text-red-400 hover:bg-red-500/20"
										onclick={() => ((deleting = t.name), (deleteError = ''))}
									>
										delete
									</button>
								</td>
							{/if}
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}

	{#if app.role === 'admin'}
		<section class="max-w-md rounded-lg border border-slate-800 bg-slate-900 p-4">
			<h2 class="mb-3 text-sm font-medium text-slate-300">New target</h2>
			<form onsubmit={createTarget} class="space-y-2">
				<input
					class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
					placeholder="Name"
					bind:value={name}
				/>
				<input
					class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
					placeholder="VIP CIDR (for replicated LUNs)"
					bind:value={vip}
				/>
				<div class="max-h-32 space-y-1 overflow-y-auto text-sm">
					{#each blockVolumes as v (v.namespace + '/' + v.name)}
						<label class="flex items-center gap-2">
							<input type="checkbox" value={v.name} bind:group={luns} />
							<span class="truncate">{v.name}</span>
						</label>
					{:else}
						<p class="text-xs text-slate-500">No block volumes; create one first.</p>
					{/each}
				</div>
				<input
					class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
					placeholder="Initiator IQNs (comma separated)"
					bind:value={initiators}
				/>
				{#if actionError}<p class="text-sm text-red-400">{actionError}</p>{/if}
				<button
					class="w-full rounded-md bg-sky-600 px-2 py-1.5 text-sm font-medium text-white hover:bg-sky-500"
					type="submit"
				>
					Create target
				</button>
			</form>
		</section>
	{/if}
</div>

{#if deleting}
	<ConfirmDialog
		title="Delete target"
		{busy}
		error={deleteError}
		onconfirm={deleteTarget}
		onclose={() => (deleting = '')}
	>
		<p>
			Target <span class="font-mono text-slate-200">{deleting}</span> will stop being exported; logged-in
			initiators lose their LUNs. The volumes and their data stay.
		</p>
	</ConfirmDialog>
{/if}
