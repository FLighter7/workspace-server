import type { ServerTarget } from '../PairingCode.js';
import type { PutResult, Welcome } from '../ServerClient.js';
import type { SyncClient } from '../SyncEngine.js';
import { WorkspaceError, type WorkspaceErrorCode } from '../WorkspaceError.js';

function bytesOf(body: Uint8Array | string): Uint8Array {
	return typeof body === 'string' ? new TextEncoder().encode(body) : body.slice();
}

/**
 * One in-memory workspace-server for tests of an app's sync: pass `connect`
 * as the engine's `connect`. It keeps one opaque document, as the real one
 * does, and switches let a test make it fail the way a real one can. `log`
 * records every call in order, across clients (`hello#1`, `put#2`, …).
 *
 * Several engines may share one server, standing in for several devices.
 */
export class FakeServer {
	serverId = 'S1';
	name = 'Desk';
	hosts = ['a.local', '192.168.1.5'];
	etag: string | null = null;
	body: Uint8Array = new Uint8Array(0);
	/** Paired devices: id to token. */
	devices = new Map<string, string>([['dev1', 'tok1']]);

	/** Make `connect` fail with this code (e.g. `connectTimeout`). */
	connectError: WorkspaceErrorCode | null = null;
	/** Make `hello` fail with this code. */
	helloError: WorkspaceErrorCode | null = null;
	/** Make `pair` fail with this code. */
	pairError: WorkspaceErrorCode | null = null;
	/** Answer this many next `put`s with a conflict, as though another device wrote first. */
	conflicts = 0;

	readonly clients: FakeClient[] = [];
	readonly log: string[] = [];
	/** Every `put`, stored or not, in order. */
	readonly puts: { ifEtag: string | null; body: Uint8Array }[] = [];
	private etagCounter = 0;

	readonly connect = async (target: ServerTarget): Promise<SyncClient> => {
		void target;
		this.log.push('connect');
		if (this.connectError) throw new WorkspaceError('Fake connect failure', this.connectError);
		const client = new FakeClient(this, this.clients.length + 1);
		this.clients.push(client);

		return client;
	};

	/** Stores a document as though another device had written it; returns its etag. */
	store(body: Uint8Array | string): string {
		this.body = bytesOf(body);
		this.etag = this.nextEtag();
		return this.etag;
	}

	/** The stored document as text. */
	text(): string {
		return new TextDecoder().decode(this.body);
	}

	/** Every open client but `except` hears a `changed`, as after a stored put. */
	pushChanged(except?: FakeClient): void {
		for (const client of this.clients) {
			if (client !== except && client.open) client.emitChanged(this.etag ?? '');
		}
	}

	/** @internal */
	nextEtag(): string {
		return `e${++this.etagCounter}`;
	}
}

/** One device's connection to a `FakeServer`. */
export class FakeClient implements SyncClient {
	open = true;
	private readonly changed = new Set<(etag: string) => void>();
	private readonly closed = new Set<(error: WorkspaceError) => void>();

	constructor(
		private readonly server: FakeServer,
		readonly number: number,
	) {}

	private call(name: string): void {
		this.server.log.push(`${name}#${this.number}`);
		if (!this.open) throw new WorkspaceError('Closed', 'closed');
	}

	async hello(device: string, token: string): Promise<Welcome> {
		this.call('hello');
		if (this.server.helloError) throw new WorkspaceError('Fake hello', this.server.helloError);
		if (this.server.devices.get(device) !== token) {
			throw new WorkspaceError('Bad token', 'badToken');
		}

		return this.welcome();
	}

	async pair(
		secret: string,
		name: string,
	): Promise<{ device: string; token: string; welcome: Welcome }> {
		void secret;
		void name;
		this.call('pair');
		if (this.server.pairError) throw new WorkspaceError('Fake pair', this.server.pairError);
		const device = `dev${this.server.devices.size + 1}`;
		const token = `tok${this.server.devices.size + 1}`;
		this.server.devices.set(device, token);

		return { device, token, welcome: this.welcome() };
	}

	async head(): Promise<string | null> {
		this.call('head');
		return this.server.etag;
	}

	async get(): Promise<{ etag: string | null; body: Uint8Array }> {
		this.call('get');
		return { etag: this.server.etag, body: this.server.body.slice() };
	}

	async put(ifEtag: string | null, body: Uint8Array): Promise<PutResult> {
		this.call('put');
		this.server.puts.push({ ifEtag, body: body.slice() });
		if (this.server.conflicts > 0) {
			this.server.conflicts--;
			// Another device wrote meanwhile.
			this.server.etag = this.server.nextEtag();
			return { stored: false, etag: this.server.etag };
		}
		if (ifEtag !== this.server.etag) return { stored: false, etag: this.server.etag };

		this.server.body = body.slice();
		this.server.etag = this.server.nextEtag();
		this.server.pushChanged(this);

		return { stored: true, etag: this.server.etag };
	}

	async forget(): Promise<void> {
		this.call('forget');
		this.server.devices.clear();
		this.open = false;
	}

	onChanged(listener: (etag: string) => void): () => void {
		this.changed.add(listener);
		return () => this.changed.delete(listener);
	}

	onClosed(listener: (error: WorkspaceError) => void): () => void {
		this.closed.add(listener);
		return () => this.closed.delete(listener);
	}

	close(): void {
		if (!this.open) return;
		this.server.log.push(`close#${this.number}`);
		this.open = false;
	}

	emitChanged(etag: string): void {
		for (const listener of [...this.changed]) listener(etag);
	}

	/** The server side closes the channel: the connection is lost. */
	drop(): void {
		this.open = false;
		for (const listener of [...this.closed]) {
			listener(new WorkspaceError('The connection to the server closed', 'closed'));
		}
	}

	private welcome(): Welcome {
		return {
			serverId: this.server.serverId,
			name: this.server.name,
			etag: this.server.etag,
			hosts: [...this.server.hosts],
		};
	}
}
