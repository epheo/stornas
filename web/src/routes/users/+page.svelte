<script lang="ts">
	import { onMount } from 'svelte';
	import { app } from '$lib/state.svelte';
	import { post, del } from '$lib/api';
	import { toasts } from '$lib/toast.svelte';
	import ConfirmDialog from '$lib/ui/ConfirmDialog.svelte';

	type LocalUser = { name: string; role: string; smb: boolean };

	let users = $state<LocalUser[]>([]);
	let actionError = $state('');
	let name = $state('');
	let password = $state('');
	let role = $state('viewer');
	let smb = $state(false);

	async function loadUsers() {
		const r = await fetch('/api/v1/users').catch(() => undefined);
		if (r?.ok) users = (await r.json()) ?? [];
	}
	onMount(loadUsers);

	async function createUser(e: Event) {
		e.preventDefault();
		actionError = await post('/api/v1/users', { name, password, role, smb });
		if (!actionError) {
			toasts.show(`User ${name} created`, 'success');
			name = '';
			password = '';
			loadUsers();
		}
	}

	let deleting = $state('');
	let deleteError = $state('');
	let busy = $state(false);

	async function deleteUser() {
		busy = true;
		deleteError = '';
		const err = await del(`/api/v1/users/${deleting}`);
		busy = false;
		if (err) deleteError = err;
		else {
			toasts.show(`User ${deleting} deleted`, 'success');
			deleting = '';
			loadUsers();
		}
	}
</script>

<div class="space-y-6">
	<h1 class="text-xl font-semibold text-slate-100">Users</h1>

	{#if app.role !== 'admin'}
		<p class="text-sm text-slate-500">User management needs the admin role.</p>
	{:else}
		{#if actionError}<p class="text-sm text-red-400">{actionError}</p>{/if}

		<div class="grid max-w-3xl gap-4 md:grid-cols-2">
			<div class="rounded-lg border border-slate-800 bg-slate-900">
				<table class="w-full text-sm">
					<thead class="text-left text-xs text-slate-400">
						<tr class="border-b border-slate-800">
							<th class="px-3 py-2 font-medium">Name</th>
							<th class="px-3 py-2 font-medium">Role</th>
							<th class="px-3 py-2 font-medium">SMB</th>
							<th class="px-3 py-2 font-medium">Actions</th>
						</tr>
					</thead>
					<tbody>
						{#each users as u (u.name)}
							<tr class="border-t border-slate-800/60">
								<td class="px-3 py-2 font-medium text-slate-200">{u.name}</td>
								<td class="px-3 py-2 text-slate-400">{u.role}</td>
								<td class="px-3 py-2 text-slate-400">{u.smb ? 'yes' : '-'}</td>
								<td class="px-3 py-2">
									{#if u.name !== 'admin'}
										<button
											class="rounded bg-red-500/10 px-1.5 py-0.5 text-xs text-red-400 hover:bg-red-500/20"
											onclick={() => ((deleting = u.name), (deleteError = ''))}
										>
											delete
										</button>
									{/if}
								</td>
							</tr>
						{:else}
							<tr><td class="px-3 py-2 text-sm text-slate-500" colspan="4">No users.</td></tr>
						{/each}
					</tbody>
				</table>
			</div>

			<form
				onsubmit={createUser}
				class="space-y-2 self-start rounded-lg border border-slate-800 bg-slate-900 p-4"
			>
				<h2 class="text-sm font-medium text-slate-300">New user</h2>
				<input
					class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
					placeholder="Username"
					autocomplete="off"
					bind:value={name}
				/>
				<input
					class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm placeholder:text-slate-500 focus:border-sky-500 focus:outline-none"
					type="password"
					placeholder="Password"
					autocomplete="new-password"
					bind:value={password}
				/>
				<select
					class="w-full rounded-md border border-slate-700 bg-slate-800 px-2 py-1.5 text-sm focus:border-sky-500 focus:outline-none"
					bind:value={role}
				>
					<option value="viewer">viewer</option>
					<option value="admin">admin</option>
				</select>
				<label class="flex items-center gap-2 text-sm text-slate-300">
					<input type="checkbox" bind:checked={smb} /> SMB access (share logins)
				</label>
				<button
					class="w-full rounded-md bg-sky-600 px-2 py-1.5 text-sm font-medium text-white hover:bg-sky-500"
					type="submit"
				>
					Create user
				</button>
			</form>
		</div>
	{/if}
</div>

{#if deleting}
	<ConfirmDialog
		title="Delete user"
		{busy}
		error={deleteError}
		onconfirm={deleteUser}
		onclose={() => (deleting = '')}
	>
		<p>
			User <span class="font-mono text-slate-200">{deleting}</span> loses UI and SMB access immediately.
		</p>
	</ConfirmDialog>
{/if}
