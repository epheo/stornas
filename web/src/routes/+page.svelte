<script lang="ts">
	import {
		Server,
		Database,
		HardDrive,
		FolderOpen,
		Cable,
		Camera,
		CircleCheck,
		Circle,
		TriangleAlert,
	} from 'lucide-svelte';
	import { app } from '$lib/state.svelte';
	import { formatBytes } from '$lib/stream';
	import StatTile from '$lib/ui/StatTile.svelte';
	import Meter from '$lib/ui/Meter.svelte';

	const snap = $derived(app.snap);
	const readyNodes = $derived(snap.nodes.filter((n) => n.ready).length);
	const sickPools = $derived(snap.pools.filter((p) => p.health !== 'Online' || !p.available));
	const sickShares = $derived(snap.shares.filter((s) => !s.available));
	const sickTargets = $derived(snap.targets.filter((t) => !t.available));
	const downNodes = $derived(new Set(snap.nodes.filter((n) => !n.ready).map((n) => n.name)));
	// Every replica on a down node = no path to the data (failure matrix:
	// stated plainly, not hidden behind a stale phase).
	const strandedVolumes = $derived(
		snap.volumes.filter((v) => {
			const reps = v.replication?.replicas ?? [];
			return reps.length > 0 && reps.every((r) => downNodes.has(r.node));
		}),
	);
	const syncingVolumes = $derived(
		snap.volumes.filter((v) => v.replication?.replicas?.some((r) => r.diskState !== 'UpToDate')),
	);

	// First-run walkthrough: shown until storage exists, gone once volumes
	// do (exports stay a listed step, not a nag).
	const setupSteps = $derived([
		{
			label: 'Create a storage pool from free disks',
			href: '/pools',
			done: snap.pools.length > 0,
		},
		{ label: 'Create a volume on it', href: '/volumes', done: snap.volumes.length > 0 },
		{
			label: 'Export it: NFS/SMB share or iSCSI target',
			href: '/shares',
			done: snap.shares.length > 0 || snap.targets.length > 0,
		},
	]);
	const onboarding = $derived(snap.pools.length === 0 || snap.volumes.length === 0);

	type Issue = { text: string; href: string };
	const issues = $derived<Issue[]>([
		...snap.nodes
			.filter((n) => !n.ready)
			.map((n) => ({ text: `Node ${n.name} is not Ready`, href: '/nodes' })),
		...sickPools.map((p) => ({
			text: `Pool ${p.name}: ${p.health}${p.reason ? ` (${p.reason})` : ''}`,
			href: '/pools',
		})),
		...snap.volumes
			.filter((v) => v.replication?.splitBrain)
			.map((v) => ({
				text: `Volume ${v.name}: split brain, pick a survivor on the volumes page`,
				href: '/volumes',
			})),
		...strandedVolumes.map((v) => ({
			text: `Volume ${v.name} is unavailable: its node is down`,
			href: '/volumes',
		})),
		...syncingVolumes
			.filter((v) => !strandedVolumes.includes(v))
			.map((v) => {
				const syncing = v.replication?.replicas?.find((r) => r.syncPercent != null);
				return {
					text: syncing
						? `Volume ${v.name}: resyncing on ${syncing.node} (${syncing.syncPercent}%)`
						: `Volume ${v.name}: replica not UpToDate`,
					href: '/volumes',
				};
			}),
		...sickShares.map((s) => ({
			text: `Share ${s.name}: ${s.reason || s.state}`,
			href: '/shares',
		})),
		...sickTargets.map((t) => ({
			text: `Target ${t.name}: ${t.reason || t.state}`,
			href: '/targets',
		})),
	]);
</script>

