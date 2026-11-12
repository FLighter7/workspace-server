import { describe, expect, it } from 'vitest';
import type { Pairing, PairingStore } from '../src/PairingStore.js';
import { MAIN_GRACE_MS, SyncEngine, type SyncAdapter, type SyncState } from '../src/SyncEngine.js';
import { FakeServer } from '../src/testing/FakeServer.js';
import { CODE, makePairing, settle } from './fixtures.js';

/*
 * A main and its subordinate, as two fake servers: the device pairs with
 * the main, learns the subordinate from the main's welcome, and syncs with
 * it when the main is out of reach. The subordinate admits the main's
 * devices, so the two share their device registry.
 */

interface Timer {
	callback: () => void;
	ms: number;
	cleared: boolean;
}

/** One deferred promise, for a server that answers only when the test says. */
function gate(): { promise: Promise<void>; open: () => void } {
	let open = (): void => undefined;
	const promise = new Promise<void>((resolve) => {
		open = resolve;
	});

	return { promise, open };
}

function setup(options: { fallbacks?: boolean } = {}) {
	const main = new FakeServer();
	main.role = 'main';
	main.port = 47801;
	const sub = new FakeServer();
	sub.serverId = 'S2';
	sub.name = 'Laptop';
	sub.hosts = ['b.local', '192.168.1.6'];
	sub.port = 47802;
	sub.fingerprint = Array.from({ length: 32 }, () => 'CD').join(':');
	sub.role = 'subordinate';
	sub.devices = main.devices;
	main.subordinates = [sub.entry()];

	const state = {
		pairing: makePairing({
			...main.entry(),
			fallbacks: options.fallbacks === false ? undefined : [sub.entry()],
		}) as Pairing | null,
		changeCount: 0,
	};
	if (state.pairing && !state.pairing.fallbacks) delete state.pairing.fallbacks;
	const timers: Timer[] = [];
	const states: SyncState[] = [];
	// A document that is never merged: an empty server gets one write.
	const adapter: SyncAdapter = {
		changeCount: () => state.changeCount,
		plan: (doc) => ({
			write: doc.etag === null ? new TextEncoder().encode('{"notes":[]}') : null,
		}),
	};
	const pairingStore: PairingStore = {
		load: () => (state.pairing ? structuredClone(state.pairing) : null),
		save: (pairing) => {
			state.pairing = structuredClone(pairing);
		},
		clear: () => {
			state.pairing = null;
		},
	};
	const engine = new SyncEngine({
		app: 'notes',
		pairingStore,
		adapter,
		onState: (next) => states.push(next),
		deviceName: () => 'iPhone Safari',
		connect: FakeServer.route(main, sub),
		now: () => new Date('2026-11-12T12:00:00.000Z'),
		timers: {
			set: (callback, ms) => {
				timers.push({ callback, ms, cleared: false });
				return timers.length - 1;
			},
			clear: (handle) => {
				timers[handle as number].cleared = true;
			},
		},
	});
	const pending = (): Timer[] => timers.filter((timer) => !timer.cleared);
	const last = (): SyncState => states[states.length - 1];

	/** Fires the pending timer of `ms`, and lets what it starts run. */
	async function fire(ms: number): Promise<void> {
		const timer = pending().find((t) => t.ms === ms);
		expect(timer).toBeDefined();
		if (!timer) return;
		timer.cleared = true;
		timer.callback();
		await settle();
	}

	return { main, sub, state, states, engine, pending, last, fire };
}

