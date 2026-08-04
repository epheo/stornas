import type { Snapshot } from './model.gen';

// Live snapshot feed: seed over HTTP so the first paint never waits on the
// socket, then follow the WS stream, reconnecting with a flat 2s delay (the
// server conflates frames, so a reconnect never replays a backlog).
export function connectState(onFrame: (s: Snapshot) => void): () => void {
	let ws: WebSocket | undefined;
	let timer: ReturnType<typeof setTimeout> | undefined;
	let closed = false;

	fetch('/api/v1/state')
		.then((r) => (r.ok ? r.json() : undefined))
		.then((s) => s && !closed && onFrame(s))
		.catch(() => {});

	function open() {
		const proto = location.protocol === 'https:' ? 'wss' : 'ws';
		ws = new WebSocket(`${proto}://${location.host}/api/v1/stream`);
		ws.onmessage = (e) => onFrame(JSON.parse(e.data).snapshot);
		ws.onclose = () => {
			if (!closed) timer = setTimeout(open, 2000);
		};
	}
	open();

	return () => {
		closed = true;
		if (timer) clearTimeout(timer);
		ws?.close();
	};
}

export function formatBytes(n: number): string {
	if (n <= 0) return '-';
	const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
	let i = 0;
	let v = n;
	while (v >= 1024 && i < units.length - 1) {
		v /= 1024;
		i++;
	}
	return `${v >= 100 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}
