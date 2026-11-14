import { describe, expect, it, vi } from 'vitest';
import {
	SMALL_TRANSFER_BYTES,
	SyncEngine,
	type SyncState,
	type Transfer,
} from '../src/SyncEngine.js';
import { FakeClient, FakeServer } from '../src/testing/FakeServer.js';
import { settle, setup } from './engineHarness.js';
import type { Pairing } from '../src/PairingStore.js';
import { makePairing } from './fixtures.js';

/*
 * Transfers cut off with their connection (docs/protocol.md, "Transactions"
 * and "Reading"): a small one starts over by itself, a large one waits for
 * the app's choice, to resume from what arrived or to start over.
 */

const PHOTO = { path: 'photos', id: 'p1', type: 'jpg' };
const NOTE = { path: 'notes', id: 'n1', type: 'md' };
const MiB = 1024 * 1024;

/** `size` bytes that are not all alike. */
function bytes(size: number, seed = 0): Uint8Array {
	return Uint8Array.from({ length: size }, (_, i) => (i * 7 + seed) % 251);
}

function same(a: Uint8Array | null, b: Uint8Array): boolean {
	return a !== null && a.byteLength === b.byteLength && a.every((byte, i) => byte === b[i]);
}

/** The transfers the state told, in order, without repeats. */
function transfers(states: { transfer: Transfer | null }[]): (Transfer | null)[] {
	const out: (Transfer | null)[] = [];
	for (const { transfer } of states) {
		const prev = out[out.length - 1];
		if (JSON.stringify(prev) !== JSON.stringify(transfer)) out.push(transfer);
	}
	return out;
}

