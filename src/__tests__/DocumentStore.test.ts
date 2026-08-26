import { mkdtemp, readFile, readdir, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { DocumentStore, PreconditionFailedError } from '../DocumentStore.ts';
import { InvalidNameError } from '../names.ts';

let root: string;
let store: DocumentStore;

beforeEach(async () => {
	root = await mkdtemp(join(tmpdir(), 'workspace-server-'));
	store = new DocumentStore(root);
});

afterEach(() => {
	// Left on disk deliberately: the OS reclaims tmpdir, and a failed test
	// keeps whatever it wrote for inspection.
});

const BODY = JSON.stringify({ hello: 'world' });

describe('DocumentStore.read', () => {
	it('reports nothing for a document that was never written', async () => {
		expect(await store.read('todo', 'data')).toBeNull();
	});

	it('reports nothing for an app directory that does not exist', async () => {
		expect(await store.read('never-used', 'data')).toBeNull();
	});

	it('returns the body exactly as it was written', async () => {
		await store.write('todo', 'data', BODY, null);

		expect((await store.read('todo', 'data'))?.body).toBe(BODY);
	});

	it('returns a quoted etag, as HTTP requires', async () => {
		await store.write('todo', 'data', BODY, null);
		const stored = await store.read('todo', 'data');

		expect(stored?.etag).toMatch(/^"[0-9a-f]{64}"$/);
	});

	it('derives the etag from content, so identical bodies share one', async () => {
		await store.write('todo', 'data', BODY, null);
		await store.write('other', 'data', BODY, null);

		const one = await store.read('todo', 'data');
		const other = await store.read('other', 'data');

		expect(one?.etag).toBe(other?.etag);
	});

	it('gives different content different etags', async () => {
		await store.write('todo', 'data', BODY, null);
		const first = await store.read('todo', 'data');

		await store.write('todo', 'data', JSON.stringify({ hello: 'moon' }), first?.etag ?? null);
		const second = await store.read('todo', 'data');

		expect(second?.etag).not.toBe(first?.etag);
	});
});

describe('DocumentStore.write — preconditions', () => {
	it('creates a document when none is expected and none exists', async () => {
		const result = await store.write('todo', 'data', BODY, null);

		expect(result.etag).toMatch(/^"[0-9a-f]{64}"$/);
	});

	it('refuses to create over a document that already exists', async () => {
		await store.write('todo', 'data', BODY, null);

		await expect(store.write('todo', 'data', BODY, null)).rejects.toBeInstanceOf(
			PreconditionFailedError,
		);
	});

	it('updates when the expected etag matches', async () => {
		const created = await store.write('todo', 'data', BODY, null);
		const next = JSON.stringify({ hello: 'again' });

		await store.write('todo', 'data', next, created.etag);

		expect((await store.read('todo', 'data'))?.body).toBe(next);
	});

	it('refuses to update when the expected etag is stale', async () => {
		const created = await store.write('todo', 'data', BODY, null);
		await store.write('todo', 'data', JSON.stringify({ v: 2 }), created.etag);

		await expect(
			store.write('todo', 'data', JSON.stringify({ v: 3 }), created.etag),
		).rejects.toBeInstanceOf(PreconditionFailedError);
	});

	it('refuses to update a document that is not there', async () => {
		await expect(store.write('todo', 'data', BODY, '"whatever"')).rejects.toBeInstanceOf(
			PreconditionFailedError,
		);
	});

	it('leaves the stored body untouched when a precondition fails', async () => {
		const created = await store.write('todo', 'data', BODY, null);

		await expect(store.write('todo', 'data', 'clobbered', '"stale"')).rejects.toThrow();

		expect((await store.read('todo', 'data'))?.body).toBe(BODY);
		expect((await store.read('todo', 'data'))?.etag).toBe(created.etag);
	});
});

describe('DocumentStore.write — durability', () => {
	it('leaves no temporary files behind once a write settles', async () => {
		await store.write('todo', 'data', BODY, null);

		expect(await readdir(join(root, 'todo'))).toEqual(['data.json']);
	});

	it('writes a file another process can read as-is', async () => {
		await store.write('todo', 'data', BODY, null);

		expect(await readFile(join(root, 'todo', 'data.json'), 'utf8')).toBe(BODY);
	});

	it('picks up a document written underneath it by something else', async () => {
		await store.write('todo', 'data', BODY, null);
		await writeFile(join(root, 'todo', 'data.json'), '{"edited":"by hand"}', 'utf8');

		expect((await store.read('todo', 'data'))?.body).toBe('{"edited":"by hand"}');
	});

	/**
	 * Two writes racing between another writer's read and its rename must not
	 * interleave into a half-written file. Exactly one wins; the loser is
	 * rejected rather than silently dropped.
	 */
	it('serializes concurrent writes to one document instead of interleaving them', async () => {
		const created = await store.write('todo', 'data', BODY, null);

		const results = await Promise.allSettled([
			store.write('todo', 'data', JSON.stringify({ writer: 'a' }), created.etag),
			store.write('todo', 'data', JSON.stringify({ writer: 'b' }), created.etag),
		]);

		const fulfilled = results.filter((result) => result.status === 'fulfilled');
		expect(fulfilled).toHaveLength(1);

		const stored = await store.read('todo', 'data');
		expect(['{"writer":"a"}', '{"writer":"b"}']).toContain(stored?.body);
	});

	it('keeps documents of different apps apart', async () => {
		await store.write('todo', 'data', JSON.stringify({ app: 'todo' }), null);
		await store.write('notes', 'data', JSON.stringify({ app: 'notes' }), null);

		expect((await store.read('todo', 'data'))?.body).toBe('{"app":"todo"}');
		expect((await store.read('notes', 'data'))?.body).toBe('{"app":"notes"}');
	});
});

describe('DocumentStore — name safety', () => {
	const rejected = [
		'..',
		'../escape',
		'todo/../..',
		'todo\\..\\..',
		'UPPER',
		'has space',
		'has.dot',
		'',
		'a'.repeat(65),
	];

	for (const name of rejected) {
		it(`refuses ${JSON.stringify(name)} as an app name`, async () => {
			await expect(store.read(name, 'data')).rejects.toBeInstanceOf(InvalidNameError);
		});

		it(`refuses ${JSON.stringify(name)} as a document name`, async () => {
			await expect(store.write('todo', name, BODY, null)).rejects.toBeInstanceOf(InvalidNameError);
		});
	}

	it('accepts lowercase names with digits and dashes', async () => {
		await expect(store.write('my-app-2', 'doc-1', BODY, null)).resolves.toBeDefined();
	});

	it('never writes outside its root, even when asked to', async () => {
		await expect(store.write('../escape', 'data', BODY, null)).rejects.toThrow();

		expect(await readdir(root)).toEqual([]);
	});
});
