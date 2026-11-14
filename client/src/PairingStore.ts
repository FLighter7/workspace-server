import { readFallbackServer, type FallbackServer, type ServerTarget } from './PairingCode.js';

/**
 * A body of an unfinished upload, as the device staged it: its document, its
 * size, and its etag (the SHA-256 of its bytes), which tells whether the body
 * a later sync wants to write is the same one, and so may resume.
 */
export interface StagedBody {
	path: string;
	id: string;
	type: string;
	bytes: number;
	etag: string;
}

/**
 * A large upload that did not finish: its transaction, open on the server
 * for 24 hours after its last activity, and the bodies it staged. A `begin`
 * with the same `tx` returns to it, and each body the next sync still wants
 * resumes from what the server reports it received.
 */
export interface PendingUpload {
	tx: string;
	/** The server the transaction is open on; on another, its bodies go from the start. */
	server: string;
	/** The bytes of every body the upload puts. */
	total: number;
	/**
	 * How many of them had gone when this was saved: at the start, when cut
	 * off, and once every body arrived. Only `begin` says what the server
	 * holds; this decides whether to ask before resuming.
	 */
	done: number;
	bodies: StagedBody[];
}

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
	/**
	 * The revision of the app's set at the last successful sync, `null` for an
	 * empty set or before the first: a sync that finds the set still at it,
	 * with no local change since, reads nothing. Every server of a family
	 * holding the same files has the same revision.
	 */
	lastRevision: string | null;
	/** ISO 8601 UTC of the last successful sync. */
	lastSyncAt: string | null;
	/**
	 * The paired server's subordinates, as its last `welcome` listed them:
	 * the engine syncs with one of them when the paired server is out of
	 * reach. Absent when there are none.
	 */
	fallbacks?: FallbackServer[];
	/**
	 * The family's term as the paired server last named it as the main (see
	 * `Welcome.term`). The device follows a fallback that answers as the
	 * main at this term or a higher one: it becomes the paired server.
	 * Absent at 0.
	 */
	term?: number;
	/** A large upload cut off before its commit, to resume; absent when there is none. */
	upload?: PendingUpload;
}

/** Where the sync engine keeps this device's pairing. */
export interface PairingStore {
	/** `null` for a missing, corrupt or wrongly shaped record alike. */
	load(): Pairing | null;
	/**
	 * Saves the record. Returns `false` when it could not (storage full or
	 * blocked); a store that returns nothing is taken to have saved it.
	 */
	save(pairing: Pairing): boolean | void;
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

function isCount(value: unknown): value is number {
	return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
}

function readStagedBody(value: unknown): StagedBody | null {
	if (typeof value !== 'object' || value === null) return null;
	const { path, id, type, bytes, etag } = value as Record<string, unknown>;
	if (!isString(path) || !isString(id) || !isString(type) || !isCount(bytes) || !isString(etag)) {
		return null;
	}
	return { path, id, type, bytes, etag };
}

/** A pending upload as stored; `null` when any part of it does not read. */
function readUpload(value: unknown): PendingUpload | null {
	if (typeof value !== 'object' || value === null) return null;
	const { tx, server, total, done, bodies } = value as Record<string, unknown>;
	if (
		!isString(tx) ||
		!isString(server) ||
		!isCount(total) ||
		!isCount(done) ||
		!Array.isArray(bodies)
	) {
		return null;
	}
	const read = bodies.map(readStagedBody);
	if (read.some((body) => body === null)) return null;

	return { tx, server, total, done, bodies: read as StagedBody[] };
}

function readPairing(value: unknown): Pairing | null {
	if (typeof value !== 'object' || value === null || Array.isArray(value)) return null;
	const raw = value as Record<string, unknown>;
	const { serverId, name, device, token, password, fingerprint, port, hosts } = raw;
	const { lastRevision, lastSyncAt } = raw;

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
		isNullableString(lastRevision) &&
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
		lastRevision,
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
	// One that does not read costs the resume: the next sync uploads afresh.
	const upload = readUpload(raw.upload);
	if (upload) pairing.upload = upload;

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
				return true;
			} catch (error) {
				// Storage is full or blocked. Sync still runs for this visit; the
				// next start finds the record as it was last saved, or none.
				console.warn('Server sync could not save its pairing record', error);
				return false;
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
