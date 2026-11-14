import { describe, expect, it } from 'vitest';
import { createPairingStore, type Pairing } from '../src/PairingStore.js';
import { MemoryStorage, makePairing } from './fixtures.js';

const KEY = 'todo.server';

function setup() {
	const storage = new MemoryStorage();
	const store = createPairingStore({ key: KEY, storage: () => storage });

	return { storage, store };
}

describe('createPairingStore', () => {
	it('save then load round-trips', () => {
		const { storage, store } = setup();
		const pairing = makePairing({ lastRevision: 'r1', lastSyncAt: '2026-11-07T10:00:00.000Z' });

		store.save(pairing);

		expect(store.load()).toEqual(pairing);
		expect(storage.getItem(KEY)).not.toBeNull();
	});

	it('keeps the revision of the last sync, null included, and refuses a record without one', () => {
		const { storage, store } = setup();

		for (const lastRevision of ['r1', null]) {
			const pairing = makePairing({ lastRevision });
			store.save(pairing);
			expect(store.load()).toEqual(pairing);
		}

		// A record of the single document (rc.1 and before) has none: the device pairs again.
		const { lastRevision, ...old } = makePairing();
		void lastRevision;
		storage.setItem(KEY, JSON.stringify({ ...old, lastEtag: 'e9' }));
		expect(store.load()).toBeNull();
		storage.setItem(KEY, JSON.stringify({ ...makePairing(), lastRevision: 7 }));
		expect(store.load()).toBeNull();
	});

	it('keeps an upload to resume, and drops one it cannot read, never the pairing', () => {
		const { storage, store } = setup();
		const upload = {
			tx: 'tx-0123456789abcdef',
			total: 3_000_000,
			done: 1_000_000,
			bodies: [{ path: 'photos', id: 'p1', type: 'jpg', bytes: 3_000_000, etag: 'e'.repeat(64) }],
		};
		const pairing = makePairing({ upload });

		store.save(pairing);
		expect(store.load()).toEqual(pairing);

		for (const broken of [
			{ ...upload, tx: 7 },
			{ ...upload, done: -1 },
			{ ...upload, bodies: [{ ...upload.bodies[0], etag: null }] },
			'nope',
		]) {
			storage.setItem(KEY, JSON.stringify({ ...makePairing(), upload: broken }));
			expect(store.load()).toEqual(makePairing());
		}
	});

	it('keeps fallback servers, and reads a record without them as having none', () => {
		const { store } = setup();
		const laptop = {
			serverId: 'Lp7tOp0123ab',
			name: 'Laptop',
			password: 'Zx9yW8vU7tS6rQ5pO4nM3lK2',
			fingerprint: Array.from({ length: 32 }, () => 'C3').join(':'),
			port: 47801,
			hosts: ['5c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f.local', '192.168.1.30'],
		};
		const pairing = makePairing({ fallbacks: [laptop] });

		store.save(pairing);
		expect(store.load()).toEqual(pairing);

		store.save(makePairing());
		expect(store.load()).not.toHaveProperty('fallbacks');
	});

	it('drops fallbacks it cannot read, never the pairing', () => {
		const { storage, store } = setup();
		const laptop = {
			serverId: 'Lp7tOp0123ab',
			name: 'Laptop',
			password: 'Zx9yW8vU7tS6rQ5pO4nM3lK2',
			fingerprint: Array.from({ length: 32 }, () => 'C3').join(':'),
			port: 47801,
			hosts: ['192.168.1.30'],
		};
		const broken = { ...laptop, serverId: 'Tb1tOp0123ab', port: 0 };
		storage.setItem(KEY, JSON.stringify({ ...makePairing(), fallbacks: [broken, laptop] }));
		expect(store.load()?.fallbacks).toEqual([laptop]);

		storage.setItem(KEY, JSON.stringify({ ...makePairing(), fallbacks: 'nope' }));
		expect(store.load()).toEqual(makePairing());
	});

	it('keeps the term, and reads a record without one, or with one it cannot read, as term 0', () => {
		const { storage, store } = setup();
		store.save(makePairing({ term: 2 }));
		expect(store.load()?.term).toBe(2);

		for (const term of [undefined, 0, -1, 1.5, '2']) {
			storage.setItem(KEY, JSON.stringify({ ...makePairing(), term }));
			expect(store.load()).toEqual(makePairing());
		}
	});

	it('load returns null for a missing record', () => {
		expect(setup().store.load()).toBeNull();
	});

	it('load returns null for corrupt JSON', () => {
		const { storage, store } = setup();
		storage.setItem(KEY, '{nope');

		expect(store.load()).toBeNull();
	});

	it('load returns null for a record missing its token', () => {
		const { storage, store } = setup();
		const pairing: Partial<Pairing> = makePairing();
		delete pairing.token;
		storage.setItem(KEY, JSON.stringify(pairing));

		expect(store.load()).toBeNull();
	});

	it('load returns null for a record with no hosts', () => {
		const { storage, store } = setup();
		storage.setItem(KEY, JSON.stringify(makePairing({ hosts: [] })));

		expect(store.load()).toBeNull();
	});

	it('clear removes it', () => {
		const { storage, store } = setup();
		store.save(makePairing());

		store.clear();

		expect(store.load()).toBeNull();
		expect(storage.getItem(KEY)).toBeNull();
	});

	it('blocked storage reads as unpaired and breaks nothing', () => {
		const store = createPairingStore({
			key: KEY,
			storage: () => {
				throw new DOMException('The operation is insecure.', 'SecurityError');
			},
		});

		expect(store.load()).toBeNull();
		expect(() => store.save(makePairing())).not.toThrow();
		expect(() => store.clear()).not.toThrow();
	});
});
