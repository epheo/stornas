import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Snapshot } from './model.gen';
import { connectState, formatBytes } from './stream';

describe('formatBytes', () => {
	it('renders human sizes', () => {
		expect(formatBytes(0)).toBe('-');
		expect(formatBytes(-5)).toBe('-');
		expect(formatBytes(512)).toBe('512 B');
		expect(formatBytes(1536)).toBe('1.5 KiB');
		expect(formatBytes(100 * 2 ** 30)).toBe('100 GiB');
	});
});

class FakeWS {
	static instances: FakeWS[] = [];
	onmessage: ((e: { data: string }) => void) | undefined;
	onclose: (() => void) | undefined;
	closed = false;
	constructor(public url: string) {
		FakeWS.instances.push(this);
	}
	close() {
		this.closed = true;
	}
}

function deferred<T>() {
	let resolve!: (v: T) => void;
	const promise = new Promise<T>((r) => (resolve = r));
	return { promise, resolve };
}

const flush = () => new Promise((r) => setTimeout(r, 0));
const snap = (id: string) => ({ marker: id }) as unknown as Snapshot;

describe('connectState', () => {
	beforeEach(() => {
		FakeWS.instances = [];
		vi.stubGlobal('WebSocket', FakeWS);
		vi.stubGlobal('location', { protocol: 'http:', host: 'appliance' });
	});
	afterEach(() => vi.unstubAllGlobals());

	it('paints the HTTP seed before the socket delivers', async () => {
		const seed = deferred<unknown>();
		vi.stubGlobal(
			'fetch',
			vi.fn(() => seed.promise),
		);
		const frames: Snapshot[] = [];
		const stop = connectState((s) => frames.push(s));

		seed.resolve({ ok: true, json: () => Promise.resolve(snap('seed')) });
		await flush();
		expect(frames).toEqual([snap('seed')]);

		FakeWS.instances[0].onmessage?.({ data: JSON.stringify({ snapshot: snap('ws') }) });
		expect(frames).toEqual([snap('seed'), snap('ws')]);
		stop();
	});

	it('drops a seed that loses the race against the first frame', async () => {
		const seed = deferred<unknown>();
		vi.stubGlobal(
			'fetch',
			vi.fn(() => seed.promise),
		);
		const frames: Snapshot[] = [];
		const stop = connectState((s) => frames.push(s));

		FakeWS.instances[0].onmessage?.({ data: JSON.stringify({ snapshot: snap('ws') }) });
		seed.resolve({ ok: true, json: () => Promise.resolve(snap('stale-seed')) });
		await flush();
		expect(frames).toEqual([snap('ws')]);
		stop();
	});

	it('ignores a seed arriving after stop', async () => {
		const seed = deferred<unknown>();
		vi.stubGlobal(
			'fetch',
			vi.fn(() => seed.promise),
		);
		const frames: Snapshot[] = [];
		const stop = connectState((s) => frames.push(s));
		stop();

		seed.resolve({ ok: true, json: () => Promise.resolve(snap('late')) });
		await flush();
		expect(frames).toEqual([]);
		expect(FakeWS.instances[0].closed).toBe(true);
	});
});
