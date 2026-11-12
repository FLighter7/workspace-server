import { describe, expect, it, vi } from 'vitest';
import type { Pairing, PairingStore } from '../src/PairingStore.js';
import {
	BLOCKED_RETRY_MS,
	CHANGE_DEBOUNCE_MS,
	SyncEngine,
	type SyncAdapter,
	type SyncEngineOptions,
	type SyncState,
} from '../src/SyncEngine.js';
import { DocumentError, WorkspaceError } from '../src/WorkspaceError.js';
import { FakeServer } from '../src/testing/FakeServer.js';
import { CODE, makePairing, settle } from './fixtures.js';

const START = new Date('2026-11-07T12:00:00.000Z').getTime();

/*
 * The app under test keeps a set of notes. Its document is
 * {"schema": 1, "notes": [...]}, and merging is a union: what either side
 * holds survives. Schema 2 and up is a newer format it cannot read.
 */
type DocCode = 'newerSchema' | 'badDocument';

function encodeNotes(notes: string[], schema = 1): Uint8Array {
	return new TextEncoder().encode(JSON.stringify({ schema, notes: [...notes].sort() }));
}

function decodeNotes(body: Uint8Array): string[] {
	if (body.byteLength === 0) return [];
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

interface Timer {
	handle: number;
	callback: () => void;
	ms: number;
	cleared: boolean;
}

function setup(options: { pairing?: Pairing | null; local?: string[] } = {}) {
	const server = new FakeServer();
	const state = {
		pairing: options.pairing === undefined ? makePairing() : options.pairing,
		local: options.local ?? [],
		changeCount: 0,
		now: START,
	};
	const timers: Timer[] = [];
	const states: SyncState<DocCode>[] = [];
	const replaceLocal = vi.fn(async (notes: string[]) => {
		state.local = notes;
	});

	const adapter: SyncAdapter = {
		changeCount: () => state.changeCount,
		plan: (doc) => {
			const remote = decodeNotes(doc.body);
			const local = state.local;
			const merged = [...new Set([...remote, ...local])].sort();

			return {
				apply: same(merged, local) ? undefined : () => replaceLocal(merged),
				write: same(merged, remote) ? null : encodeNotes(merged),
			};
		},
	};
	const pairingStore: PairingStore = {
		load: () => (state.pairing ? { ...state.pairing } : null),
		save: (pairing) => {
			state.pairing = { ...pairing };
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

	/** Fires the one pending timer, and lets what it starts run. */
	async function fire(): Promise<void> {
		const [timer] = pending();
		expect(timer).toBeDefined();
		timer.cleared = true;
		timer.callback();
		await settle();
	}

	/** A local edit, as the app reports it. */
	function edit(notes: string[]): void {
		state.local = notes;
		state.changeCount++;
		engine.localChanged();
	}

	return {
		server,
		state,
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
	};
}

describe('SyncEngine', () => {
	it('no pairing means unpaired', async () => {
		const { engine, last, server } = setup({ pairing: null });

		engine.start(true);
		await settle();

		expect(last().status).toBe('unpaired');
		expect(server.log).toEqual([]);
	});

	it('first sync writes the local state to an empty server', async () => {
		const { engine, last, server, state, written } = setup({ local: ['a'] });

		engine.start(true);
		await settle();

		expect(server.log).toEqual(['connect', 'hello#1', 'head#1', 'get#1', 'put#1']);
		expect(written()).toEqual([{ ifEtag: null, notes: ['a'] }]);
		expect(state.pairing?.lastEtag).toBe(server.etag);
		expect(state.pairing?.lastSyncAt).toBe('2026-11-07T12:00:00.000Z');
		expect(last()).toMatchObject({
			status: 'connected',
			serverName: 'Desk',
			lastSyncAt: '2026-11-07T12:00:00.000Z',
			error: null,
		});
	});

	it('refreshes the hosts and name from the welcome', async () => {
		const { engine, server, state } = setup();
		server.hosts = ['a.local', '10.0.0.7'];
		server.name = 'Laptop';

		engine.start(true);
		await settle();

		expect(state.pairing?.hosts).toEqual(['a.local', '10.0.0.7']);
		expect(state.pairing?.name).toBe('Laptop');
	});

	it('what only the server holds is applied without a write', async () => {
		const { engine, server, state, replaceLocal } = setup({ local: ['a'] });
		server.store(encodeNotes(['a', 'b']));

		engine.start(true);
		await settle();

		expect(replaceLocal).toHaveBeenCalledWith(['a', 'b']);
		expect(state.local).toEqual(['a', 'b']);
		expect(server.puts).toEqual([]);
		expect(state.pairing?.lastEtag).toBe(server.etag);
	});

	it('nothing changed means no get', async () => {
		const { engine, server } = setup({ local: ['a'] });
		engine.start(true);
		await settle();
		server.log.length = 0;

		await engine.syncNow();

		expect(server.log).toEqual(['head#1']);
	});

	it('a changed push syncs', async () => {
		const { engine, server, state } = setup({ local: ['a'] });
		engine.start(true);
		await settle();
		server.store(encodeNotes(['a', 'b']));
		server.log.length = 0;

		server.clients[0].emitChanged(server.etag ?? '');
		await settle();

		expect(server.log).toEqual(['head#1', 'get#1']);
		expect(state.local).toContain('b');
	});

	it('local changes sync after the debounce', async () => {
		const { engine, server, pending, fire, edit } = setup({ local: ['a'] });
		engine.start(true);
		await settle();
		server.log.length = 0;

		edit(['a', 'one']);
		edit(['a', 'one', 'two']);

		expect(pending().map((timer) => timer.ms)).toEqual([CHANGE_DEBOUNCE_MS]);
		expect(server.log).toEqual([]);
		await fire();

		expect(server.log).toEqual(['head#1', 'get#1', 'put#1']);
		expect(decodeNotes(server.body)).toEqual(['a', 'one', 'two']);
	});

	it('conflicts are retried, then reported', async () => {
		const once = setup({ local: ['a'] });
		once.server.conflicts = 1;
		once.engine.start(true);
		await settle();
		expect(once.server.puts).toHaveLength(2);
		expect(once.last().status).toBe('connected');

		const always = setup({ local: ['a'] });
		always.server.conflicts = 4;
		always.engine.start(true);
		await settle();
		expect(always.server.puts).toHaveLength(4);
		expect(always.last()).toMatchObject({ status: 'error', error: { code: 'conflict' } });
	});

	it('a different server stops with serverChanged', async () => {
		const { engine, server, last, pending } = setup();
		server.serverId = 'OTHER';

		engine.start(true);
		await settle();

		expect(last()).toMatchObject({ status: 'error', error: { code: 'serverChanged' } });
		expect(server.log).toEqual(['connect', 'hello#1', 'close#1']);
		expect(pending()).toEqual([]);
	});

	it('badToken stops without retries', async () => {
		const { engine, server, last, pending } = setup({ pairing: makePairing({ token: 'wrong' }) });

		engine.start(true);
		await settle();
		engine.localChanged();
		engine.online();
		await engine.syncNow();

		expect(last()).toMatchObject({ status: 'error', error: { code: 'badToken' } });
		expect(pending()).toEqual([]);
		expect(server.log.filter((entry) => entry === 'connect')).toHaveLength(1);
	});

	it('blocked keeps the pairing and retries once a minute', async () => {
		const { engine, server, last, state, pending, fire } = setup();
		server.helloError = 'blocked';
		const before = { ...state.pairing };

		engine.start(true);
		await settle();

		expect(last()).toMatchObject({ status: 'paused', error: { code: 'blocked' } });
		expect(state.pairing).toEqual(before);
		expect(server.log).toEqual(['connect', 'hello#1', 'close#1']);
		expect(pending().map((timer) => timer.ms)).toEqual([BLOCKED_RETRY_MS]);

		await fire();
		expect(last()).toMatchObject({ status: 'paused', error: { code: 'blocked' } });
		expect(pending().map((timer) => timer.ms)).toEqual([BLOCKED_RETRY_MS]);

		server.helloError = null;
		await fire();

		expect(server.log.filter((entry) => entry === 'connect')).toHaveLength(3);
		expect(last()).toMatchObject({ status: 'connected', error: null });
		expect(pending()).toEqual([]);
	});

	it('Sync now does not wait out a block', async () => {
		const { engine, server, last } = setup();
		server.helloError = 'blocked';
		engine.start(true);
		await settle();

		server.helloError = null;
		await engine.syncNow();
		await settle();

		expect(server.log.filter((entry) => entry === 'connect')).toHaveLength(2);
		expect(last().status).toBe('connected');
	});

	it('a server-side failure retries with backoff and keeps the pairing', async () => {
		const { engine, server, last, state, pending, fire } = setup({ local: ['a'] });
		engine.start(true);
		await settle();
		const before = { ...state.pairing };

		vi.spyOn(server.clients[0], 'get').mockRejectedValueOnce(
			new WorkspaceError('disk full', 'internal'),
		);
		server.store(encodeNotes(['b']));
		await engine.syncNow();

		expect(last()).toMatchObject({ status: 'paused', error: { code: 'internal' } });
		expect(server.log).toContain('close#1');
		expect(state.pairing?.token).toBe(before.token);
		expect(pending().map((timer) => timer.ms)).toEqual([2000]);

		await fire();
		expect(last()).toMatchObject({ status: 'connected', error: null });
		expect(state.local).toEqual(['a', 'b']);
	});

	it('a refusal retrying cannot cure waits for the next trigger', async () => {
		const { engine, server, last, pending } = setup({ local: ['a'] });
		server.conflicts = 4;

		engine.start(true);
		await settle();

		expect(last()).toMatchObject({ status: 'error', error: { code: 'conflict' } });
		expect(pending()).toEqual([]);
		expect(server.log).not.toContain('close#1');
	});

	it('unreachable retries with backoff', async () => {
		const { engine, server, last, pending, fire } = setup();
		server.connectError = 'connectTimeout';

		engine.start(true);
		await settle();
		expect(last()).toMatchObject({ status: 'unreachable', error: { code: 'connectTimeout' } });

		const delays: number[] = [];
		for (let i = 0; i < 7; i++) {
			delays.push(pending()[0].ms);
			await fire();
		}
		expect(delays).toEqual([2000, 5000, 15000, 30000, 60000, 300000, 300000]);

		const connectsBefore = server.log.filter((entry) => entry === 'connect').length;
		engine.setVisible(true);
		await settle();
		expect(server.log.filter((entry) => entry === 'connect')).toHaveLength(connectsBefore + 1);
		expect(pending().map((timer) => timer.ms)).toEqual([2000]);
	});

	it('a lost connection retries while visible', async () => {
		const { engine, server, last, pending, fire } = setup();
		engine.start(true);
		await settle();

		server.clients[0].drop();
		await settle();
		expect(last().status).toBe('unreachable');
		expect(pending().map((timer) => timer.ms)).toEqual([2000]);

		await fire();
		expect(last().status).toBe('connected');
		expect(server.clients).toHaveLength(2);
	});

	it('hidden closes and stops', async () => {
		const { engine, server, pending, edit } = setup();
		engine.start(true);
		await settle();
		edit(['a']);
		expect(pending()).toHaveLength(1);

		engine.setVisible(false);
		expect(server.log).toContain('close#1');
		expect(pending()).toEqual([]);

		// A failure while hidden schedules nothing.
		server.connectError = 'connectTimeout';
		engine.online();
		engine.localChanged();
		await settle();
		expect(pending()).toEqual([]);
		expect(server.log.filter((entry) => entry === 'connect')).toHaveLength(1);
	});

	it('a connection that fails after hiding schedules nothing', async () => {
		const { engine, server, pending } = setup();
		server.connectError = 'connectTimeout';
		engine.start(true);
		engine.setVisible(false);
		await settle();

		expect(pending()).toEqual([]);
	});

	it('one connection at a time', async () => {
		const { engine, server } = setup();
		engine.start(true);
		await settle();

		engine.setVisible(false);
		engine.setVisible(true);
		await settle();
		await engine.pair(CODE);
		await settle();

		const order = server.log.filter((entry) => entry === 'connect' || entry.startsWith('close'));
		expect(order).toEqual(['connect', 'close#1', 'connect', 'close#2', 'connect']);
	});

	it('the write guard stops after ten writes a minute', async () => {
		const { engine, server, state, last, fire, edit } = setup({ local: ['a'] });
		engine.start(true);
		await settle();
		expect(server.puts).toHaveLength(1);

		for (let i = 1; i <= 10; i++) {
			state.now = START + i * 1000;
			edit([...state.local, `edit-${i}`]);
			await fire();
		}

		expect(server.puts).toHaveLength(10);
		expect(last()).toMatchObject({ status: 'error', error: { code: 'tooManyWrites' } });
		expect(decodeNotes(server.body)).toContain('edit-9');
		expect(decodeNotes(server.body)).not.toContain('edit-10');
	});

	it('writes again once the minute has passed', async () => {
		const { engine, server, state, last, fire, edit } = setup({ local: ['a'] });
		engine.start(true);
		await settle();

		for (let i = 1; i <= 9; i++) {
			state.now = START + i * 1000;
			edit([...state.local, `edit-${i}`]);
			await fire();
		}
		state.now = START + 61_000;
		edit([...state.local, 'later']);
		await fire();

		expect(server.puts).toHaveLength(11);
		expect(last().status).toBe('connected');
	});

	it('a document the app cannot read halts sync and is not written over', async () => {
		const { engine, server, last, pending, replaceLocal } = setup({ local: ['a'] });
		server.store(encodeNotes([], 99));

		engine.start(true);
		await settle();

		expect(last()).toMatchObject({ status: 'error', error: { code: 'newerSchema' } });
		expect(last().error).toBeInstanceOf(DocumentError);
		expect(replaceLocal).not.toHaveBeenCalled();
		expect(server.puts).toEqual([]);
		expect(pending()).toEqual([]);

		server.log.length = 0;
		await engine.syncNow();
		expect(server.log).toEqual([]);
	});

	it('a damaged document halts with the app code', async () => {
		const { engine, server, last } = setup({ local: ['a'] });
		server.store('not json');

		engine.start(true);
		await settle();

		expect(last()).toMatchObject({ status: 'error', error: { code: 'badDocument' } });
		expect(server.puts).toEqual([]);
	});

	it('any other failure of the adapter reads as internal, and is retried', async () => {
		const { engine, adapter, last, pending } = setup({ local: ['a'] });
		vi.spyOn(adapter, 'plan').mockImplementationOnce(() => {
			throw new TypeError('a bug in the app');
		});

		engine.start(true);
		await settle();

		expect(last()).toMatchObject({ status: 'paused', error: { code: 'internal' } });
		expect(pending().map((timer) => timer.ms)).toEqual([2000]);
	});

	it('pairing saves the pairing and syncs', async () => {
		const { engine, server, state, last } = setup({ pairing: null, local: ['a'] });
		engine.start(true);

		await engine.pair(CODE);

		expect(state.pairing).toEqual({
			serverId: 'S1',
			name: 'Desk',
			device: 'dev2',
			token: 'tok2',
			password: 'b'.repeat(24),
			fingerprint: 'CC:DD',
			port: 47801,
			hosts: ['a.local', '192.168.1.5'],
			lastEtag: server.etag,
			lastSyncAt: '2026-11-07T12:00:00.000Z',
		});
		expect(server.log).toEqual(['connect', 'pair#1', 'head#1', 'get#1', 'put#1']);
		expect(last().status).toBe('connected');
	});

	it('pairing merges with what the server holds', async () => {
		const { engine, server, state } = setup({ pairing: null, local: ['a'] });
		server.store(encodeNotes(['b']));
		engine.start(true);

		await engine.pair(CODE);

		expect(decodeNotes(server.body)).toEqual(['a', 'b']);
		expect(state.local).toEqual(['a', 'b']);
	});

	it('a failed pairing is rethrown and saves nothing', async () => {
		const { engine, server, state, last } = setup({ pairing: null });
		server.pairError = 'badSecret';
		engine.start(true);

		await expect(engine.pair(CODE)).rejects.toMatchObject({ code: 'badSecret' });

		expect(state.pairing).toBeNull();
		expect(last().status).toBe('unpaired');
		expect(server.log).toEqual(['connect', 'pair#1', 'close#1']);
	});

	it('pairing again clears a halt', async () => {
		const { engine, server, last } = setup({
			pairing: makePairing({ token: 'wrong' }),
			local: ['a'],
		});
		engine.start(true);
		await settle();
		expect(last().error?.code).toBe('badToken');

		await engine.pair(CODE);

		expect(last()).toMatchObject({ status: 'connected', error: null });
		expect(server.puts).toHaveLength(1);
	});

	it('disconnect forgets and clears', async () => {
		const { engine, server, state, last } = setup();
		engine.start(true);
		await settle();

		await engine.disconnect();

		expect(server.log).toContain('forget#1');
		expect(server.devices.size).toBe(0);
		expect(state.pairing).toBeNull();
		expect(last()).toEqual({
			status: 'unpaired',
			serverName: null,
			via: null,
			lastSyncAt: null,
			error: null,
		});
	});

	it('disconnect while unreachable clears the pairing without forget', async () => {
		const { engine, server, state, pending } = setup();
		server.connectError = 'connectTimeout';
		engine.start(true);
		await settle();

		await engine.disconnect();

		expect(server.log.some((entry) => entry.startsWith('forget'))).toBe(false);
		expect(state.pairing).toBeNull();
		expect(pending()).toEqual([]);
	});

	it('a local edit made during a sync is synced next', async () => {
		const { engine, server, state, fire, edit } = setup({ local: ['a'] });
		engine.start(true);
		await settle();

		// The edit lands after the local state was read for the merge.
		const realPut = server.clients[0].put.bind(server.clients[0]);
		let edited = false;
		vi.spyOn(server.clients[0], 'put').mockImplementation(async (ifEtag, body) => {
			if (!edited) {
				edited = true;
				edit([...state.local, 'during']);
			}
			return realPut(ifEtag, body);
		});
		edit(['a', 'before']);
		await fire();
		await fire();

		expect(decodeNotes(server.body)).toEqual(['a', 'before', 'during']);
	});

	it('two devices on one server settle without writing back and forth', async () => {
		const a = setup({ local: ['a'] });
		const b = setup({ local: ['b'] });
		// One server for both devices.
		const engineB = new SyncEngine({ ...b.engineOptions, connect: a.server.connect });

		a.engine.start(true);
		await settle();
		engineB.start(true);
		await settle();
		await settle();
		const writes = a.server.puts.length;
		await a.engine.syncNow();
		await engineB.syncNow();
		await settle();

		expect(a.server.puts).toHaveLength(writes);
		expect(decodeNotes(a.server.body)).toEqual(['a', 'b']);
		expect(a.state.local).toEqual(['a', 'b']);
		expect(b.state.local).toEqual(['a', 'b']);
	});
});