describe('SyncEngine with fallback servers', () => {
	it('learns the subordinates from the main’s welcome', async () => {
		const { engine, state, last, main, sub } = setup({ fallbacks: false });

		engine.start(true);
		await settle();

		expect(state.pairing?.fallbacks).toEqual([sub.entry()]);
		expect(last()).toMatchObject({ status: 'connected', serverName: 'Desk', via: null });
		expect(main.log.filter((entry) => entry === 'connect')).toHaveLength(1);
	});

	it('pairing keeps the subordinates the welcome lists', async () => {
		const { engine, state, main, sub } = setup({ fallbacks: false });
		state.pairing = null;
		engine.start(true);

		await engine.pair({ ...CODE, ...main.entry() });

		const saved = state.pairing as Pairing | null;
		expect(saved?.serverId).toBe(main.serverId);
		expect(saved?.fallbacks).toEqual([sub.entry()]);
	});

	it('takes the main at once when it answers as one, and closes the rest', async () => {
		const { engine, last, main, sub, pending } = setup();

		engine.start(true);
		await settle();

		expect(last()).toMatchObject({ status: 'connected', via: null });
		expect(main.log).toContain('head#1');
		expect(sub.log).toEqual(['connect', 'hello#1', 'close#1']);
		expect(pending()).toEqual([]);
	});

	it('prefers the main when it answers within the grace period', async () => {
		const { engine, last, main, sub, pending } = setup();
		const slow = gate();
		main.connectGate = slow.promise;

		engine.start(true);
		await settle();
		expect(sub.log).toEqual(['connect', 'hello#1']);
		expect(last().status).toBe('connecting');
		expect(pending().map((timer) => timer.ms)).toEqual([MAIN_GRACE_MS]);

		slow.open();
		await settle();

		expect(last()).toMatchObject({ status: 'connected', via: null });
		expect(main.log).toContain('head#1');
		expect(sub.log).toEqual(['connect', 'hello#1', 'close#1']);
		expect(pending()).toEqual([]);
	});

	it('falls back to a subordinate after the grace period while the main does not answer', async () => {
		const { engine, last, main, sub, state, fire } = setup();
		main.connectGate = new Promise(() => undefined); // away: the attempt hangs

		engine.start(true);
		await settle();
		await fire(MAIN_GRACE_MS);

		expect(last()).toMatchObject({ status: 'connected', serverName: 'Desk', via: 'Laptop' });
		expect(sub.log).toEqual(['connect', 'hello#1', 'head#1', 'get#1', 'put#1']);
		expect(state.pairing?.lastEtag).toBe(sub.etag);
	});

	it('falls back at once when the main has failed', async () => {
		const { engine, last, main, sub, pending } = setup();
		main.connectError = 'connectTimeout';

		engine.start(true);
		await settle();

		expect(last()).toMatchObject({ status: 'connected', via: 'Laptop' });
		expect(sub.log).toContain('head#1');
		expect(pending()).toEqual([]);
	});

	it('keeps what the subordinate’s welcome says about it, and the fallback list it lacks', async () => {
		const { engine, main, sub, state } = setup();
		main.connectError = 'connectTimeout';
		sub.hosts = ['b.local', '10.0.0.9'];

		engine.start(true);
		await settle();

		expect(state.pairing?.hosts).toEqual(main.hosts);
		expect(state.pairing?.fallbacks).toEqual([{ ...sub.entry(), hosts: ['b.local', '10.0.0.9'] }]);
	});

	it('shares lastEtag: a subordinate holding the same document is not read again', async () => {
		const { engine, main, sub, state } = setup();
		engine.start(true);
		await settle();
		expect(state.pairing?.lastEtag).toBe(main.etag);

		// The subordinate pulled the main's document: same bytes, same etag.
		sub.etag = main.etag;
		sub.body = main.body.slice();
		main.connectError = 'connectTimeout';
		engine.setVisible(false);
		engine.setVisible(true);
		await settle();

		expect(sub.log).toEqual(['connect', 'hello#1', 'close#1', 'connect', 'hello#2', 'head#2']);
	});

	it('drops the subordinates the main no longer lists', async () => {
		const { engine, main, state } = setup();
		main.subordinates = [];

		engine.start(true);
		await settle();

		expect(state.pairing).not.toHaveProperty('fallbacks');
	});

	it('does not take a welcome from a server outside the set', async () => {
		const { engine, last, main, sub, state } = setup();
		main.connectError = 'connectTimeout';
		sub.serverId = 'OTHER';

		engine.start(true);
		await settle();

		expect(last()).toMatchObject({ status: 'unreachable', via: null });
		expect(sub.log).toEqual(['connect', 'hello#1', 'close#1']);
		expect(state.pairing?.fallbacks).toHaveLength(1);
	});

	it('stops with serverChanged when the paired server’s address answers as another', async () => {
		const { engine, last, main, pending } = setup();
		main.serverId = 'OTHER';

		engine.start(true);
		await settle();

		expect(last()).toMatchObject({ status: 'error', error: { code: 'serverChanged' } });
		expect(pending()).toEqual([]);
	});

	it('a subordinate refusing the device does not stop sync: it may not know it yet', async () => {
		const { engine, last, main, sub, pending } = setup();
		main.connectError = 'connectTimeout';
		sub.helloError = 'badToken';

		engine.start(true);
		await settle();

		expect(last()).toMatchObject({ status: 'unreachable', error: { code: 'connectTimeout' } });
		expect(pending().map((timer) => timer.ms)).toEqual([2000]);
	});

	it('the main refusing the device stops sync, whatever a subordinate answers', async () => {
		const { engine, last, main, pending } = setup();
		main.helloError = 'badToken';

		engine.start(true);
		await settle();

		expect(last()).toMatchObject({ status: 'error', error: { code: 'badToken' } });
		expect(pending()).toEqual([]);
	});

	it('hiding while it waits for the main closes every attempt and its timer', async () => {
		const { engine, main, sub, pending } = setup();
		const slow = gate();
		main.connectGate = slow.promise;
		engine.start(true);
		await settle();

		engine.setVisible(false);
		await settle();
		expect(sub.log).toEqual(['connect', 'hello#1', 'close#1']);
		expect(pending()).toEqual([]);

		slow.open();
		await settle();
		expect(main.log).toEqual(['connect', 'close#1']);
	});

	it('a subordinate that answers and then goes away is not taken', async () => {
		const { engine, last, main, sub } = setup();
		main.connectError = 'connectTimeout';
		const slow = gate();
		sub.connectGate = slow.promise;
		engine.start(true);
		await settle();
		slow.open();
		await settle();
		expect(last().via).toBe('Laptop');

		sub.clients[0].drop();
		await settle();

		expect(last()).toMatchObject({ status: 'unreachable', via: null });
	});
});
