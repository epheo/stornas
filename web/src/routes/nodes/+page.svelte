<script lang="ts">
	import { app } from '$lib/state.svelte';
	import { formatBytes } from '$lib/stream';
	import StatusBadge from '$lib/ui/StatusBadge.svelte';

	const snap = $derived(app.snap);
</script>

<div class="space-y-6">
	<h1 class="text-xl font-semibold text-slate-100">Nodes</h1>

	<div class="space-y-3">
		{#each snap.nodes as node (node.name)}
			<section class="rounded-lg border border-slate-800 bg-slate-900">
				<div class="flex flex-wrap items-center gap-3 px-4 py-3 text-sm">
					<StatusBadge kind={node.ready ? 'ok' : 'bad'} label={node.ready ? 'Ready' : 'NotReady'} />
					<span class="font-medium text-slate-100">{node.name}</span>
					<span class="text-slate-400">{node.roles.join(', ')}</span>
					<span class="font-mono text-xs text-slate-400">{node.addresses.join(' ')}</span>
					<span class="ml-auto text-xs text-slate-500">{node.kubeletVersion}</span>
				</div>
				{#if (node.disks ?? []).length > 0}
					<div class="overflow-x-auto border-t border-slate-800">
						<table class="w-full text-sm">
							<thead class="text-left text-xs text-slate-400">
								<tr>
									<th class="px-4 py-1.5 font-medium">Device</th>
									<th class="px-3 py-1.5 font-medium">Model</th>
									<th class="px-3 py-1.5 font-medium">Serial</th>
									<th class="px-3 py-1.5 font-medium">Size</th>
									<th class="px-3 py-1.5 font-medium">Type</th>
									<th class="px-3 py-1.5 font-medium">Use</th>
								</tr>
							</thead>
							<tbody>
								{#each node.disks as d (d.path)}
									<tr class="border-t border-slate-800/60">
										<td class="px-4 py-1.5 font-mono text-xs text-slate-300">{d.path}</td>
										<td class="px-3 py-1.5 text-slate-400">{d.model || '-'}</td>
										<td class="px-3 py-1.5 font-mono text-xs text-slate-400">{d.serial || '-'}</td>
										<td class="px-3 py-1.5 tabular-nums text-slate-300">
											{formatBytes(d.sizeBytes)}
										</td>
										<td class="px-3 py-1.5 text-slate-400">{d.rotational ? 'HDD' : 'SSD'}</td>
										<td class="px-3 py-1.5">
											<span
												class="rounded px-1.5 py-0.5 text-xs {d.claimed
													? 'bg-sky-500/10 text-sky-400'
													: 'bg-slate-800 text-slate-400'}"
											>
												{d.claimed ? 'in pool' : 'free'}
											</span>
										</td>
									</tr>
								{/each}
							</tbody>
						</table>
					</div>
				{:else}
					<p class="border-t border-slate-800 px-4 py-2 text-xs text-slate-500">
						No disks reported.
					</p>
				{/if}
			</section>
		{:else}
			<p class="text-sm text-slate-500">No nodes.</p>
		{/each}
	</div>
</div>
