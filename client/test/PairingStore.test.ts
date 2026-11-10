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
		const pairing = makePairing({ lastEtag: 'e1', lastSyncAt: '2026-11-07T10:00:00.000Z' });

		store.save(pairing);

		expect(store.load()).toEqual(pairing);
		expect(storage.getItem(KEY)).not.toBeNull();
	});

	it('reads a record saved before the package existed', () => {
		// The todo app saved exactly this shape under this key; devices paired
		// then must not have to pair again.
		const { storage, store } = setup();
		const saved = {
			serverId: 'Ab3dEf6hIj9k',
			name: 'Desk',
			device: 'Dev0123456789ab',
			token: 'tok',
			password: 'abcdefghijklmnopqrstuvwx',
			fingerprint: '0A:1B',
			port: 47800,
			hosts: ['0b7c5a52-6a3e-4d2b-9f1e-7d2c3b4a5f60.local', '192.168.1.20'],
			lastEtag: 'e9',
			lastSyncAt: '2026-11-09T18:00:00.000Z',
		};
		storage.setItem(KEY, JSON.stringify(saved));

		expect(store.load()).toEqual(saved);
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
