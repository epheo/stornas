<script lang="ts">
	import { onMount } from 'svelte';
	import { HardDrive, Server, Database, Camera, Cable, Users, LogOut } from 'lucide-svelte';
	import type { Snapshot } from '$lib/model.gen';
	import { connectState, formatBytes } from '$lib/stream';
	import { post, del } from '$lib/api';
	import CreateForms from '$lib/CreateForms.svelte';

	type LocalUser = { name: string; role: string; smb: boolean };

	let snap = $state<Snapshot>({
		pools: [],
		nodes: [],
		volumes: [],
		shares: [],
		targets: [],
		snapshots: [],
	});
	let role = $state('');
	let who = $state('');
	let users = $state<LocalUser[]>([]);
	let actionError = $state('');
	let userName = $state('');
	let userPassword = $state('');
	let userRole = $state('viewer');
	let userSMB = $state(false);

	async function loadUsers() {
		const r = await fetch('/api/v1/users').catch(() => undefined);
		if (r?.ok) users = await r.json();
	}

	onMount(() => {
		fetch('/api/v1/session')
			.then((r) => (r.ok ? r.json() : undefined))
			.then((s) => {
				role = s?.role ?? '';
				who = s?.username ?? s?.name ?? '';
				if (role === 'admin') loadUsers();
			});
		return connectState((s) => (snap = s));
	});

	async function logout() {
		await fetch('/api/v1/logout', { method: 'POST' }).catch(() => undefined);
		location.reload();
	}

	const healthClass: Record<string, string> = {
		Online: 'bg-emerald-100 text-emerald-800',
		Degraded: 'bg-amber-100 text-amber-800',
		Failed: 'bg-red-100 text-red-800',
		Unknown: 'bg-gray-100 text-gray-600',
	};

	async function run(fn: () => Promise<string>) {
		actionError = (await fn()) ?? '';
	}
	function deleteVolume(name: string) {
		if (confirm(`Delete volume ${name}? Data is lost.`)) {
			run(() => del(`/api/v1/volumes/${name}`));
		}
	}
	function resizeVolume(name: string, current: number) {
		const size = prompt(`New size for ${name} (currently ${formatBytes(current)}):`, '');
		if (size) run(() => post(`/api/v1/volumes/${name}/resize`, { size }));
	}
	function snapshotVolume(name: string) {
		const snapName = prompt('Snapshot name:', `${name}-${new Date().toISOString().slice(0, 10)}`);
		if (snapName) run(() => post('/api/v1/snapshots', { name: snapName, volume: name }));
	}
	function restoreSnapshot(name: string) {
		const volName = prompt('New volume name (restored from snapshot):', `${name}-restore`);
		if (volName) {
			run(() => post('/api/v1/volumes', { name: volName, size: '', fromSnapshot: name }));
		}
	}
	function deleteSnapshot(name: string) {
		if (confirm(`Delete snapshot ${name}?`)) run(() => del(`/api/v1/snapshots/${name}`));
	}
	function deleteShare(name: string) {
		if (confirm(`Delete share ${name}? Clients lose access.`)) {
			run(() => del(`/api/v1/shares/${name}`));
		}
	}
	function deleteTarget(name: string) {
		if (confirm(`Delete target ${name}? Initiators lose access.`)) {
			run(() => del(`/api/v1/targets/${name}`));
		}
	}
	async function createUser(e: Event) {
		e.preventDefault();
		actionError = await post('/api/v1/users', {
			name: userName,
			password: userPassword,
			role: userRole,
			smb: userSMB,
		});
		if (!actionError) {
			userName = '';
			userPassword = '';
			loadUsers();
		}
	}
	async function deleteUser(name: string) {
		if (!confirm(`Delete user ${name}?`)) return;
		actionError = await del(`/api/v1/users/${name}`);
		if (!actionError) loadUsers();
	}
</script>

{#snippet actionButton(label: string, danger: boolean, onclick: () => void)}
	<button
		class="rounded px-1.5 py-0.5 text-xs {danger
			? 'bg-red-50 text-red-700 hover:bg-red-100'
			: 'bg-gray-100 text-gray-700 hover:bg-gray-200'}"
		{onclick}
	>
		{label}
	</button>
{/snippet}

