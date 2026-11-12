import { readFallbackServer, type FallbackServer, type ServerTarget } from './PairingCode.js';

/**
 * What a device keeps after pairing: how to reach the server it paired
 * with, and who it is there. Its fallback servers take the same device and
 * token.
 */
export interface Pairing extends ServerTarget {
	serverId: string;
	name: string;
	/** This device's id on the server. */
	device: string;
	/** This device's secret token; the server keeps only its hash. */
	token: string;
	/** The etag of the server's document at the last successful sync. */
	lastEtag: string | null;
	/** ISO 8601 UTC of the last successful sync. */
	lastSyncAt: string | null;
	/**
	 * The paired server's subordinates, as its last `welcome` listed them:
	 * the engine syncs with one of them when the paired server is out of
	 * reach. Absent from a record saved before there were any, which reads
	 * as none.
	 */
	fallbacks?: FallbackServer[];
	/**
	 * The family's term as the paired server last named it as the main (see
	 * `Welcome.term`). The device follows a fallback that answers as the
	 * main at this term or a higher one: it becomes the paired server.
	 * Absent from a record saved before, which reads as 0.
	 */
	term?: number;
}

/** Where the sync engine keeps this device's pairing. */
export interface PairingStore {
	/** `null` for a missing, corrupt or wrongly shaped record alike. */
	load(): Pairing | null;
	save(pairing: Pairing): void;
	clear(): void;
}

/** The part of the Web Storage API a pairing store uses. */
export type PairingStorage = Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>;

export interface PairingStoreOptions {
	/**
	 * The storage key. Device-local: a pairing holds this device's token, so
	 * it belongs in no backup and is never synced.
	 */
	key: string;
	/**
	 * The storage, read on every access: reading `localStorage` itself can
	 * throw where storage is blocked. Defaults to `localStorage`.
	 */
	storage?: () => PairingStorage;
}

function isString(value: unknown): value is string {
	return typeof value === 'string';
}

function isNullableString(value: unknown): value is string | null {
	return value === null || isString(value);
}

function readPairing(value: unknown): Pairing | null {
	if (typeof value !== 'object' || value === null || Array.isArray(value)) return null;
	const raw = value as Record<string, unknown>;
	const { serverId, name, device, token, password, fingerprint, port, hosts } = raw;
	const { lastEtag, lastSyncAt } = raw;

	const valid =
		isString(serverId) &&
		isString(name) &&
		isString(device) &&
		isString(token) &&
		token.length > 0 &&
		isString(password) &&
		isString(fingerprint) &&
		typeof port === 'number' &&
		Number.isInteger(port) &&
		Array.isArray(hosts) &&
		hosts.length > 0 &&
		hosts.every(isString) &&
		isNullableString(lastEtag) &&
		isNullableString(lastSyncAt);
	if (!valid) return null;

	const pairing: Pairing = {
		serverId,
		name,
		device,
		token,
		password,
		fingerprint,
		port,
		hosts: [...(hosts as string[])],
		lastEtag,
		lastSyncAt,
	};
	// A list that does not read costs the fallbacks, never the pairing.
	const fallbacks = Array.isArray(raw.fallbacks)
		? raw.fallbacks
				.map(readFallbackServer)
				.filter((server): server is FallbackServer => server !== null)
		: [];
	if (fallbacks.length > 0) pairing.fallbacks = fallbacks;
	const { term } = raw;
	if (typeof term === 'number' && Number.isSafeInteger(term) && term > 0) pairing.term = term;

	return pairing;
}

/**
 * A pairing store over Web Storage, one JSON record under `key`. Every
 * access is guarded: a browser with storage blocked behaves as an unpaired
 * one rather than breaking the app.
 */
export function createPairingStore(options: PairingStoreOptions): PairingStore {
	const { key } = options;
	const storage = options.storage ?? (() => localStorage);

	return {
		load() {
			try {
				const raw = storage().getItem(key);
				return raw === null ? null : readPairing(JSON.parse(raw));
			} catch {
				return null;
			}
		},
		save(pairing) {
			try {
				storage().setItem(key, JSON.stringify(pairing));
			} catch {
				// Storage is full or blocked. Sync still runs for this visit; the
				// next start finds no pairing and asks to pair again.
			}
		},
		clear() {
			try {
				storage().removeItem(key);
			} catch {
				// Nothing to do: storage that cannot be written holds nothing either.
			}
		},
	};
}
