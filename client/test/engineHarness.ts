import { expect, vi } from 'vitest';
import type { Pairing, PairingStore } from '../src/PairingStore.js';
import type { DocumentKey, Listing } from '../src/ServerClient.js';
import {
	SyncEngine,
	documentPath,
	type SyncAdapter,
	type SyncEngineOptions,
	type SyncState,
} from '../src/SyncEngine.js';
import { DocumentError } from '../src/WorkspaceError.js';
import { FakeServer } from '../src/testing/FakeServer.js';
import { makePairing, settle } from './fixtures.js';

export const START = new Date('2026-11-07T12:00:00.000Z').getTime();

/** The one document of the notes app. */
export const DATA: DocumentKey = { path: '', id: 'data', type: 'json' };

/*
 * Two apps under test. The notes app keeps a set of notes in one document,
 * `data.json`: {"schema": 1, "notes": [...]}, merged as a union, so what
 * either side holds survives; schema 2 and up is a newer format it cannot
 * read. The files app keeps any number of documents, one per file: it reads
 * only those whose etag moved since it last saw them, the server's copy
 * wins, and it writes the files changed here and deletes those removed here.
 */
export type DocCode = 'newerSchema' | 'badDocument';

export function encodeNotes(notes: string[], schema = 1): Uint8Array {
	return new TextEncoder().encode(JSON.stringify({ schema, notes: [...notes].sort() }));
}

export function decodeNotes(body: Uint8Array | null): string[] {
	if (body === null || body.byteLength === 0) return [];
	let doc: unknown;
	try {
		doc = JSON.parse(new TextDecoder().decode(body));
	} catch {
		throw new DocumentError<DocCode>('Not JSON', 'badDocument');
	}
	const { schema, notes } = (doc ?? {}) as { schema?: unknown; notes?: unknown };
	if (typeof schema === 'number' && schema > 1) {
		throw new DocumentError<DocCode>('A newer document', 'newerSchema');
	}
	if (!Array.isArray(notes)) throw new DocumentError<DocCode>('No notes', 'badDocument');

	return notes as string[];
}

function same(a: string[], b: string[]): boolean {
	return a.length === b.length && [...a].sort().every((note, i) => note === [...b].sort()[i]);
}

/** What a device of either app holds. */
export interface Local {
	notes: string[];
	/** The files app's files, by canonical key. */
	files: Map<string, { key: DocumentKey; body: Uint8Array }>;
	/** The etag of each file as the server last had it, by canonical key. */
	known: Map<string, string>;
	/** Files changed here since, to write. */
	dirty: Set<string>;
	/** Files removed here since, to delete. */
	deleted: Map<string, DocumentKey>;
	changeCount: number;
}

export function notesAdapter(local: Local, replaceLocal: (notes: string[]) => Promise<void>) {
	const adapter: SyncAdapter = {
		changeCount: () => local.changeCount,
		select: () => [DATA],
		plan: (_listing, read) => {
			const remote = decodeNotes(read[0]?.body ?? null);
			const notes = local.notes;
			const merged = [...new Set([...remote, ...notes])].sort();

			return {
				apply: same(merged, notes) ? undefined : () => replaceLocal(merged),
				put: same(merged, remote) ? [] : [{ doc: DATA, body: encodeNotes(merged) }],
			};
		},
	};
	return adapter;
}

export function filesAdapter(local: Local) {
	const adapter: SyncAdapter = {
		changeCount: () => local.changeCount,
		select: (listing: Listing) =>
			listing.docs.filter((doc) => local.known.get(documentPath(doc)) !== doc.etag),
		plan: (listing, read) => {
			const listed = new Set(listing.docs.map(documentPath));
			const files = new Map(local.files);
			const known = new Map(local.known);
			const dirty = new Set(local.dirty);
			for (const doc of read) {
				const path = documentPath(doc);
				files.set(path, { key: { path: doc.path, id: doc.id, type: doc.type }, body: doc.body });
				known.set(path, doc.etag);
				dirty.delete(path);
			}
			for (const path of local.known.keys()) {
				if (listed.has(path) || dirty.has(path)) continue;
				files.delete(path);
				known.delete(path);
			}

			return {
				apply: async () => {
					local.files = files;
					local.known = known;
					local.dirty = dirty;
				},
				put: [...dirty].flatMap((path) => {
					const file = files.get(path);
					return file ? [{ doc: file.key, body: file.body }] : [];
				}),
				delete: [...local.deleted.values()],
			};
		},
		committed: (docs) => {
			for (const doc of docs) {
				const path = documentPath(doc);
				if (doc.etag === null) {
					local.known.delete(path);
					local.deleted.delete(path);
				} else {
					local.known.set(path, doc.etag);
					local.dirty.delete(path);
				}
			}
		},
	};
	return adapter;
}

