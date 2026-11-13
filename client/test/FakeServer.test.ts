import { describe, expect, it } from 'vitest';
import { listAll, type ListPage } from '../src/ServerClient.js';
import { FakeClient, FakeServer } from '../src/testing/FakeServer.js';
import { makePairing, settle } from './fixtures.js';

/*
 * The fake holds transactions as the real server does (docs/protocol.md,
 * "Transactions"): per device and app, not per connection.
 */

const DATA = { path: '', id: 'data', type: 'json' };
const NOTE = { path: 'notes', id: 'n1', type: 'md' };
const TX = 'tx-0123456789abcdef';
const OTHER_TX = 'tx-fedcba9876543210';
const bytes = (text: string): Uint8Array => new TextEncoder().encode(text);

async function connect(server: FakeServer, device = 'dev1', token = 'tok1'): Promise<FakeClient> {
	const client = (await server.connect(makePairing())) as FakeClient;
	await client.hello(device, token);
	return client;
}

describe('FakeServer transactions', () => {
	it('a begin after a reconnect returns to the transaction, with what it staged', async () => {
		const server = new FakeServer();
		const note = server.storeDocument(NOTE, '# n1');
		const first = await connect(server);
		await first.begin(TX);
		await first.put(TX, DATA, null, bytes('{}'));
		await first.delete(TX, NOTE, note);
		first.drop();

		const second = await connect(server);

		await expect(second.begin(TX)).resolves.toEqual({
			tx: TX,
			staged: [
				{ ...DATA, ifEtag: null, bytes: 2, received: 2 },
				{ ...NOTE, delete: true, ifEtag: note, bytes: 0, received: 0 },
			],
		});
		await expect(second.commit(TX)).resolves.toMatchObject({ committed: true });
		expect(server.text()).toBe('{}');
		expect([...server.documents.keys()]).toEqual(['data.json']);
	});

	it('a begin with another id discards the device’s transaction, whichever session holds it', async () => {
		const server = new FakeServer();
		const phone = await connect(server);
		const tab = await connect(server);
		await phone.begin(TX);
		await phone.put(TX, DATA, null, bytes('{}'));

		await tab.begin(OTHER_TX);

		await expect(phone.put(TX, DATA, null, bytes('{}'))).rejects.toMatchObject({
			code: 'noTransaction',
		});
		await expect(phone.commit(TX)).rejects.toMatchObject({ code: 'noTransaction' });
		expect([...server.transactions.keys()]).toEqual([OTHER_TX]);
	});

	it('another device’s transaction is noTransaction, and an abort of it leaves it be', async () => {
		const server = new FakeServer();
		server.devices.set('dev2', 'tok2');
		const mine = await connect(server);
		const theirs = await connect(server, 'dev2', 'tok2');
		await theirs.begin(TX);

		await expect(mine.begin(TX)).rejects.toMatchObject({ code: 'noTransaction' });
		await expect(mine.put(TX, DATA, null, bytes('{}'))).rejects.toMatchObject({
			code: 'noTransaction',
		});
		await expect(mine.abort(TX)).resolves.toEqual({ tx: TX });
		expect(server.transactions.get(TX)?.device).toBe('dev2');
	});

	it('a conflict leaves the transaction open; a commit ends it', async () => {
		const server = new FakeServer();
		const client = await connect(server);
		await client.begin(TX);
		await client.put(TX, DATA, 'e-old', bytes('{}'));

		await expect(client.commit(TX)).resolves.toMatchObject({ committed: false });
		await client.put(TX, DATA, null, bytes('{}'), 2);
		await expect(client.commit(TX)).resolves.toMatchObject({ committed: true });
		expect(server.transactions.size).toBe(0);
	});

	it('a transaction id that breaks the rules is protocol, and the connection closes after it', async () => {
		const server = new FakeServer();
		const client = await connect(server);
		const closed: string[] = [];
		client.onClosed((error) => closed.push(error.code));

		await expect(client.begin('short')).rejects.toMatchObject({ code: 'protocol' });
		expect(closed).toEqual([]);
		await settle();

		expect(client.open).toBe(false);
		expect(closed).toEqual(['closed']);
	});

	it('a resume that does not fit what was staged is protocol', async () => {
		const server = new FakeServer();
		const client = await connect(server);
		await client.begin(TX);

		await expect(client.put(TX, DATA, null, bytes('{}'), 1)).rejects.toMatchObject({
			code: 'protocol',
		});
	});
});

describe('FakeServer listing', () => {
	it('pages with the cursors it gave, and refuses a forged one', async () => {
		const server = new FakeServer();
		server.pageSize = 1;
		server.store('{}');
		server.storeDocument(NOTE, '# n1');
		const client = await connect(server);

		const first = (await client.list()) as ListPage;
		expect(first.docs.map((doc) => doc.id)).toEqual(['data']);
		const second = await client.list({ revision: first.revision, cursor: first.cursor });
		expect((second as ListPage).docs.map((doc) => doc.id)).toEqual(['n1']);
		await expect(listAll(client)).resolves.toMatchObject({ revision: server.revision });

		await expect(client.list({ revision: first.revision, cursor: 'forged' })).rejects.toMatchObject(
			{ code: 'protocol' },
		);
	});

	it('a cursor without the revision of its page is protocol', async () => {
		const server = new FakeServer();
		server.pageSize = 1;
		server.store('{}');
		server.storeDocument(NOTE, '# n1');
		const client = await connect(server);
		const first = (await client.list()) as ListPage;

		await expect(client.list({ cursor: first.cursor })).rejects.toMatchObject({
			code: 'protocol',
		});
	});
});
