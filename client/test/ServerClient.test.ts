import { afterEach, describe, expect, it, vi } from 'vitest';
import { ServerClient, listAll, type ListPage, type Stale } from '../src/ServerClient.js';
import type { WorkspaceError } from '../src/WorkspaceError.js';
import { FakeChannel } from '../src/testing/FakeChannel.js';

const WELCOME = {
	t: 'welcome',
	proto: 1,
	serverId: 'S1',
	name: 'Desk',
	revision: 'r1',
	hosts: ['a.local', '192.168.1.5'],
};

const DATA = { path: '', id: 'data', type: 'json' };
const PHOTO = { path: 'photos/2026', id: 'p1', type: 'jpg' };

/** Lets pending promise callbacks run. */
async function flush(): Promise<void> {
	for (let i = 0; i < 10; i++) await Promise.resolve();
}

function setup() {
	const channel = new FakeChannel();
	const client = new ServerClient(channel, { app: 'todo' });

	return { channel, client };
}

/** The binary messages sent so far. */
function pieces(channel: FakeChannel): Uint8Array[] {
	return channel.sent.filter((item): item is Uint8Array => typeof item !== 'string');
}

describe('ServerClient', () => {
	afterEach(() => {
		vi.useRealTimers();
	});

	it('hello sends proto and app and resolves the welcome', async () => {
		const { channel, client } = setup();

		const welcome = client.hello('d1', 'tok');
		await flush();
		expect(channel.sentFrames()).toEqual([
			{ t: 'hello', proto: 1, app: 'todo', device: 'd1', token: 'tok' },
		]);
		channel.receiveFrame(WELCOME);

		await expect(welcome).resolves.toEqual({
			serverId: 'S1',
			name: 'Desk',
			revision: 'r1',
			hosts: ['a.local', '192.168.1.5'],
		});
	});

	it('a welcome naming diverged servers carries them; an empty list is left out', async () => {
		const { channel, client } = setup();
		const since = '2026-11-13T09:00:00Z';

		const first = client.hello('d1', 'tok');
		await flush();
		channel.receiveFrame({ ...WELCOME, diverged: [{ serverId: 'L1', name: 'Laptop', since }] });
		await expect(first).resolves.toMatchObject({
			diverged: [{ serverId: 'L1', name: 'Laptop', since }],
		});

		const second = setup();
		const empty = second.client.hello('d1', 'tok');
		await flush();
		second.channel.receiveFrame({ ...WELCOME, diverged: [] });
		await expect(empty).resolves.not.toHaveProperty('diverged');
	});

	it('pair resolves after paired and welcome', async () => {
		const { channel, client } = setup();
		let settled = false;

		const pairing = client.pair('sec', 'iPhone Safari');
		void pairing.then(() => (settled = true));
		await flush();
		expect(channel.sentFrames()).toEqual([
			{ t: 'pair', proto: 1, app: 'todo', secret: 'sec', name: 'iPhone Safari' },
		]);

		channel.receiveFrame({ t: 'paired', device: 'dev1', token: 'tok1' });
		await flush();
		expect(settled).toBe(false);
		channel.receiveFrame({ ...WELCOME, revision: null });

		await expect(pairing).resolves.toEqual({
			device: 'dev1',
			token: 'tok1',
			welcome: { serverId: 'S1', name: 'Desk', revision: null, hosts: ['a.local', '192.168.1.5'] },
		});
	});

	it('head resolves the revision', async () => {
		const { channel, client } = setup();

		const head = client.head();
		await flush();
		expect(channel.sentFrames()).toEqual([{ t: 'head' }]);
		channel.receiveFrame({ t: 'head', revision: null });

		await expect(head).resolves.toBeNull();
	});

	it('listAll reads every page at one revision, and starts over when the set moves', async () => {
		const pages: (ListPage | Stale)[] = [
			{ revision: 'r1', docs: [{ ...DATA, etag: 'e1', bytes: 2 }], removed: [], cursor: 'c1' },
			{ stale: 'r2' },
			{ revision: 'r2', docs: [{ ...DATA, etag: 'e2', bytes: 2 }], removed: [], cursor: 'c1' },
			{ revision: 'r2', docs: [], removed: [{ ...PHOTO, at: '2026-11-13T10:00:00Z' }] },
		];
		const asked: unknown[] = [];
		const client = {
			list: async (options?: unknown) => {
				asked.push(options);
				return pages.shift() as ListPage | Stale;
			},
		};

		await expect(listAll(client)).resolves.toEqual({
			revision: 'r2',
			docs: [{ ...DATA, etag: 'e2', bytes: 2 }],
			removed: [{ ...PHOTO, at: '2026-11-13T10:00:00Z' }],
		});
		expect(asked).toEqual([
			undefined,
			{ revision: 'r1', cursor: 'c1' },
			undefined,
			{ revision: 'r2', cursor: 'c1' },
		]);
	});

	it('listAll gives up with conflict on a set that never holds still', async () => {
		let n = 0;
		const client = {
			list: async (): Promise<ListPage | Stale> =>
				n++ % 2 === 0
					? { revision: `r${n}`, docs: [], removed: [], cursor: 'c' }
					: { stale: `r${n}` },
		};

		await expect(listAll(client)).rejects.toMatchObject({ code: 'conflict' });
	});

	it('get resolves the assembled body', async () => {
		const { channel, client } = setup();

		const doc = client.get(DATA);
		await flush();
		expect(channel.sentFrames()).toEqual([{ t: 'get', ...DATA }]);
		channel.receiveFrame({ t: 'doc', ...DATA, etag: 'e2', bytes: 3, offset: 0 });
		channel.receiveBinary([7, 8]);
		channel.receiveBinary([9]);

		await expect(doc).resolves.toEqual({
			...DATA,
			etag: 'e2',
			bytes: 3,
			offset: 0,
			body: Uint8Array.from([7, 8, 9]),
		});
	});

	it('get hands the body to onPiece as it arrives, and resolves with an empty body', async () => {
		const { channel, client } = setup();
		const heard: number[][] = [];

		const doc = client.get(DATA, {
			etag: 'e2',
			offset: 1,
			onPiece: (piece) => heard.push([...piece]),
		});
		await flush();
		expect(channel.sentFrames()).toEqual([{ t: 'get', ...DATA, etag: 'e2', offset: 1 }]);
		channel.receiveFrame({ t: 'doc', ...DATA, etag: 'e2', bytes: 4, offset: 1 });
		channel.receiveBinary([7, 8]);
		expect(heard).toEqual([[7, 8]]);
		channel.receiveBinary([9]);

		await expect(doc).resolves.toEqual({
			...DATA,
			etag: 'e2',
			bytes: 4,
			offset: 1,
			body: new Uint8Array(0),
		});
		expect(heard).toEqual([[7, 8], [9]]);
	});

	it('an onPiece that throws fails the get once the body is in, and the client goes on', async () => {
		const { channel, client } = setup();
		const broken = new Error('disk full');
		let calls = 0;

		const doc = client.get(DATA, {
			onPiece: () => {
				calls++;
				throw broken;
			},
		});
		doc.catch(() => undefined);
		await flush();
		channel.receiveFrame({ t: 'doc', ...DATA, etag: 'e2', bytes: 3 });
		channel.receiveBinary([7, 8]);
		channel.receiveBinary([9]);

		await expect(doc).rejects.toBe(broken);
		expect(calls).toBe(1);
		const head = client.head();
		await flush();
		channel.receiveFrame({ t: 'head', revision: 'r1' });
		await expect(head).resolves.toBe('r1');
	});

	it('get resolves stale when the document left the listing', async () => {
		const { channel, client } = setup();

		const doc = client.get(DATA, { etag: 'e1' });
		await flush();
		channel.receiveFrame({ t: 'stale', revision: 'r3' });

		await expect(doc).resolves.toEqual({ stale: 'r3' });
	});

	it('a get past offset 0 needs an etag', async () => {
		const { channel, client } = setup();

		await expect(client.get(DATA, { offset: 3 })).rejects.toBeInstanceOf(RangeError);
		expect(channel.sent).toEqual([]);
	});

	it('a doc that is not the one asked for is a protocol failure', async () => {
		const { channel, client } = setup();

		const doc = client.get(DATA);
		await flush();
		channel.receiveFrame({ t: 'doc', ...PHOTO, etag: 'e2', bytes: 0 });

		await expect(doc).rejects.toMatchObject({ code: 'protocol' });
		expect(channel.readyState).toBe('closed');
	});

	it('begin, put, commit: a transaction', async () => {
		const { channel, client } = setup();

		const began = client.begin('tx-0123456789abcdef');
		await flush();
		channel.receiveFrame({ t: 'began', tx: 'tx-0123456789abcdef', staged: [] });
		await expect(began).resolves.toEqual({ tx: 'tx-0123456789abcdef', staged: [] });

		const put = client.put('tx-0123456789abcdef', DATA, 'e1', Uint8Array.from([1, 2, 3]));
		await flush();
		expect(channel.sentFrames()[1]).toEqual({
			t: 'put',
			tx: 'tx-0123456789abcdef',
			...DATA,
			ifEtag: 'e1',
			bytes: 3,
			offset: 0,
		});
		expect(pieces(channel)).toEqual([Uint8Array.from([1, 2, 3])]);
		channel.receiveFrame({ t: 'staged', ...DATA, received: 3 });
		await expect(put).resolves.toEqual({ ...DATA, received: 3 });

		const commit = client.commit('tx-0123456789abcdef');
		await flush();
		channel.receiveFrame({ t: 'committed', revision: 'r2', docs: [{ ...DATA, etag: 'e2' }] });
		await expect(commit).resolves.toEqual({
			committed: true,
			revision: 'r2',
			docs: [{ ...DATA, etag: 'e2' }],
		});
	});

	it('put resumes a Blob from an offset', async () => {
		const { channel, client } = setup();

		const put = client.put('tx-0123456789abcdef', PHOTO, null, new Blob(['jpeg!']), 2);
		await flush();
		await new Promise((resolve) => setTimeout(resolve, 0));
		expect(channel.sentFrames()).toEqual([
			{ t: 'put', tx: 'tx-0123456789abcdef', ...PHOTO, ifEtag: null, bytes: 5, offset: 2 },
		]);
		expect(new TextDecoder().decode(pieces(channel)[0])).toBe('eg!');
		channel.receiveFrame({ t: 'staged', ...PHOTO, received: 5 });

		await expect(put).resolves.toEqual({ ...PHOTO, received: 5 });
	});

	it('a put whose offset is past its body sends nothing', async () => {
		const { channel, client } = setup();

		await expect(
			client.put('tx-0123456789abcdef', DATA, null, new Uint8Array(2), 3),
		).rejects.toBeInstanceOf(RangeError);
		expect(channel.sent).toEqual([]);
	});

	it('a commit that conflicts resolves, not committed', async () => {
		const { channel, client } = setup();

		const commit = client.commit('tx-0123456789abcdef');
		await flush();
		channel.receiveFrame({ t: 'conflict', revision: 'r3', docs: [{ ...DATA, etag: null }] });

		await expect(commit).resolves.toEqual({
			committed: false,
			revision: 'r3',
			docs: [{ ...DATA, etag: null }],
		});
	});

	it('a refused put answers after its body', async () => {
		const { channel, client } = setup();

		const put = client.put('tx-0123456789abcdef', PHOTO, null, Uint8Array.from([1]));
		await flush();
		expect(pieces(channel)).toHaveLength(1);
		channel.receiveFrame({ t: 'error', code: 'quota', message: 'over the quota' });

		await expect(put).rejects.toMatchObject({ code: 'quota' });
	});

	it('delete and abort', async () => {
		const { channel, client } = setup();

		const removed = client.delete('tx-0123456789abcdef', PHOTO, 'e3');
		await flush();
		channel.receiveFrame({ t: 'staged', ...PHOTO, received: 0 });
		await expect(removed).resolves.toEqual({ ...PHOTO, received: 0 });

		const aborted = client.abort('tx-0123456789abcdef');
		await flush();
		channel.receiveFrame({ t: 'aborted', tx: 'tx-0123456789abcdef' });
		await expect(aborted).resolves.toEqual({ tx: 'tx-0123456789abcdef' });

		expect(channel.sentFrames()).toEqual([
			{ t: 'delete', tx: 'tx-0123456789abcdef', ...PHOTO, ifEtag: 'e3' },
			{ t: 'abort', tx: 'tx-0123456789abcdef' },
		]);
	});

	it('resolve resolves with its answer, which the listeners hear too; a push only reaches them', async () => {
		const { channel, client } = setup();
		const heard: string[] = [];
		client.onResolved((server) => heard.push(server));

		const resolving = client.resolve('L1', 'main');
		await flush();
		expect(channel.sentFrames()).toEqual([{ t: 'resolve', server: 'L1', keep: 'main' }]);
		channel.receiveFrame({ t: 'resolved', server: 'L1' });
		await expect(resolving).resolves.toEqual({ server: 'L1' });

		channel.receiveFrame({ t: 'resolved', server: 'L2' });
		expect(heard).toEqual(['L1', 'L2']);
	});

	it('diverged reaches the listener', () => {
		const { channel, client } = setup();
		const heard: unknown[] = [];
		client.onDiverged((servers) => heard.push(servers));

		channel.receiveFrame({ t: 'diverged', servers: [] });

		expect(heard).toEqual([[]]);
	});

	it('an error frame rejects with its code', async () => {
		const { channel, client } = setup();

		const welcome = client.hello('d1', 'bad');
		await flush();
		channel.receiveFrame({ t: 'error', code: 'badToken', message: 'unknown device' });

		await expect(welcome).rejects.toMatchObject({
			name: 'WorkspaceError',
			code: 'badToken',
			message: 'unknown device',
		});
	});

	it('an unknown error code becomes internal', async () => {
		const { channel, client } = setup();

		const head = client.head();
		await flush();
		channel.receiveFrame({ t: 'error', code: 'diskOnFire', message: 'x' });

		await expect(head).rejects.toMatchObject({ code: 'internal' });
	});

	it('requests are serialized', async () => {
		const { channel, client } = setup();

		const first = client.head();
		const second = client.head();
		await flush();
		expect(channel.sentFrames()).toEqual([{ t: 'head' }]);

		channel.receiveFrame({ t: 'head', revision: 'r1' });
		await expect(first).resolves.toBe('r1');
		await flush();
		expect(channel.sentFrames()).toEqual([{ t: 'head' }, { t: 'head' }]);

		channel.receiveFrame({ t: 'head', revision: 'r2' });
		await expect(second).resolves.toBe('r2');
	});

	it('a failed request does not hold up the next', async () => {
		const { channel, client } = setup();

		const first = client.head();
		const second = client.head();
		await flush();
		channel.receiveFrame({ t: 'error', code: 'internal', message: 'disk' });
		await expect(first).rejects.toMatchObject({ code: 'internal' });
		await flush();
		channel.receiveFrame({ t: 'head', revision: 'r2' });

		await expect(second).resolves.toBe('r2');
	});

	it('changed reaches the listener, null for an empty set', () => {
		const { channel, client } = setup();
		const revisions: (string | null)[] = [];
		const unsubscribe = client.onChanged((revision) => revisions.push(revision));

		channel.receiveFrame({ t: 'changed', revision: 'r5' });
		channel.receiveFrame({ t: 'changed', revision: null });
		unsubscribe();
		channel.receiveFrame({ t: 'changed', revision: 'r6' });

		expect(revisions).toEqual(['r5', null]);
	});

	it('a close rejects the pending request with closed and notifies onClosed once', async () => {
		const { channel, client } = setup();
		const closed: WorkspaceError[] = [];
		client.onClosed((error) => closed.push(error));

		const head = client.head();
		await flush();
		channel.fireClose();
		channel.fireClose();

		await expect(head).rejects.toMatchObject({ code: 'closed' });
		expect(closed.map((error) => error.code)).toEqual(['closed']);
		await expect(client.head()).rejects.toMatchObject({ code: 'closed' });
	});

	it('an unexpected reply is a protocol failure that closes the channel', async () => {
		const { channel, client } = setup();
		const closed: WorkspaceError[] = [];
		client.onClosed((error) => closed.push(error));

		const head = client.head();
		await flush();
		channel.receiveFrame({ t: 'staged', ...DATA, received: 0 });

		await expect(head).rejects.toMatchObject({ code: 'protocol' });
		expect(closed.map((error) => error.code)).toEqual(['protocol']);
		expect(channel.readyState).toBe('closed');
	});

	it('close rejects what waits without notifying onClosed', async () => {
		const { channel, client } = setup();
		const closed: WorkspaceError[] = [];
		client.onClosed((error) => closed.push(error));

		const head = client.head();
		await flush();
		client.close();

		await expect(head).rejects.toMatchObject({ code: 'closed' });
		expect(closed).toEqual([]);
		expect(channel.readyState).toBe('closed');
	});

	it('forget resolves when the server closes the channel', async () => {
		const { channel, client } = setup();

		const forgetting = client.forget();
		await flush();
		expect(channel.sentFrames()).toEqual([{ t: 'forget' }]);
		channel.fireClose();

		await expect(forgetting).resolves.toBeUndefined();
	});

	it('forget gives up waiting after two seconds', async () => {
		vi.useFakeTimers();
		const { client } = setup();

		const forgetting = client.forget();
		await vi.advanceTimersByTimeAsync(2000);

		await expect(forgetting).resolves.toBeUndefined();
	});
});