export interface Timer {
	handle: number;
	callback: () => void;
	ms: number;
	cleared: boolean;
}

export interface SetupOptions {
	pairing?: Pairing | null;
	notes?: string[];
	/** Use the files app instead of the notes app. */
	files?: boolean;
	server?: FakeServer;
}

export function setup(options: SetupOptions = {}) {
	const server = options.server ?? new FakeServer();
	const state = {
		pairing: options.pairing === undefined ? makePairing() : options.pairing,
		now: START,
	};
	const local: Local = {
		notes: options.notes ?? [],
		files: new Map(),
		known: new Map(),
		dirty: new Set(),
		deleted: new Map(),
		changeCount: 0,
	};
	const timers: Timer[] = [];
	const states: SyncState<DocCode>[] = [];
	const replaceLocal = vi.fn(async (notes: string[]) => {
		local.notes = notes;
	});
	const adapter = options.files ? filesAdapter(local) : notesAdapter(local, replaceLocal);
	const pairingStore: PairingStore = {
		load: () => (state.pairing ? structuredClone(state.pairing) : null),
		save: (pairing) => {
			state.pairing = structuredClone(pairing);
		},
		clear: () => {
			state.pairing = null;
		},
	};

	const engineOptions: SyncEngineOptions<DocCode> = {
		app: 'notes',
		pairingStore,
		adapter,
		onState: (next) => states.push(next),
		deviceName: () => 'iPhone Safari',
		connect: server.connect,
		now: () => new Date(state.now),
		timers: {
			set: (callback, ms) => {
				const timer = { handle: timers.length + 1, callback, ms, cleared: false };
				timers.push(timer);
				return timer.handle;
			},
			clear: (handle) => {
				const timer = timers.find((entry) => entry.handle === handle);
				if (timer) timer.cleared = true;
			},
		},
	};

	const engine = new SyncEngine(engineOptions);
	const pending = (): Timer[] => timers.filter((timer) => !timer.cleared);
	const last = (): SyncState<DocCode> => states[states.length - 1];
	const written = (): { ifEtag: string | null; notes: string[] }[] =>
		server.puts.map((put) => ({ ifEtag: put.ifEtag, notes: decodeNotes(put.body) }));

	/** Fires the one pending timer (or the one of `ms`), and lets what it starts run. */
	async function fire(ms?: number): Promise<void> {
		const timer = pending().find((t) => ms === undefined || t.ms === ms);
		expect(timer).toBeDefined();
		if (!timer) return;
		timer.cleared = true;
		timer.callback();
		await settle();
	}

	/** A local edit of the notes, as the app reports it. */
	function edit(notes: string[]): void {
		local.notes = notes;
		local.changeCount++;
		engine.localChanged();
	}

	/** A file written here, as the files app reports it. */
	function writeFile(key: DocumentKey, body: Uint8Array | string): void {
		const path = documentPath(key);
		const bytes = typeof body === 'string' ? new TextEncoder().encode(body) : body;
		local.files.set(path, { key, body: bytes });
		local.dirty.add(path);
		local.deleted.delete(path);
		local.changeCount++;
		engine.localChanged();
	}

	/** A file removed here, as the files app reports it. */
	function removeFile(key: DocumentKey): void {
		const path = documentPath(key);
		local.files.delete(path);
		local.dirty.delete(path);
		local.deleted.set(path, key);
		local.changeCount++;
		engine.localChanged();
	}

	/** The files app's files as text, by canonical key. */
	function filesText(): Record<string, string> {
		return Object.fromEntries(
			[...local.files].map(([path, file]) => [path, new TextDecoder().decode(file.body)]),
		);
	}

	return {
		server,
		state,
		local,
		states,
		engine,
		engineOptions,
		adapter,
		replaceLocal,
		pending,
		last,
		written,
		fire,
		edit,
		writeFile,
		removeFile,
		filesText,
	};
}

export { settle };
