import { describe, expect, it } from 'vitest';
import { SyncEngine, type SyncState } from '../src/SyncEngine.js';
import { FakeServer } from '../src/testing/FakeServer.js';
import { DATA, encodeNotes, settle, setup, type DocCode } from './engineHarness.js';
import { makePairing } from './fixtures.js';

/*
 * A divergence of the user's servers as the engine shows it
 * (docs/protocol.md, "Divergence"): the welcome and the `diverged` pushes
 * name what waits, `resolveDivergence` answers it, and `resolved` closes the
 * question on every device.
 */

const SINCE = '2026-11-13T10:00:00.123456789Z';

/** A main (the desk) and its subordinate (the laptop), whose copy diverged from the desk's. */
function family() {
	const main = new FakeServer();
	main.role = 'main';
	const sub = new FakeServer();
	Object.assign(sub, { serverId: 'S2', name: 'Laptop', port: 47802, role: 'subordinate' });
	sub.fingerprint = Array.from({ length: 32 }, () => 'CD').join(':');
	sub.hosts = ['b.local', '192.168.1.6'];
	sub.devices = main.devices;
	main.subordinates = [sub.entry()];
	const laptop = { serverId: 'S2', name: 'Laptop', since: SINCE };
	main.diverged = [laptop];
	sub.diverged = [laptop];

	return { main, sub, laptop };
}

/** A device paired with `main`, reaching `servers`. */
function device(main: FakeServer, ...servers: FakeServer[]) {
	const d = setup({ server: main, notes: ['a'], pairing: makePairing({ ...main.entry() }) });
	const states: SyncState<DocCode>[] = [];
	const engine = new SyncEngine({
		...d.engineOptions,
		connect: FakeServer.route(main, ...servers),
		onState: (state) => states.push(state),
	});
	return { ...d, engine, states, last: () => states[states.length - 1] };
}

describe('SyncEngine and a divergence of the servers', () => {
	it('names the divergence the welcome lists, with both servers’ names, and keeps syncing', async () => {
		const { main, laptop } = family();
		const phone = device(main);

		phone.engine.start(true);
		await settle();

		expect(phone.last()).toMatchObject({
			status: 'connected',
			diverged: [{ ...laptop, mainName: 'Desk' }],
		});
		expect(main.text(DATA)).not.toBeNull();
	});

	it('names the main as the paired server while it syncs through the subordinate', async () => {
		const { main, sub, laptop } = family();
		main.connectError = 'connectTimeout';
		const phone = device(main, sub);
		phone.state.pairing = makePairing({ ...main.entry(), fallbacks: [sub.entry()] });

		phone.engine.start(true);
		await settle();

		expect(phone.last()).toMatchObject({
			via: 'Laptop',
			diverged: [{ ...laptop, mainName: 'Desk' }],
		});
	});

	it('follows the diverged pushes, and an empty list clears it', async () => {
		const { main, laptop } = family();
		main.diverged = [];
		const phone = device(main);
		phone.engine.start(true);
		await settle();
		expect(phone.last().diverged).toEqual([]);

		main.setDiverged([laptop]);
		expect(phone.last().diverged).toEqual([{ ...laptop, mainName: 'Desk' }]);

		main.setDiverged([]);
		expect(phone.last().diverged).toEqual([]);
	});

	it('resolves a divergence: the server takes the choice, and the question closes', async () => {
		const { main, laptop } = family();
		main.onResolve = (_server, keep) => {
			// Keeping the main's copy makes the laptop's equal to it; the desk's
			// set changes when the laptop's is kept, as a write would.
			if (keep === 'subordinate') main.storeDocument(DATA, encodeNotes(['a', 'laptop']));
		};
		const phone = device(main);
		phone.engine.start(true);
		await settle();

		await phone.engine.resolveDivergence(laptop.serverId, 'subordinate');
		main.pushChanged();
		await settle();

		expect(main.resolutions).toEqual([{ server: 'S2', keep: 'subordinate', device: 'dev1' }]);
		expect(phone.last().diverged).toEqual([]);
		// The copy kept reaches the device like any other write.
		expect(phone.local.notes).toEqual(['a', 'laptop']);
	});

	it('closes the question when another device chose first', async () => {
		const { main, laptop } = family();
		main.devices.set('dev2', 'tok2');
		const phone = device(main);
		const tablet = setup({ server: main, pairing: makePairing({ device: 'dev2', token: 'tok2' }) });
		phone.engine.start(true);
		tablet.engine.start(true);
		await settle();
		expect(phone.last().diverged).toHaveLength(1);
		expect(tablet.last().diverged).toHaveLength(1);

		await tablet.engine.resolveDivergence(laptop.serverId, 'main');

		expect(phone.last().diverged).toEqual([]);
		expect(tablet.last().diverged).toEqual([]);
		// A later choice changes nothing, and is answered all the same.
		await phone.engine.resolveDivergence(laptop.serverId, 'subordinate');
		expect(main.resolutions).toEqual([{ server: 'S2', keep: 'main', device: 'dev2' }]);
	});

	it('keeps the question while the connection is down, and refuses an answer then', async () => {
		const { main, laptop } = family();
		const phone = device(main);
		phone.engine.start(true);
		await settle();

		main.clients[0].drop();
		await settle();

		expect(phone.last()).toMatchObject({ status: 'unreachable', diverged: [{ ...laptop }] });
		await expect(phone.engine.resolveDivergence(laptop.serverId, 'main')).rejects.toMatchObject({
			code: 'closed',
		});
	});

	it('the next server reached replaces the list, and pairing again or disconnecting clears it', async () => {
		const { main, laptop } = family();
		const phone = device(main);
		phone.engine.start(true);
		await settle();
		expect(phone.last().diverged).toEqual([{ ...laptop, mainName: 'Desk' }]);

		main.diverged = [];
		phone.engine.setVisible(false);
		phone.engine.setVisible(true);
		await settle();
		expect(phone.last().diverged).toEqual([]);

		main.diverged = [laptop];
		phone.engine.setVisible(false);
		phone.engine.setVisible(true);
		await settle();
		await phone.engine.disconnect();
		expect(phone.last().diverged).toEqual([]);
	});
});
