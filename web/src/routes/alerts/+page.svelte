<script lang="ts">
	import { app } from '$lib/state.svelte';
	import StatusBadge from '$lib/ui/StatusBadge.svelte';

	const snap = $derived(app.snap);
</script>

<div class="space-y-6">
	<h1 class="text-xl font-semibold text-slate-100">Alerts</h1>
	<p class="text-sm text-slate-500">
		Warning events from the cluster, newest first. Normal activity is not shown.
	</p>

	{#if snap.alerts.length === 0}
		<p class="text-sm text-slate-500">No warnings.</p>
	{:else}
		<div class="overflow-x-auto rounded-lg border border-slate-800 bg-slate-900">
			<table class="w-full text-sm">
				<thead class="text-left text-xs text-slate-400">
					<tr class="border-b border-slate-800">
						<th class="px-3 py-2 font-medium">Last seen</th>
						<th class="px-3 py-2 font-medium">Count</th>
						<th class="px-3 py-2 font-medium">Namespace</th>
						<th class="px-3 py-2 font-medium">Object</th>
						<th class="px-3 py-2 font-medium">Reason</th>
						<th class="px-3 py-2 font-medium">Message</th>
					</tr>
				</thead>
				<tbody>
					{#each snap.alerts as a (`${a.namespace}/${a.object}/${a.reason}`)}
						<tr class="border-t border-slate-800/60 align-top">
							<td class="px-3 py-2 text-xs whitespace-nowrap tabular-nums text-slate-400">
								{a.lastSeen ? a.lastSeen.slice(0, 19).replace('T', ' ') : '-'}
							</td>
							<td class="px-3 py-2 tabular-nums text-slate-400">{a.count > 1 ? a.count : '-'}</td>
							<td class="px-3 py-2 text-slate-400">{a.namespace || '-'}</td>
							<td class="px-3 py-2 font-mono text-xs text-slate-300">{a.object}</td>
							<td class="px-3 py-2 font-medium whitespace-nowrap text-amber-400">{a.reason}</td>
							<td class="px-3 py-2 text-slate-300">{a.message}</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}

	<section>
		<h2 class="mb-2 text-sm font-medium text-slate-300">Activity</h2>
		<p class="mb-3 text-sm text-slate-500">
			Admin actions since the server started; the feed is not persisted.
		</p>
		{#if snap.tasks.length === 0}
			<p class="text-sm text-slate-500">No actions yet.</p>
		{:else}
			<div class="overflow-x-auto rounded-lg border border-slate-800 bg-slate-900">
				<table class="w-full text-sm">
					<thead class="text-left text-xs text-slate-400">
						<tr class="border-b border-slate-800">
							<th class="px-3 py-2 font-medium">Time</th>
							<th class="px-3 py-2 font-medium">User</th>
							<th class="px-3 py-2 font-medium">Action</th>
							<th class="px-3 py-2 font-medium">Object</th>
							<th class="px-3 py-2 font-medium">Result</th>
						</tr>
					</thead>
					<tbody>
						<!-- index key: an audit trail can repeat identical rows in one second -->
						{#each snap.tasks as t, i (i)}
							<tr class="border-t border-slate-800/60">
								<td class="px-3 py-2 text-xs whitespace-nowrap tabular-nums text-slate-400">
									{t.at ? t.at.slice(0, 19).replace('T', ' ') : '-'}
								</td>
								<td class="px-3 py-2 text-slate-300">{t.by || 'system'}</td>
								<td class="px-3 py-2 text-slate-400">{t.verb}</td>
								<td class="px-3 py-2 font-mono text-xs text-slate-300">{t.object}</td>
								<td class="px-3 py-2">
									<StatusBadge kind={t.ok ? 'ok' : 'bad'} label={t.ok ? 'ok' : 'failed'} />
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
	</section>
</div>