describe('SyncEngine transfers', () => {
	it('tells an upload’s progress, and drops it once done', async () => {
		const device = setup({ files: true });
		const body = bytes(3 * MiB);
		device.writeFile(PHOTO, body);

		device.engine.start(true);
		await settle();

		const told = transfers(device.states).filter((t) => t !== null);
		expect(told[0]).toEqual({ direction: 'up', done: 0, total: 3 * MiB, interrupted: false });
		expect(told.at(-1)).toEqual({
			direction: 'up',
			done: 3 * MiB,
			total: 3 * MiB,
			interrupted: false,
		});
		expect(device.last().transfer).toBeNull();
		expect(same(device.server.document(PHOTO), body)).toBe(true);
		// A finished upload leaves nothing to resume.
		expect(device.state.pairing).not.toHaveProperty('upload');
	});

	it('a small upload cut off starts over by itself in another transaction', async () => {
		const device = setup({ files: true });
		device.writeFile(NOTE, bytes(1000));
		device.server.cutPutAfter = 400;

		device.engine.start(true);
		await settle();
		expect(device.last()).toMatchObject({ status: 'unreachable', transfer: null });
		expect(device.state.pairing).not.toHaveProperty('upload');

		await device.fire(2000);

		expect(device.last()).toMatchObject({ status: 'connected', transfer: null });
		expect(device.server.puts.map((put) => put.offset)).toEqual([0, 0]);
		expect(same(device.server.document(NOTE), bytes(1000))).toBe(true);
	});

	it('a large upload cut off waits, then resumes from what the server received', async () => {
		const device = setup({ files: true });
		const body = bytes(3 * MiB);
		device.writeFile(NOTE, 'small');
		device.writeFile(PHOTO, body);
		device.server.cutPutAfter = MiB + 5;

		device.engine.start(true);
		await settle();

		const total = 3 * MiB + 5;
		expect(device.last()).toMatchObject({
			status: 'unreachable',
			transfer: { direction: 'up', total, interrupted: true },
		});
		// Saved, so the upload resumes after a reload too.
		const upload = device.state.pairing?.upload;
		expect(upload).toMatchObject({ total, bodies: [{ path: 'notes' }, { path: 'photos' }] });
		expect(upload?.done).toBe(device.last().transfer?.done);
		const tx = upload?.tx ?? '';

		// Connected again, it waits for the app's choice, whatever comes.
		await device.fire(2000);
		expect(device.last()).toMatchObject({ status: 'interrupted', transfer: { interrupted: true } });
		device.writeFile({ path: 'notes', id: 'n2', type: 'md' }, 'later');
		await device.fire();
		await device.engine.syncNow();
		expect(device.server.log.filter((entry) => entry.startsWith('head'))).toHaveLength(1);
		expect(device.last().status).toBe('interrupted');

		await device.engine.resumeTransfer();

		expect(device.server.log).toContain('begin#2');
		const resumed = device.server.puts.filter((put) => put.key.id === 'p1');
		expect(resumed.map((put) => put.offset)).toEqual([0, MiB]);
		expect(resumed[1].body.byteLength).toBe(2 * MiB);
		expect(device.server.transactions.size).toBe(0);
		expect([...device.server.log].reverse().find((e) => e.startsWith('begin'))).toBe('begin#2');
		expect(same(device.server.document(PHOTO), body)).toBe(true);
		expect(device.server.text({ path: 'notes', id: 'n2', type: 'md' })).toBe('later');
		expect(device.last()).toMatchObject({ status: 'connected', transfer: null, error: null });
		expect(device.state.pairing).not.toHaveProperty('upload');
		// The same transaction: the resume began with its id.
		expect(tx).not.toBe('');
	});

	it('a large upload cut off can be started over', async () => {
		const device = setup({ files: true });
		device.writeFile(PHOTO, bytes(3 * MiB));
		device.server.cutPutAfter = MiB;

		device.engine.start(true);
		await settle();
		await device.fire(2000);
		const tx = device.state.pairing?.upload?.tx;

		await device.engine.restartTransfer();

		expect(device.server.puts.map((put) => put.offset)).toEqual([0, 0]);
		expect([...device.server.transactions.keys()]).not.toContain(tx);
		expect(same(device.server.document(PHOTO), bytes(3 * MiB))).toBe(true);
		expect(device.last()).toMatchObject({ status: 'connected', transfer: null });
		expect(device.state.pairing).not.toHaveProperty('upload');
	});

	it('a large upload cut off by a reload resumes in the next run', async () => {
		const device = setup({ files: true });
		const body = bytes(3 * MiB, 3);
		device.writeFile(PHOTO, body);
		device.server.cutPutAfter = MiB + MiB / 2;
		device.engine.start(true);
		await settle();
		const saved = device.state.pairing?.upload;
		expect(saved).toBeDefined();

		// The app reloads: a new engine over the same pairing and local files.
		const states: { transfer: Transfer | null; status: string }[] = [];
		const next = new SyncEngine({ ...device.engineOptions, onState: (s) => states.push(s) });
		next.start(true);
		await settle();

		expect(states.at(-1)).toMatchObject({
			status: 'interrupted',
			transfer: { direction: 'up', done: saved?.done, total: 3 * MiB, interrupted: true },
		});

		await next.resumeTransfer();

		const resumed = device.server.puts.at(-1);
		expect(resumed?.offset).toBe(MiB + MiB / 2);
		expect(same(device.server.document(PHOTO), body)).toBe(true);
		expect(states.at(-1)).toMatchObject({ status: 'connected', transfer: null });
	});

	it('a body that changed since it was cut off goes again from the start', async () => {
		const device = setup({ files: true });
		device.writeFile(PHOTO, bytes(3 * MiB, 1));
		device.server.cutPutAfter = MiB;
		device.engine.start(true);
		await settle();
		await device.fire(2000);

		device.writeFile(PHOTO, bytes(3 * MiB, 2));
		await device.engine.resumeTransfer();

		expect(device.server.puts.map((put) => put.offset)).toEqual([0, 0]);
		expect(same(device.server.document(PHOTO), bytes(3 * MiB, 2))).toBe(true);
	});

	it('hiding the app during a large upload cuts it off', async () => {
		const device = setup({ files: true });
		device.writeFile(PHOTO, bytes(2 * MiB));
		const original = FakeClient.prototype.put;
		const put = vi.spyOn(FakeClient.prototype, 'put').mockImplementationOnce(async function (
			this: FakeClient,
			...args
		) {
			device.engine.setVisible(false);
			return original.apply(this, args);
		});

		device.engine.start(true);
		await settle();

		expect(device.last().transfer).toMatchObject({ direction: 'up', interrupted: true });
		device.engine.setVisible(true);
		await settle();
		expect(device.last().status).toBe('interrupted');
		put.mockRestore();
	});

	it('a large upload whose commit was cut off resumes by itself: nothing is left to send', async () => {
		const device = setup({ files: true });
		device.writeFile(PHOTO, bytes(2 * MiB));
		const original = FakeClient.prototype.commit;
		const commit = vi.spyOn(FakeClient.prototype, 'commit').mockImplementationOnce(async function (
			this: FakeClient,
		) {
			this.drop();
			throw new Error('never sent');
		});

		device.engine.start(true);
		await settle();
		expect(device.last()).toMatchObject({ status: 'unreachable', transfer: null });
		commit.mockImplementation(original);

		await device.fire(2000);

		expect(device.server.puts.map((put) => put.offset)).toEqual([0, 2 * MiB]);
		expect(device.last()).toMatchObject({ status: 'connected', transfer: null });
		commit.mockRestore();
	});

	it('a small read cut off reads again by itself', async () => {
		const device = setup({ files: true });
		device.server.storeDocument(NOTE, bytes(5000));
		device.server.cutGetAfter = 2000;

		device.engine.start(true);
		await settle();
		expect(device.last()).toMatchObject({ status: 'unreachable', transfer: null });
		const get = vi.spyOn(FakeClient.prototype, 'get');

		await device.fire(2000);

		expect(get.mock.calls.map(([, options]) => options?.offset ?? 0)).toEqual([0]);
		expect(same(device.local.files.get('notes/n1.md')?.body ?? null, bytes(5000))).toBe(true);
		get.mockRestore();
	});

	it('a large read cut off waits, then resumes from the offset while the etag holds', async () => {
		const device = setup({ files: true });
		const body = bytes(3 * MiB);
		device.server.storeDocument(NOTE, 'small');
		device.server.storeDocument(PHOTO, body);
		device.server.cutGetAfter = MiB;

		device.engine.start(true);
		await settle();
		expect(device.last().transfer).toMatchObject({
			direction: 'down',
			total: 3 * MiB + 5,
			interrupted: true,
		});
		await device.fire(2000);
		expect(device.last().status).toBe('interrupted');
		const get = vi.spyOn(FakeClient.prototype, 'get');

		await device.engine.resumeTransfer();

		// The note came whole before the cut: only the photo is read, from where it stopped.
		expect(get.mock.calls.map(([key, options]) => [key.id, options?.offset])).toEqual([
			['p1', MiB - 5],
		]);
		expect(same(device.local.files.get('photos/p1.jpg')?.body ?? null, body)).toBe(true);
		expect(device.last()).toMatchObject({ status: 'connected', transfer: null });
		get.mockRestore();
	});

	it('a large read whose document changed meanwhile reads it afresh', async () => {
		const device = setup({ files: true });
		device.server.storeDocument(PHOTO, bytes(3 * MiB, 1));
		device.server.cutGetAfter = MiB;
		device.engine.start(true);
		await settle();
		await device.fire(2000);

		device.server.storeDocument(PHOTO, bytes(3 * MiB, 2));
		const get = vi.spyOn(FakeClient.prototype, 'get');
		await device.engine.resumeTransfer();

		expect(get.mock.calls.map(([, options]) => options?.offset)).toEqual([0]);
		expect(same(device.local.files.get('photos/p1.jpg')?.body ?? null, bytes(3 * MiB, 2))).toBe(
			true,
		);
		get.mockRestore();
	});

	it('a large read cut off can be started over', async () => {
		const device = setup({ files: true });
		device.server.storeDocument(PHOTO, bytes(3 * MiB));
		device.server.cutGetAfter = MiB;
		device.engine.start(true);
		await settle();
		await device.fire(2000);
		const get = vi.spyOn(FakeClient.prototype, 'get');

		await device.engine.restartTransfer();

		expect(get.mock.calls.map(([, options]) => options?.offset)).toEqual([0]);
		expect(device.last()).toMatchObject({ status: 'connected', transfer: null });
		get.mockRestore();
	});

	it('resume and restart do nothing when no transfer waits', async () => {
		const device = setup({ files: true });
		device.engine.start(true);
		await settle();
		device.server.log.length = 0;

		await device.engine.resumeTransfer();
		await device.engine.restartTransfer();

		expect(device.server.log).toEqual([]);
	});

	it('the threshold: a transfer of exactly SMALL_TRANSFER_BYTES starts over by itself', async () => {
		const device = setup({ files: true });
		device.writeFile(PHOTO, bytes(SMALL_TRANSFER_BYTES));
		device.server.cutPutAfter = 10;

		device.engine.start(true);
		await settle();

		expect(device.last().transfer).toBeNull();
		expect(device.state.pairing).not.toHaveProperty('upload');
	});

	it('a large upload with less than SMALL_TRANSFER_BYTES left goes on by itself when cut off', async () => {
		const device = setup({ files: true });
		const body = bytes(3 * MiB);
		device.writeFile(PHOTO, body);
		device.server.cutPutAfter = 2 * MiB + MiB / 2;

		device.engine.start(true);
		await settle();
		expect(device.last()).toMatchObject({ status: 'unreachable', transfer: null });

		await device.fire(2000);

		expect(device.server.puts.map((put) => put.offset)).toEqual([0, 2 * MiB + MiB / 2]);
		expect(same(device.server.document(PHOTO), body)).toBe(true);
		expect(device.last()).toMatchObject({ status: 'connected', transfer: null });
	});
});

