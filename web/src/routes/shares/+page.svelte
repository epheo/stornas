<script lang="ts">
	import { app } from '$lib/state.svelte';
	import { post, del } from '$lib/api';
	import StatusBadge from '$lib/ui/StatusBadge.svelte';

	const snap = $derived(app.snap);
	const fsVolumes = $derived(snap.volumes.filter((v) => !v.block));

	// Agent conventions: NFS exports /var/lib/stornas/shares/<ns>-<name>,
	// the SMB section is the share name.
	function nodeIP(name: string): string {
		return snap.nodes.find((n) => n.name === name)?.addresses?.[0] ?? name;
	}
	function nfsPath(s: { namespace: string; name: string; node: string }): string {
		return `${nodeIP(s.node)}:/var/lib/stornas/shares/${s.namespace}-${s.name}`;
	}
	function smbPath(s: { name: string; node: string }): string {
		return `\\\\${nodeIP(s.node)}\\${s.name}`;
	}

	let actionError = $state('');
	let notice = $state('');
	let name = $state('');
	let claim = $state('');
	let nfsClients = $state('');
	let smb = $state(false);

	async function createShare(e: Event) {
		e.preventDefault();
		actionError = '';
		notice = '';
		const err = await post('/api/v1/shares', {
			name,
			claim,
			nfsClients: nfsClients ? nfsClients.split(',').map((s) => s.trim()) : [],
			smb,
		});
		if (err) actionError = err;
		else {
			notice = `Share ${name} created`;
			name = '';
		}
	}

	async function deleteShare(n: string) {
		if (!confirm(`Delete share ${n}? Clients lose access.`)) return;
		actionError = await del(`/api/v1/shares/${n}`);
	}
</script>

<div class="space-y-6">
	<h1 class="text-xl font-semibold text-slate-100">Shares</h1>

	{#if actionError}<p class="text-sm text-red-400">{actionError}</p>{/if}
	{#if notice}<p class="text-sm text-emerald-400">{notice}</p>{/if}

	{#if snap.shares.length === 0}
		<p class="text-sm text-slate-500">No shares yet.</p>
	{:else}
		<div class="overflow-x-auto rounded-lg border border-slate-800 bg-slate-900">
			<table class="w-full text-sm">
				<thead class="text-left text-xs text-slate-400">
					<tr class="border-b border-slate-800">
						<th class="px-3 py-2 font-medium">Name</th>
						<th class="px-3 py-2 font-medium">Volume</th>
						<th class="px-3 py-2 font-medium">Access</th>
						<th class="px-3 py-2 font-medium">Node</th>
						<th class="px-3 py-2 font-medium">State</th>
						{#if app.role === 'admin'}<th class="px-3 py-2 font-medium">Actions</th>{/if}
					</tr>
				</thead>
				<tbody>
					{#each snap.shares as s (s.namespace + '/' + s.name)}
						<tr class="border-t border-slate-800/60">
							<td class="px-3 py-2 font-medium text-slate-200">{s.name}</td>
							<td class="px-3 py-2 text-slate-400">{s.claim}</td>
							<td class="px-3 py-2">
								{#if s.node}
									{#if s.nfs}
										<div class="font-mono text-xs text-slate-300">{nfsPath(s)}</div>
									{/if}
									{#if s.smb}
										<div class="font-mono text-xs text-slate-300">{smbPath(s)}</div>
									{/if}
								{:else}
									<span class="text-slate-500">
										{[s.nfs && 'NFS', s.smb && 'SMB'].filter(Boolean).join(' + ')} (not placed)
									</span>
								{/if}
							</td>
							<td class="px-3 py-2 text-slate-400">{s.node || '-'}</td>
							<td class="px-3 py-2">
								<StatusBadge
									kind={s.available ? 'ok' : s.state === 'Failed' ? 'bad' : 'warn'}
									label={s.available ? s.state : s.reason || s.state}
								/>
							</td>
							{#if app.role === 'admin'}
								<td class="px-3 py-2">
									<button
										class="rounded bg-red-500/10 px-1.5 py-0.5 text-xs text-red-400 hover:bg-red-500/20"
										onclick={() => deleteShare(s.name)}
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
			<h2 class="mb-3 text-sm font-medium text-slate-300">New share</h2>
			<form onsubmit={createShare} class="space-y-2">
				<input
					class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
					placeholder="Name"
					bind:value={name}
				/>
				<select
					class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm focus:border-sky-500 focus:outline-none"
					bind:value={claim}
				>
					<option value="" disabled>Volume</option>
					{#each fsVolumes as v (v.namespace + '/' + v.name)}
						<option value={v.name}>{v.name}</option>
					{/each}
				</select>
				<input
					class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
					placeholder="NFS clients (comma separated)"
					bind:value={nfsClients}
				/>
				<label class="flex items-center gap-2 text-sm text-slate-300">
					<input type="checkbox" bind:checked={smb} /> SMB
				</label>
				<button
					class="w-full rounded-md bg-sky-600 px-2 py-1.5 text-sm font-medium text-white hover:bg-sky-500"
					type="submit"
				>
					Create share
				</button>
			</form>
		</section>
	{/if}
</div>
