import type { PairingCode } from '../src/PairingCode.js';
import type { Pairing, PairingStorage } from '../src/PairingStore.js';

export function makePairing(overrides: Partial<Pairing> = {}): Pairing {
	return {
		serverId: 'S1',
		name: 'Desk',
		device: 'dev1',
		token: 'tok1',
		password: 'a'.repeat(24),
		fingerprint: 'AA:BB',
		port: 47800,
		hosts: ['a.local', '192.168.1.5'],
		lastEtag: null,
		lastSyncAt: null,
		...overrides,
	};
}

export const CODE: PairingCode = {
	serverId: 'S1',
	name: 'Desk',
	secret: 'sec',
	password: 'b'.repeat(24),
	fingerprint: 'CC:DD',
	port: 47801,
	hosts: ['b.local'],
};

/** Web Storage in memory. */
export class MemoryStorage implements PairingStorage {
	readonly items = new Map<string, string>();

	getItem(key: string): string | null {
		return this.items.get(key) ?? null;
	}

	setItem(key: string, value: string): void {
		this.items.set(key, value);
	}

	removeItem(key: string): void {
		this.items.delete(key);
	}
}

/** Lets every queued promise callback run. */
export function settle(): Promise<void> {
	return new Promise((resolve) => setTimeout(resolve, 0));
}
