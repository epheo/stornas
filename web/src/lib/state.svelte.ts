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

export async function loadSession(): Promise<void> {
	const r = await fetch('/api/v1/session').catch(() => undefined);
	if (!r?.ok) return;
	const s = await r.json();
	app.role = s?.role ?? '';
	app.who = s?.username ?? s?.name ?? '';
}
