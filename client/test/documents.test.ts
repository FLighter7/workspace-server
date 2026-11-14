import { describe, expect, it, vi } from 'vitest';
import { MAX_WRITES_PER_MINUTE, SyncEngine } from '../src/SyncEngine.js';
import { FakeClient, FakeServer } from '../src/testing/FakeServer.js';
import { START, settle, setup } from './engineHarness.js';
import { makePairing } from './fixtures.js';

/*
 * The engine over an app of several documents (the files app of the
 * harness): what it reads, what it writes and in how many transactions, and
 * how a listing in pages, a read that finds a document changed and a commit
 * that fails all list again.
 */

const A = { path: 'notes', id: 'a', type: 'md' };
const B = { path: 'notes', id: 'b', type: 'md' };
const C = { path: 'photos/2026', id: 'c', type: 'jpg' };

describe('SyncEngine with several documents', () => {
	it('writes every changed document in one transaction', async () => {
		const device = setup({ files: true });
		device.engine.start(true);
		await settle();
		device.server.log.length = 0;

		device.writeFile(A, 'alpha');
		device.writeFile(B, 'beta');
		device.writeFile(C, 'jpeg');
		await device.fire();

		expect(device.server.log).toEqual([
			'head#1',
			'list#1',
			'begin#1',
			'put#1',
			'put#1',
			'put#1',
			'commit#1',
		]);
		expect(device.server.text(A)).toBe('alpha');
		expect(device.server.text(C)).toBe('jpeg');
		// The adapter learnt the etags it wrote: nothing to read back.
		expect(device.local.known.get('notes/a.md')).toBe(device.server.etag(A));
		device.server.log.length = 0;
		await device.engine.syncNow();
		expect(device.server.log).toEqual(['head#1']);
	});

	it('two devices exchange documents, reading only the ones that moved', async () => {
		const server = new FakeServer();
		server.devices.set('dev2', 'tok2');
		const one = setup({ files: true, server });
		const two = setup({
			files: true,
			server,
			pairing: makePairing({ device: 'dev2', token: 'tok2' }),
		});
		one.writeFile(A, 'alpha');
		one.writeFile(B, 'beta');
		one.engine.start(true);
		await settle();

		two.writeFile(C, 'jpeg');
		two.engine.start(true);
		await settle();
		await settle();

		expect(two.filesText()).toEqual({
			'notes/a.md': 'alpha',
			'notes/b.md': 'beta',
			'photos/2026/c.jpg': 'jpeg',
		});
		// The first device hears the push, and reads only the new document.
		const gets = server.log.filter((entry) => entry === 'get#1');
		expect(gets).toHaveLength(1);
		expect(one.filesText()).toEqual(two.filesText());

		// A document deleted on one device goes from the other.
		one.removeFile(B);
		await one.fire();
		await settle();
		expect(server.documents.has('notes/b.md')).toBe(false);
		expect([...server.removed.keys()]).toEqual(['notes/b.md']);
		expect(two.filesText()).toEqual({ 'notes/a.md': 'alpha', 'photos/2026/c.jpg': 'jpeg' });
	});

	it('reads what another device wrote between its listing and its commit', async () => {
		const device = setup({ files: true });
		device.writeFile(A, 'alpha');
		const original = FakeClient.prototype.begin;
		let raced = false;
		const begin = vi.spyOn(FakeClient.prototype, 'begin').mockImplementation(function (
			this: FakeClient,
			tx,
		) {
			if (!raced) {
				// Another device's commit of another document, which pushes nothing here.
				raced = true;
				device.server.storeDocument(C, 'jpeg');
			}
			return original.call(this, tx);
		});

		device.engine.start(true);
		await settle();

		// The commit's revision holds a document it never read: it syncs again.
		expect(device.filesText()).toEqual({ 'notes/a.md': 'alpha', 'photos/2026/c.jpg': 'jpeg' });
		expect(device.state.pairing?.lastRevision).toBe(device.server.revision);
		expect(device.server.log.filter((entry) => entry.startsWith('list'))).toHaveLength(2);
		begin.mockRestore();
	});

	it('leaves out a body the server holds already and a delete of what it does not hold', async () => {
		const device = setup({ files: true });
		device.server.storeDocument(A, 'alpha');
		device.writeFile(A, 'alpha');
		device.removeFile(B);

		device.engine.start(true);
		await settle();

		expect(device.server.log).not.toContain('begin#1');
		expect(device.last()).toMatchObject({ status: 'connected', error: null });
	});

	it('reads a listing of many pages, and starts it over when the set moves between them', async () => {
		const device = setup({ files: true });
		const server = device.server;
		server.pageSize = 2;
		for (let i = 0; i < 5; i++)
			server.storeDocument({ path: 'n', id: `d${i}`, type: 'txt' }, `${i}`);
		server.removeDocument({ path: 'n', id: 'd4', type: 'txt' });
		const original = FakeClient.prototype.list;
		let moved = false;
		const list = vi.spyOn(FakeClient.prototype, 'list').mockImplementation(function (
			this: FakeClient,
			options,
		) {
			if (options?.cursor && !moved) {
				moved = true;
				server.storeDocument({ path: 'n', id: 'late', type: 'txt' }, 'late');
			}
			return original.call(this, options);
		});

		device.engine.start(true);
		await settle();

		expect(Object.keys(device.filesText()).sort()).toEqual([
			'n/d0.txt',
			'n/d1.txt',
			'n/d2.txt',
			'n/d3.txt',
			'n/late.txt',
		]);
		// One page, stale, then three pages of five documents and a tombstone.
		expect(list).toHaveBeenCalledTimes(5);
		list.mockRestore();
	});

	it('a document deleted between the listing and the read is listed again', async () => {
		const device = setup({ files: true });
		device.server.storeDocument(A, 'alpha');
		device.server.storeDocument(B, 'beta');
		const original = FakeClient.prototype.get;
		let raced = false;
		const get = vi.spyOn(FakeClient.prototype, 'get').mockImplementation(function (
			this: FakeClient,
			key,
			options,
		) {
			if (!raced) {
				raced = true;
				device.server.removeDocument(B);
			}
			return original.call(this, key, options);
		});

		device.engine.start(true);
		await settle();

		expect(device.filesText()).toEqual({ 'notes/a.md': 'alpha' });
		expect(device.server.log.filter((entry) => entry === 'list#1')).toHaveLength(2);
		get.mockRestore();
	});

	it('a commit that fails after the commit point lists again and never commits it again', async () => {
		const device = setup({ files: true });
		device.server.commitError = 'internal';
		device.server.commitAppliedFirst = true;
		device.writeFile(A, 'alpha');
		device.writeFile(B, 'beta');

		device.engine.start(true);
		await settle();

		expect(device.server.log.filter((entry) => entry.startsWith('commit'))).toHaveLength(1);
		expect(device.server.log.filter((entry) => entry.startsWith('list'))).toHaveLength(2);
		// The second listing found both written; the transaction kept to tell is let go.
		expect(device.server.log.at(-1)).toBe('abort#1');
		expect(device.server.text(B)).toBe('beta');
		expect(device.last()).toMatchObject({ status: 'connected', error: null });
		expect(device.state.pairing?.lastRevision).toBe(device.server.revision);
	});

	it('a body staged for a key no longer wanted starts another transaction', async () => {
		const device = setup({ files: true });
		device.server.conflicts = 1;
		device.writeFile(A, 'alpha');
		device.writeFile(B, 'beta');
		// After the conflict, B is no longer to be written.
		const original = FakeClient.prototype.commit;
		let first = true;
		const spy = vi.spyOn(FakeClient.prototype, 'commit').mockImplementation(async function (
			this: FakeClient,
			tx,
		) {
			const result = await original.call(this, tx);
			if (first) {
				first = false;
				device.local.dirty.delete('notes/b.md');
				device.local.files.delete('notes/b.md');
			}
			return result;
		});

		device.engine.start(true);
		await settle();

		const begins = device.server.log.filter((entry) => entry.startsWith('begin'));
		expect(begins).toHaveLength(3);
		expect(device.server.text(A)).toBe('alpha');
		expect(device.server.text(B)).toBeNull();
		spy.mockRestore();
	});

	it('the write guard counts commits, however many documents each writes', async () => {
		const device = setup({ files: true });
		device.engine.start(true);
		await settle();

		for (let i = 1; i <= MAX_WRITES_PER_MINUTE + 1; i++) {
			device.state.now = START + i * 1000;
			device.writeFile({ path: 'n', id: `x${i}`, type: 'txt' }, `${i}`);
			device.writeFile({ path: 'n', id: `y${i}`, type: 'txt' }, `${i}`);
			await device.fire();
		}

		expect(device.server.log.filter((entry) => entry.startsWith('commit'))).toHaveLength(
			MAX_WRITES_PER_MINUTE,
		);
		expect(device.server.puts).toHaveLength(MAX_WRITES_PER_MINUTE * 2);
		expect(device.last()).toMatchObject({ status: 'error', error: { code: 'tooManyWrites' } });
	});

	it('writes a Blob, hashing it to leave out one the server holds', async () => {
		const device = setup({ files: true });
		const adapter = device.engineOptions.adapter;
		const engine = new SyncEngine({
			...device.engineOptions,
			adapter: {
				...adapter,
				plan: () => ({ put: [{ doc: C, body: new Blob(['jpeg bytes']) }] }),
			},
		});

		engine.start(true);
		await settle();
		expect(device.server.text(C)).toBe('jpeg bytes');
		device.server.log.length = 0;

		device.local.changeCount++;
		await engine.syncNow();
		expect(device.server.log).toEqual(['head#1', 'list#1']);
	});

	it('a plan that puts a document unchanged and deletes it keeps it', async () => {
		const device = setup({ files: true });
		device.server.storeDocument(A, 'alpha');
		const engine = new SyncEngine({
			...device.engineOptions,
			adapter: {
				...device.engineOptions.adapter,
				plan: () => ({ put: [{ doc: A, body: new TextEncoder().encode('alpha') }], delete: [A] }),
			},
		});

		engine.start(true);
		await settle();

		expect(device.server.text(A)).toBe('alpha');
		expect(device.server.log).not.toContain('begin#1');
	});

	it('tells the adapter of a body the server held already, and of a delete of what it did not hold', async () => {
		const device = setup({ files: true });
		const etag = device.server.storeDocument(A, 'alpha');
		device.local.known.set('notes/a.md', 'older');
		device.writeFile(A, 'alpha');
		device.removeFile(B);

		device.engine.start(true);
		await settle();

		expect(device.local.known.get('notes/a.md')).toBe(etag);
		expect(device.local.dirty.size).toBe(0);
		expect(device.local.deleted.size).toBe(0);
	});

	it('a commit naming another etag than the body sent is not told to the adapter, and syncs again', async () => {
		const device = setup({ files: true });
		device.writeFile(A, 'alpha');
		const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
		const original = FakeClient.prototype.commit;
		const commit = vi.spyOn(FakeClient.prototype, 'commit').mockImplementationOnce(async function (
			this: FakeClient,
			tx: string,
		) {
			const result = await original.call(this, tx);
			return { ...result, docs: result.docs.map((doc) => ({ ...doc, etag: 'f'.repeat(64) })) };
		});
		const committed = vi.spyOn(device.engineOptions.adapter, 'committed');

		device.engine.start(true);
		await settle();

		expect(committed.mock.calls[0]?.[0] ?? []).not.toContainEqual(
			expect.objectContaining({ id: 'a', etag: 'f'.repeat(64) }),
		);
		expect(warn).toHaveBeenCalled();
		// Synced again: the listing then names the body that is there.
		expect(device.server.log.filter((entry) => entry.startsWith('list'))).toHaveLength(2);
		expect(device.state.pairing?.lastRevision).toBe(device.server.revision);
		commit.mockRestore();
		warn.mockRestore();
	});

	it('a plan the server refused for its quota is not sent again until something changes', async () => {
		const device = setup({ files: true });
		device.server.quota = 1000;
		device.writeFile(A, 'x'.repeat(2000));

		device.engine.start(true);
		await settle();
		expect(device.last()).toMatchObject({ status: 'error', error: { code: 'quota' } });
		expect(device.server.puts).toHaveLength(1);

		// An edit that leaves the plan as it was, and a push: nothing is sent.
		device.local.changeCount++;
		device.engine.localChanged();
		await device.fire();
		device.server.clients[0].emitChanged(device.server.revision);
		await settle();
		expect(device.server.puts).toHaveLength(1);
		expect(device.last()).toMatchObject({ status: 'error', error: { code: 'quota' } });

		// The set changed: the plan is tried against it.
		device.server.storeDocument(C, 'jpeg');
		device.server.clients[0].emitChanged(device.server.revision);
		await settle();
		expect(device.server.puts).toHaveLength(2);

		// The user asks, after raising the quota: it goes.
		device.server.quota = null;
		await device.engine.syncNow();
		expect(device.server.text(A)).toBe('x'.repeat(2000));
		expect(device.last()).toMatchObject({ status: 'connected', error: null });
	});

	it('Sync now sends a refused plan again with the sync it starts only, even one that waits', async () => {
		const device = setup({ files: true });
		const big = { path: 'videos', id: 'v', type: 'mp4' };
		device.server.storeDocument(big, new Uint8Array(3 * 1024 * 1024));
		device.server.quota = 1000;
		const wanted: { path: string; id: string; type: string }[] = [];
		const engine = new SyncEngine({
			...device.engineOptions,
			adapter: {
				changeCount: () => device.local.changeCount,
				select: () => wanted,
				plan: () => ({ put: [{ doc: A, body: new TextEncoder().encode('x'.repeat(2000)) }] }),
			},
		});
		engine.start(true);
		await settle();
		expect(device.server.puts).toHaveLength(1);

		// A large read of the same set is cut off, and waits.
		wanted.push(big);
		device.server.cutGetAfter = 1024 * 1024;
		device.server.clients[0].emitChanged(device.server.revision);
		await settle();
		await device.fire(2000);
		expect(device.states.at(-1)?.status).toBe('interrupted');

		// Sync now while it waits: that sync stops at the question.
		await engine.syncNow();
		expect(device.states.at(-1)?.status).toBe('interrupted');

		// Resumed, the read finishes; the refused plan, the set unchanged, is not sent.
		await engine.resumeTransfer();
		expect(device.server.puts).toHaveLength(1);
		expect(device.states.at(-1)).toMatchObject({ status: 'error', error: { code: 'quota' } });
	});
});