/*
 * The record of a transaction's bodies never names bytes the server does
 * not hold: a put that never reached the server must not let a resume
 * confirm or finish another body's bytes under the new body's etag.
 */
describe('SyncEngine resuming only what the server holds', () => {
	const P2 = { path: 'photos', id: 'p2', type: 'jpg' };

	it('a body edited after it was staged, whose new put never arrived, is sent whole', async () => {
		const device = setup({ files: true });
		device.writeFile(PHOTO, bytes(2 * MiB, 1));
		device.writeFile(P2, bytes(2 * MiB, 2));
		// Both bodies staged whole; the connection drops at the commit.
		const original = FakeClient.prototype.commit;
		const commit = vi.spyOn(FakeClient.prototype, 'commit').mockImplementationOnce(async function (
			this: FakeClient,
		) {
			this.drop();
			throw new Error('never sent');
		});
		device.engine.start(true);
		await settle();
		commit.mockImplementation(original);

		// Both edited, same sizes; the next attempt is cut 1 MiB into the first,
		// so the second's put never goes.
		device.writeFile(PHOTO, bytes(2 * MiB, 11));
		device.writeFile(P2, bytes(2 * MiB, 12));
		device.server.cutPutAfter = MiB;
		await device.fire(2000);
		expect(device.last().transfer).toMatchObject({ interrupted: true });

		await device.engine.resumeTransfer();

		expect(same(device.server.document(PHOTO), bytes(2 * MiB, 11))).toBe(true);
		expect(same(device.server.document(P2), bytes(2 * MiB, 12))).toBe(true);
		expect(device.local.known.get('photos/p2.jpg')).toBe(device.server.etag(P2));
		commit.mockRestore();
	});

	it('a resumed offset never lands on the bytes of an older body: no mixed body', async () => {
		const device = setup({ files: true });
		const A = bytes(3 * MiB, 1);
		const B = bytes(3 * MiB, 2);
		device.writeFile(PHOTO, A);
		device.server.cutPutAfter = MiB;
		device.engine.start(true);
		await settle();
		await device.fire(2000);
		expect(device.last().status).toBe('interrupted');

		// Edited meanwhile, the same size; the put of B never reaches the server.
		device.writeFile(PHOTO, B);
		device.server.losePuts = 1;
		await device.engine.resumeTransfer();
		await device.fire(2000);
		expect(device.last().status).toBe('interrupted');
		await device.engine.resumeTransfer();

		expect(same(device.server.document(PHOTO), B)).toBe(true);
		const last = device.server.puts.at(-1);
		expect(last?.offset).toBe(0);
	});

	it('a transaction of another server is not resumed there, whatever that one holds under its id', async () => {
		const main = new FakeServer();
		main.role = 'main';
		const laptop = new FakeServer();
		Object.assign(laptop, { serverId: 'S2', name: 'Laptop', port: 47802, role: 'subordinate' });
		laptop.fingerprint = Array.from({ length: 32 }, () => 'CD').join(':');
		laptop.devices = main.devices;
		const device = setup({
			files: true,
			server: main,
			pairing: makePairing({ ...main.entry(), fallbacks: [laptop.entry()] }),
		});
		const engine = new SyncEngine({
			...device.engineOptions,
			connect: FakeServer.route(main, laptop),
		});
		const body = bytes(3 * MiB, 4);
		device.writeFile(PHOTO, body);
		main.cutPutAfter = MiB;
		engine.start(true);
		await settle();
		const tx = device.state.pairing?.upload?.tx ?? '';
		expect(device.state.pairing?.upload?.server).toBe('S1');
		// The laptop happens to hold other bytes of that size under that id.
		laptop.transactions.set(tx, {
			device: 'dev1',
			changes: new Map([
				[
					'photos/p1.jpg',
					{ key: PHOTO, ifEtag: null, delete: false, bytes: 3 * MiB, data: bytes(3 * MiB, 9) },
				],
			]),
		});

		main.connectError = 'connectTimeout';
		await device.fire(2000);
		await engine.resumeTransfer();

		expect(same(laptop.document(PHOTO), body)).toBe(true);
		expect(laptop.puts.map((put) => put.offset)).toEqual([0]);
	});

	it('a large upload whose commit landed and whose answer was lost asks nothing after a reload', async () => {
		const device = setup({ files: true });
		device.writeFile(PHOTO, bytes(3 * MiB, 1));
		const original = FakeClient.prototype.commit;
		const commit = vi.spyOn(FakeClient.prototype, 'commit').mockImplementationOnce(async function (
			this: FakeClient,
			tx: string,
		) {
			await original.call(this, tx);
			this.drop();
			throw new Error('lost');
		});
		device.engine.start(true);
		await settle();
		commit.mockRestore();
		expect(device.state.pairing?.upload).toMatchObject({ done: 3 * MiB, total: 3 * MiB });

		const states: SyncState[] = [];
		const next = new SyncEngine({ ...device.engineOptions, onState: (s) => states.push(s) });
		next.start(true);
		await settle();

		expect(states.at(-1)).toMatchObject({ status: 'connected', transfer: null });
		expect(device.server.puts).toHaveLength(1);
		expect(device.state.pairing).not.toHaveProperty('upload');
		// The adapter hears that the body it planned is there.
		expect(device.local.dirty.size).toBe(0);
	});

	it('a large upload that ran out of conflict retries asks nothing after a reload', async () => {
		const device = setup({ files: true });
		device.writeFile(PHOTO, bytes(3 * MiB, 1));
		device.server.conflicts = 4;
		device.engine.start(true);
		await settle();
		expect(device.last()).toMatchObject({ status: 'error', error: { code: 'conflict' } });

		const states: SyncState[] = [];
		const next = new SyncEngine({ ...device.engineOptions, onState: (s) => states.push(s) });
		next.start(true);
		await settle();

		expect(states.at(-1)).toMatchObject({ status: 'connected', transfer: null });
		expect(device.server.puts.at(-1)).toMatchObject({ offset: 3 * MiB });
	});

	it('an upload whose record could not be saved is never resumed from an older record', async () => {
		const device = setup({ files: true });
		const store = device.engineOptions.pairingStore;
		let full = false;
		const options = {
			...device.engineOptions,
			pairingStore: {
				...store,
				// Storage full: nothing is saved, the older record stays.
				save: (pairing: Pairing) => {
					if (full) return false;
					store.save(pairing);
					return true;
				},
			},
		};
		const A = bytes(3 * MiB, 1);
		const B = bytes(3 * MiB, 2);
		device.writeFile(PHOTO, A);
		device.server.cutPutAfter = MiB;
		const engine = new SyncEngine(options);
		engine.start(true);
		await settle();
		await device.fire(2000);
		const saved = device.state.pairing?.upload;
		expect(saved?.bodies[0].etag).toBeDefined();

		// Edited to B, the same size; its record cannot be saved, and it is cut off too.
		device.writeFile(PHOTO, B);
		full = true;
		device.server.cutPutAfter = MiB;
		await engine.resumeTransfer();
		// The record still names A under the first transaction.
		expect(device.state.pairing?.upload).toEqual(saved);

		// The app reloads with A again: the record's transaction holds nothing of B.
		full = false;
		device.writeFile(PHOTO, A);
		const next = new SyncEngine(options);
		next.start(true);
		await settle();
		await next.resumeTransfer();

		expect(same(device.server.document(PHOTO), A)).toBe(true);
		expect(device.server.puts.at(-1)?.offset).toBe(0);
	});
});
