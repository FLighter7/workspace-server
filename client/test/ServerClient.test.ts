import { afterEach, describe, expect, it, vi } from 'vitest';
import { ServerClient } from '../src/ServerClient.js';
import type { WorkspaceError } from '../src/WorkspaceError.js';
import { FakeChannel } from '../src/testing/FakeChannel.js';

const WELCOME = {
	t: 'welcome',
	proto: 1,
	serverId: 'S1',
	name: 'Desk',
	etag: 'e1',
	hosts: ['a.local', '192.168.1.5'],
};

/** Lets pending promise callbacks run. */
async function flush(): Promise<void> {
	for (let i = 0; i < 10; i++) await Promise.resolve();
}

function setup() {
	const channel = new FakeChannel();
	const client = new ServerClient(channel, { app: 'todo' });

	return { channel, client };
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
			etag: 'e1',
			hosts: ['a.local', '192.168.1.5'],
		});
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
		channel.receiveFrame({ ...WELCOME, etag: null });

		await expect(pairing).resolves.toEqual({
			device: 'dev1',
			token: 'tok1',
			welcome: { serverId: 'S1', name: 'Desk', etag: null, hosts: ['a.local', '192.168.1.5'] },
		});
	});

	it('head resolves the etag', async () => {
		const { channel, client } = setup();

		const head = client.head();
		await flush();
		expect(channel.sentFrames()).toEqual([{ t: 'head' }]);
		channel.receiveFrame({ t: 'head', etag: null });

		await expect(head).resolves.toBeNull();
	});

	it('get resolves the assembled body', async () => {
		const { channel, client } = setup();

		const doc = client.get();
		await flush();
		expect(channel.sentFrames()).toEqual([{ t: 'get' }]);
		channel.receiveFrame({ t: 'doc', etag: 'e2', bytes: 3 });
		channel.receiveBinary([7, 8]);
		channel.receiveBinary([9]);

		await expect(doc).resolves.toEqual({ etag: 'e2', body: Uint8Array.from([7, 8, 9]) });
	});

	it('put sends the body and resolves stored', async () => {
		const { channel, client } = setup();

		const put = client.put('e1', Uint8Array.from([1, 2, 3]));
		await flush();
		expect(channel.sentFrames()).toEqual([{ t: 'put', ifEtag: 'e1', bytes: 3 }]);
		expect(channel.sent[1]).toEqual(Uint8Array.from([1, 2, 3]));
		channel.receiveFrame({ t: 'stored', etag: 'e2' });

		await expect(put).resolves.toEqual({ stored: true, etag: 'e2' });
	});

	it('put resolves a conflict', async () => {
		const { channel, client } = setup();

		const put = client.put('e1', Uint8Array.from([1]));
		await flush();
		channel.receiveFrame({ t: 'conflict', etag: 'cur' });

		await expect(put).resolves.toEqual({ stored: false, etag: 'cur' });
	});

	it('ifEtag null is sent as null', async () => {
		const { channel, client } = setup();

		void client.put(null, Uint8Array.from([1]));
		await flush();

		expect(channel.sentFrames()).toEqual([{ t: 'put', ifEtag: null, bytes: 1 }]);
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

		channel.receiveFrame({ t: 'head', etag: 'e1' });
		await expect(first).resolves.toBe('e1');
		await flush();
		expect(channel.sentFrames()).toEqual([{ t: 'head' }, { t: 'head' }]);

		channel.receiveFrame({ t: 'head', etag: 'e2' });
		await expect(second).resolves.toBe('e2');
	});

	it('a failed request does not hold up the next', async () => {
		const { channel, client } = setup();

		const first = client.head();
		const second = client.head();
		await flush();
		channel.receiveFrame({ t: 'error', code: 'internal', message: 'disk' });
		await expect(first).rejects.toMatchObject({ code: 'internal' });
		await flush();
		channel.receiveFrame({ t: 'head', etag: 'e2' });

		await expect(second).resolves.toBe('e2');
	});

	it('changed reaches the listener', () => {
		const { channel, client } = setup();
		const etags: string[] = [];
		const unsubscribe = client.onChanged((etag) => etags.push(etag));

		channel.receiveFrame({ t: 'changed', etag: 'e5' });
		unsubscribe();
		channel.receiveFrame({ t: 'changed', etag: 'e6' });

		expect(etags).toEqual(['e5']);
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
		channel.receiveFrame({ t: 'stored', etag: 'x' });

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
