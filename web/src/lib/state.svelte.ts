// Shared live app state: one WS stream feeds every page. Pages read
// app.snap; the layout owns the connection and the session identity.
import type { Snapshot } from './model.gen';
import { connectState } from './stream';

export const app = $state({
	snap: {
		pools: [],
		nodes: [],
		volumes: [],
		shares: [],
		targets: [],
		snapshots: [],
		alerts: [],
		tasks: [],
	} as Snapshot,
	who: '',
	role: '',
	// True while the account still runs on the console-logged generated
	// password; the layout nudges a change.
	mustChangePassword: false,
});

// Go marshals nil slices as null; normalize once so pages can iterate.
export function startStream(): () => void {
	return connectState((s) => {
		app.snap = {
			pools: s.pools ?? [],
			nodes: s.nodes ?? [],
			volumes: s.volumes ?? [],
			shares: s.shares ?? [],
			targets: s.targets ?? [],
			snapshots: s.snapshots ?? [],
			alerts: s.alerts ?? [],
			tasks: s.tasks ?? [],
		};
	});
}

// Returns whether a session exists, so the layout's gate check can reuse
// this fetch instead of probing /api/v1/session a second time.
export async function loadSession(): Promise<boolean> {
	const r = await fetch('/api/v1/session').catch(() => undefined);
	if (!r?.ok) return false;
	const s = await r.json();
	app.role = s?.role ?? '';
	app.who = s?.username ?? s?.name ?? '';
	app.mustChangePassword = s?.mustChangePassword ?? false;
	return true;
}