<main class="mx-auto max-w-6xl space-y-8 p-6">
	<header class="flex items-center gap-2">
		<HardDrive size={22} />
		<h1 class="text-xl font-semibold">stornas</h1>
		<div class="ml-auto flex items-center gap-3 text-sm">
			{#if who}<span class="opacity-60">{who} ({role})</span>{/if}
			<button class="flex items-center gap-1 rounded px-2 py-1 hover:bg-gray-100" onclick={logout}>
				<LogOut size={14} /> Sign out
			</button>
		</div>
	</header>

	{#if actionError}<p class="text-sm text-red-600">{actionError}</p>{/if}

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
			<Server size={14} /> Nodes and disks
		</h2>
		<div class="space-y-3">
			{#each snap.nodes as node (node.name)}
				<div class="rounded border border-gray-200">
					<div class="flex items-center gap-2 px-3 py-2 text-sm">
						<span
							class="inline-block h-2 w-2 rounded-full {node.ready
								? 'bg-emerald-500'
								: 'bg-red-500'}"
						></span>
						<span class="font-medium">{node.name}</span>
						<span class="opacity-60">{node.roles.join(', ')}</span>
						<span class="opacity-60">{node.addresses.join(' ')}</span>
						<span class="ml-auto text-xs opacity-50">{node.kubeletVersion}</span>
					</div>
					{#if (node.disks ?? []).length > 0}
						<table class="w-full border-t border-gray-100 text-sm">
							<thead class="bg-gray-50 text-left text-xs">
								<tr>
									<th class="px-3 py-1.5">Device</th>
									<th class="px-3 py-1.5">Model</th>
									<th class="px-3 py-1.5">Serial</th>
									<th class="px-3 py-1.5">Size</th>
									<th class="px-3 py-1.5">Type</th>
									<th class="px-3 py-1.5">Use</th>
								</tr>
							</thead>
							<tbody>
								{#each node.disks as d (d.path)}
									<tr class="border-t border-gray-100">
										<td class="px-3 py-1.5 font-mono text-xs">{d.path}</td>
										<td class="px-3 py-1.5">{d.model || '-'}</td>
										<td class="px-3 py-1.5 font-mono text-xs">{d.serial || '-'}</td>
										<td class="px-3 py-1.5">{formatBytes(d.sizeBytes)}</td>
										<td class="px-3 py-1.5">{d.rotational ? 'HDD' : 'SSD'}</td>
										<td class="px-3 py-1.5">
											<span
												class="rounded px-1.5 py-0.5 text-xs {d.claimed
													? 'bg-blue-100 text-blue-800'
													: 'bg-gray-100 text-gray-600'}"
											>
												{d.claimed ? 'in pool' : 'free'}
											</span>
										</td>
									</tr>
								{/each}
							</tbody>
						</table>
					{:else}
						<p class="border-t border-gray-100 px-3 py-1.5 text-xs opacity-60">
							No disks reported.
						</p>
					{/if}
				</div>
			{:else}
				<p class="text-sm opacity-60">No nodes.</p>
			{/each}
		</div>
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
							<th class="px-3 py-2">Replication</th>
							{#if role === 'admin'}<th class="px-3 py-2">Actions</th>{/if}
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
								<td class="px-3 py-2 text-xs">
									{#if vol.replication}
										{#each vol.replication.replicas as r (r.node)}
											<span
												class="mr-1 rounded px-1 py-0.5 {r.diskState === 'UpToDate'
													? 'bg-emerald-100 text-emerald-800'
													: 'bg-amber-100 text-amber-800'}"
												title={r.node}
											>
												{r.node}: {r.diskState}{r.inUse ? ' *' : ''}
											</span>
										{/each}
									{:else}
										<span class="opacity-50">local</span>
									{/if}
								</td>
								{#if role === 'admin'}
									<td class="space-x-1 px-3 py-2">
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
	</section>

	<section>
		<h2 class="mb-2 flex items-center gap-2 text-sm font-medium uppercase tracking-wide opacity-60">
			<Camera size={14} /> Snapshots
		</h2>
		{#if snap.snapshots.length === 0}
			<p class="text-sm opacity-60">No snapshots yet.</p>
		{:else}
			<div class="overflow-x-auto rounded border border-gray-200">
				<table class="w-full text-sm">
					<thead class="bg-gray-50 text-left">
						<tr>
							<th class="px-3 py-2">Name</th>
							<th class="px-3 py-2">Volume</th>
							<th class="px-3 py-2">Size</th>
							<th class="px-3 py-2">Created</th>
							<th class="px-3 py-2">Ready</th>
							{#if role === 'admin'}<th class="px-3 py-2">Actions</th>{/if}
						</tr>
					</thead>
					<tbody>
						{#each snap.snapshots as s (s.namespace + '/' + s.name)}
							<tr class="border-t border-gray-100">
								<td class="px-3 py-2 font-medium">{s.name}</td>
								<td class="px-3 py-2">{s.source}</td>
								<td class="px-3 py-2">{formatBytes(s.sizeBytes)}</td>
								<td class="px-3 py-2 text-xs">{s.createdAt ? s.createdAt.slice(0, 19) : '-'}</td>
								<td class="px-3 py-2">
									<span
										class="rounded px-1.5 py-0.5 text-xs {s.ready
											? 'bg-emerald-100 text-emerald-800'
											: 'bg-amber-100 text-amber-800'}"
									>
										{s.ready ? 'ready' : 'pending'}
									</span>
								</td>
								{#if role === 'admin'}
									<td class="space-x-1 px-3 py-2">
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
							{#if role === 'admin'}<th class="px-3 py-2">Actions</th>{/if}
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
								{#if role === 'admin'}
									<td class="px-3 py-2">
										{@render actionButton('delete', true, () => deleteShare(s.name))}
									</td>
								{/if}
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
	</section>

	<section>
		<h2 class="mb-2 flex items-center gap-2 text-sm font-medium uppercase tracking-wide opacity-60">
			<Cable size={14} /> iSCSI targets
		</h2>
		{#if snap.targets.length === 0}
			<p class="text-sm opacity-60">No targets yet.</p>
		{:else}
			<div class="overflow-x-auto rounded border border-gray-200">
				<table class="w-full text-sm">
					<thead class="bg-gray-50 text-left">
						<tr>
							<th class="px-3 py-2">Name</th>
							<th class="px-3 py-2">IQN</th>
							<th class="px-3 py-2">LUNs</th>
							<th class="px-3 py-2">Active node</th>
							<th class="px-3 py-2">VIP</th>
							<th class="px-3 py-2">Sessions</th>
							<th class="px-3 py-2">State</th>
							{#if role === 'admin'}<th class="px-3 py-2">Actions</th>{/if}
						</tr>
					</thead>
					<tbody>
						{#each snap.targets as t (t.namespace + '/' + t.name)}
							<tr class="border-t border-gray-100">
								<td class="px-3 py-2 font-medium">{t.name}</td>
								<td class="px-3 py-2 font-mono text-xs">{t.iqn || '-'}</td>
								<td class="px-3 py-2 text-xs">
									{#each t.luns ?? [] as l (l.id)}
										<span class="mr-1 rounded bg-gray-100 px-1 py-0.5">{l.id}: {l.claim}</span>
									{/each}
								</td>
								<td class="px-3 py-2">{t.activeNode || '-'}</td>
								<td class="px-3 py-2 font-mono text-xs">{t.vip || '-'}</td>
								<td class="px-3 py-2">{t.sessions}</td>
								<td class="px-3 py-2">{t.state}</td>
								{#if role === 'admin'}
									<td class="px-3 py-2">
										{@render actionButton('delete', true, () => deleteTarget(t.name))}
									</td>
								{/if}
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
	</section>
	{#if role === 'admin'}
		<section>
			<h2
				class="mb-2 flex items-center gap-2 text-sm font-medium uppercase tracking-wide opacity-60"
			>
				<Users size={14} /> Users
			</h2>
			<div class="grid gap-4 md:grid-cols-2">
				<div class="rounded border border-gray-200">
					<table class="w-full text-sm">
						<thead class="bg-gray-50 text-left">
							<tr>
								<th class="px-3 py-2">Name</th>
								<th class="px-3 py-2">Role</th>
								<th class="px-3 py-2">SMB</th>
								<th class="px-3 py-2">Actions</th>
							</tr>
						</thead>
						<tbody>
							{#each users as u (u.name)}
								<tr class="border-t border-gray-100">
									<td class="px-3 py-2 font-medium">{u.name}</td>
									<td class="px-3 py-2">{u.role}</td>
									<td class="px-3 py-2">{u.smb ? 'yes' : '-'}</td>
									<td class="px-3 py-2">
										{#if u.name !== 'admin'}
											{@render actionButton('delete', true, () => deleteUser(u.name))}
										{/if}
									</td>
								</tr>
							{:else}
								<tr><td class="px-3 py-2 text-sm opacity-60">No users.</td></tr>
							{/each}
						</tbody>
					</table>
				</div>
				<form onsubmit={createUser} class="space-y-2 rounded border border-gray-200 p-4">
					<h3 class="text-sm font-semibold">New user</h3>
					<input
						class="w-full rounded border px-2 py-1 text-sm"
						placeholder="Username"
						autocomplete="off"
						bind:value={userName}
					/>
					<input
						class="w-full rounded border px-2 py-1 text-sm"
						type="password"
						placeholder="Password"
						autocomplete="new-password"
						bind:value={userPassword}
					/>
					<select class="w-full rounded border px-2 py-1 text-sm" bind:value={userRole}>
						<option value="viewer">viewer</option>
						<option value="admin">admin</option>
					</select>
					<label class="flex items-center gap-2 text-sm">
						<input type="checkbox" bind:checked={userSMB} /> SMB access (share logins)
					</label>
					<button class="w-full rounded bg-gray-900 px-2 py-1 text-sm text-white" type="submit"
						>Create user</button
					>
				</form>
			</div>
		</section>
	{/if}
</main>