<div class="space-y-6">
	<h1 class="text-xl font-semibold text-slate-100">Dashboard</h1>

	{#if onboarding && app.role === 'admin'}
		<section class="rounded-lg border border-sky-500/30 bg-slate-900 p-4">
			<h2 class="mb-1 text-sm font-medium text-slate-100">Set up your storage</h2>
			<p class="mb-3 text-sm text-slate-400">
				Three steps take this appliance from empty to serving clients.
			</p>
			<ol class="space-y-2">
				{#each setupSteps as step, i (step.href)}
					<li>
						<a class="flex items-center gap-2.5 text-sm hover:underline" href={step.href}>
							{#if step.done}
								<CircleCheck size={16} class="shrink-0 text-emerald-400" />
								<span class="text-slate-500 line-through">{i + 1}. {step.label}</span>
							{:else}
								<Circle size={16} class="shrink-0 text-sky-400" />
								<span class="text-slate-200">{i + 1}. {step.label}</span>
							{/if}
						</a>
					</li>
				{/each}
			</ol>
		</section>
	{/if}

	<div class="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-6">
		<StatTile
			label="Nodes"
			value="{readyNodes}/{snap.nodes.length}"
			note={readyNodes === snap.nodes.length ? 'all ready' : 'node down'}
			noteTone={readyNodes === snap.nodes.length ? 'ok' : 'bad'}
			icon={Server}
			href="/nodes"
		/>
		<StatTile
			label="Pools"
			value={snap.pools.length}
			note={sickPools.length ? `${sickPools.length} unhealthy` : ''}
			noteTone="bad"
			icon={Database}
			href="/pools"
		/>
		<StatTile label="Volumes" value={snap.volumes.length} icon={HardDrive} href="/volumes" />
		<StatTile
			label="Shares"
			value={snap.shares.length}
			note={sickShares.length ? `${sickShares.length} unavailable` : ''}
			noteTone="bad"
			icon={FolderOpen}
			href="/shares"
		/>
		<StatTile
			label="Targets"
			value={snap.targets.length}
			note={sickTargets.length ? `${sickTargets.length} unavailable` : ''}
			noteTone="bad"
			icon={Cable}
			href="/targets"
		/>
		<StatTile label="Snapshots" value={snap.snapshots.length} icon={Camera} href="/volumes" />
	</div>

	<div class="grid gap-4 lg:grid-cols-2">
		<section class="rounded-lg border border-slate-800 bg-slate-900 p-4">
			<h2 class="mb-3 text-sm font-medium text-slate-300">Capacity</h2>
			{#if snap.pools.length === 0}
				<p class="text-sm text-slate-500">
					No storage pools yet. <a class="text-sky-400 hover:underline" href="/pools"
						>Create one from free disks.</a
					>
				</p>
			{:else}
				<div class="space-y-3">
					{#each snap.pools as pool (pool.name)}
						{@const used = pool.capacityBytes - pool.freeBytes}
						<div>
							<div class="mb-1 flex items-baseline justify-between text-sm">
								<a class="font-medium text-slate-200 hover:underline" href="/pools">
									{pool.name}
									<span class="text-xs text-slate-500">on {pool.node}</span>
								</a>
								<span class="text-xs text-slate-400">
									{formatBytes(used)} of {formatBytes(pool.capacityBytes)}
								</span>
							</div>
							<Meter {used} total={pool.capacityBytes} />
						</div>
					{/each}
				</div>
			{/if}
		</section>

		<section class="rounded-lg border border-slate-800 bg-slate-900 p-4">
			<h2 class="mb-3 text-sm font-medium text-slate-300">Health</h2>
			{#if issues.length === 0}
				<p class="flex items-center gap-2 text-sm text-emerald-400">
					<CircleCheck size={16} /> All systems healthy.
				</p>
			{:else}
				<ul class="space-y-2">
					{#each issues as issue (issue.text)}
						<li>
							<a
								class="flex items-center gap-2 text-sm text-amber-400 hover:underline"
								href={issue.href}
							>
								<TriangleAlert size={14} class="shrink-0" />
								{issue.text}
							</a>
						</li>
					{/each}
				</ul>
			{/if}
		</section>
	</div>

	<div class="grid gap-4 lg:grid-cols-2">
		<section class="rounded-lg border border-slate-800 bg-slate-900 p-4">
			<div class="mb-3 flex items-baseline justify-between">
				<h2 class="text-sm font-medium text-slate-300">Recent alerts</h2>
				<a class="text-xs text-sky-400 hover:underline" href="/alerts">View all</a>
			</div>
			{#if snap.alerts.length === 0}
				<p class="text-sm text-slate-500">No warnings.</p>
			{:else}
				<ul class="space-y-1.5">
					{#each snap.alerts.slice(0, 5) as a (`${a.namespace}/${a.object}/${a.reason}`)}
						<li class="flex items-baseline gap-2 text-sm">
							<span class="shrink-0 text-xs tabular-nums text-slate-500">
								{a.lastSeen ? a.lastSeen.slice(5, 16).replace('T', ' ') : '-'}
							</span>
							<span class="shrink-0 font-medium text-amber-400">{a.reason}</span>
							<span class="truncate text-slate-400">{a.object}: {a.message}</span>
						</li>
					{/each}
				</ul>
			{/if}
		</section>

		<section class="rounded-lg border border-slate-800 bg-slate-900 p-4">
			<div class="mb-3 flex items-baseline justify-between">
				<h2 class="text-sm font-medium text-slate-300">Recent activity</h2>
				<a class="text-xs text-sky-400 hover:underline" href="/alerts">View all</a>
			</div>
			{#if snap.tasks.length === 0}
				<p class="text-sm text-slate-500">No actions yet.</p>
			{:else}
				<ul class="space-y-1.5">
					<!-- index key: an audit trail can repeat identical rows in one second -->
					{#each snap.tasks.slice(0, 5) as t, i (i)}
						<li class="flex items-baseline gap-2 text-sm">
							<span class="shrink-0 text-xs tabular-nums text-slate-500">
								{t.at ? t.at.slice(5, 16).replace('T', ' ') : '-'}
							</span>
							<span class="truncate text-slate-400">
								<span class="text-slate-300">{t.by || 'system'}</span>
								{t.verb}
								<span class="font-mono text-xs text-slate-300">{t.object}</span>
							</span>
							{#if !t.ok}<span class="shrink-0 text-xs font-medium text-red-400">failed</span>{/if}
						</li>
					{/each}
				</ul>
			{/if}
		</section>
	</div>
</div>
