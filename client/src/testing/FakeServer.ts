import type { Body } from '../ChannelFraming.js';
import type { FallbackServer, ServerTarget } from '../PairingCode.js';
import type {
	Began,
	CommitResult,
	DocumentEtag,
	DocumentKey,
	GetOptions,
	ListPage,
	ReadDocument,
	Stale,
	StagedAck,
	Welcome,
} from '../ServerClient.js';
import { SYNC_DOCUMENT, type SyncClient } from '../SyncEngine.js';
import { WorkspaceError, type WorkspaceErrorCode } from '../WorkspaceError.js';

function bytesOf(body: Uint8Array | string): Uint8Array {
	return typeof body === 'string' ? new TextEncoder().encode(body) : body.slice();
}

async function bytesOfBody(body: Body): Promise<Uint8Array> {
	return body instanceof Uint8Array ? body.slice() : new Uint8Array(await body.arrayBuffer());
}

/** A 64-bit FNV-1a hash of `bytes`, as 16 hex digits: the fake's stand-in for SHA-256. */
function hash(bytes: Uint8Array): string {
	let h = 0xcbf29ce484222325n;
	for (const byte of bytes) {
		h ^= BigInt(byte);
		h = (h * 0x100000001b3n) & 0xffffffffffffffffn;
	}
	return h.toString(16).padStart(16, '0');
}

/** A key's canonical form, `path/id.type`, or `id.type` at the app's root. */
function keyString(key: DocumentKey): string {
	return key.path === '' ? `${key.id}.${key.type}` : `${key.path}/${key.id}.${key.type}`;
}

function keyOf(key: DocumentKey): DocumentKey {
	return { path: key.path, id: key.id, type: key.type };
}

/** A document a `FakeServer` holds. */
interface StoredDocument {
	key: DocumentKey;
	etag: string;
	body: Uint8Array;
}

/** A change a fake transaction holds: a body to write, or `null` to delete. */
interface FakeChange {
	key: DocumentKey;
	ifEtag: string | null;
	body: Uint8Array | null;
}

const SYNC_KEY = keyString(SYNC_DOCUMENT);

/**
 * One in-memory workspace-server for tests of an app's sync: pass `connect`
 * as the engine's `connect`. It keeps an app's set of opaque documents, as
 * the real one does, with etags and a revision that, as on the real one,
 * depend only on the bytes: two fake servers holding the same documents
 * report the same. `store`, `body`, `etag` and `text` are about the document
 * `SyncEngine` syncs (`data.json`). Switches let a test make it fail the way
 * a real one can. `log` records every call in order, across clients
 * (`hello#1`, `put#2`, …).
 *
 * Several engines may share one server, standing in for several devices.
 * Several servers stand in for a main and its subordinates: give each its
 * own `serverId` and `port`, list the subordinates' `entry()` in the main's
 * `subordinates`, let them share `devices` (the main's registry, which a
 * subordinate admits), and pass `FakeServer.route(...)` as `connect`.
 */
export class FakeServer {
	serverId = 'S1';
	name = 'Desk';
	hosts = ['a.local', '192.168.1.5'];
	/** How a device reaches this server: what `entry()` and `route` go by. */
	password = 'p'.repeat(24);
	fingerprint = Array.from({ length: 32 }, () => 'AB').join(':');
	port = 47801;
	/** The welcome's role; left out, as an older server does, when `undefined`. */
	role: string | undefined = undefined;
	/** The welcome's term; left out, as a family that never switched its main does, when `undefined`. */
	term: number | undefined = undefined;
	/** The welcome's list of subordinates; left out when `undefined`, as a subordinate does. */
	subordinates: FallbackServer[] | undefined = undefined;
	/** The app's documents, by `path/id.type`. */
	readonly documents = new Map<string, StoredDocument>();
	/** Paired devices: id to token. */
	devices = new Map<string, string>([['dev1', 'tok1']]);
	/** The most entries one list page carries; the real server's is 500. */
	pageSize = 500;

	/** Make `connect` fail with this code (e.g. `connectTimeout`). */
	connectError: WorkspaceErrorCode | null = null;
	/** Make `hello` fail with this code. */
	helloError: WorkspaceErrorCode | null = null;
	/** Make `pair` fail with this code. */
	pairError: WorkspaceErrorCode | null = null;
	/** Make the next `commit` fail with this code, after applying it when `commitAppliedFirst` is set. */
	commitError: WorkspaceErrorCode | null = null;
	/** Whether a `commitError` comes after the commit point, the write applied. */
	commitAppliedFirst = false;
	/** Answer this many next `commit`s with a conflict, as though another device wrote first. */
	conflicts = 0;
	/** While set, `connect` waits for it: a server slow to answer, or one away until it times out. */
	connectGate: Promise<void> | null = null;

	readonly clients: FakeClient[] = [];
	readonly log: string[] = [];
	/** Every `put`, committed or not, in order. */
	readonly puts: { key: DocumentKey; ifEtag: string | null; body: Uint8Array }[] = [];

	readonly connect = async (target: ServerTarget): Promise<SyncClient> => {
		void target;
		this.log.push('connect');
		if (this.connectGate) await this.connectGate;
		if (this.connectError) throw new WorkspaceError('Fake connect failure', this.connectError);
		const client = new FakeClient(this, this.clients.length + 1);
		this.clients.push(client);

		return client;
	};

	/** How a device reaches this server, as a main lists it among its subordinates. */
	entry(): FallbackServer {
		return {
			serverId: this.serverId,
			name: this.name,
			password: this.password,
			fingerprint: this.fingerprint,
			port: this.port,
			hosts: [...this.hosts],
		};
	}

	/**
	 * A `connect` that reaches whichever of `servers` the target names (by
	 * port and fingerprint); any other target times out.
	 */
	static route(...servers: FakeServer[]): (target: ServerTarget) => Promise<SyncClient> {
		return async (target) => {
			const server = servers.find(
				(s) => s.port === target.port && s.fingerprint === target.fingerprint,
			);
			if (!server) throw new WorkspaceError('No fake server there', 'connectTimeout');

			return server.connect(target);
		};
	}

	/** Stores `data.json` as though another device had written it; returns its etag. */
	store(body: Uint8Array | string): string {
		return this.storeDocument(SYNC_DOCUMENT, body);
	}

	/** Stores any document as though another device had written it; returns its etag. */
	storeDocument(key: DocumentKey, body: Uint8Array | string): string {
		const bytes = bytesOf(body);
		const etag = hash(bytes);
		this.documents.set(keyString(key), { key: keyOf(key), etag, body: bytes });
		return etag;
	}

	/** Removes a document as though another device had deleted it. */
	removeDocument(key: DocumentKey): void {
		this.documents.delete(keyString(key));
	}

	/** The etag of `data.json`, `null` when there is none. */
	get etag(): string | null {
		return this.documents.get(SYNC_KEY)?.etag ?? null;
	}

	/** The bytes of `data.json`, empty when there is none. */
	get body(): Uint8Array {
		return this.documents.get(SYNC_KEY)?.body.slice() ?? new Uint8Array(0);
	}

	/** `data.json` as text. */
	text(): string {
		return new TextDecoder().decode(this.body);
	}

	/**
	 * The set's revision: a hash of its keys and etags, `null` for an empty
	 * set, so the same documents give the same revision on every fake server.
	 */
	get revision(): string | null {
		if (this.documents.size === 0) return null;
		const lines = [...this.documents.keys()]
			.sort()
			.map((key) => `${key}\0${this.documents.get(key)?.etag ?? ''}\n`)
			.join('');
		return hash(new TextEncoder().encode(lines));
	}

	/** Every open client but `except` hears a `changed`, as after a commit. */
	pushChanged(except?: FakeClient): void {
		for (const client of this.clients) {
			if (client !== except && client.open) client.emitChanged(this.revision);
		}
	}
}

/** One device's connection to a `FakeServer`. Its open transaction lives with it. */
export class FakeClient implements SyncClient {
	open = true;
	private readonly changed = new Set<(revision: string | null) => void>();
	private readonly closed = new Set<(error: WorkspaceError) => void>();
	private tx: { id: string; changes: Map<string, FakeChange> } | null = null;

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
		return this.server.revision;
	}

	async list(
		options: { revision?: string | null; cursor?: string } = {},
	): Promise<ListPage | Stale> {
		this.call('list');
		const revision = this.server.revision;
		if ('revision' in options && options.revision !== revision) return { stale: revision };
		const keys = [...this.server.documents.keys()].sort();
		const from = options.cursor === undefined ? 0 : keys.indexOf(options.cursor) + 1;
		const pageKeys = keys.slice(from, from + this.server.pageSize);
		const page: ListPage = {
			revision,
			docs: pageKeys.map((key) => {
				const doc = this.server.documents.get(key) as StoredDocument;
				return { ...keyOf(doc.key), etag: doc.etag, bytes: doc.body.byteLength };
			}),
			removed: [],
		};
		if (from + pageKeys.length < keys.length) page.cursor = pageKeys[pageKeys.length - 1];

		return page;
	}

	async get(key: DocumentKey, options: GetOptions = {}): Promise<ReadDocument | Stale> {
		this.call('get');
		const doc = this.server.documents.get(keyString(key));
		const etag = options.etag ?? null;
		if (!doc || (etag !== null && doc.etag !== etag)) return { stale: this.server.revision };
		const offset = options.offset ?? 0;
		const body = doc.body.slice(offset);
		if (options.onPiece) {
			if (body.byteLength > 0) options.onPiece(body);
			return {
				...keyOf(key),
				etag: doc.etag,
				bytes: doc.body.byteLength,
				offset,
				body: new Uint8Array(0),
			};
		}

		return { ...keyOf(key), etag: doc.etag, bytes: doc.body.byteLength, offset, body };
	}

	async begin(tx: string): Promise<Began> {
		this.call('begin');
		if (this.tx?.id !== tx) this.tx = { id: tx, changes: new Map() };

		return {
			tx,
			staged: [...this.tx.changes.values()].map((change) => ({
				...keyOf(change.key),
				...(change.body === null ? { delete: true as const } : {}),
				ifEtag: change.ifEtag,
				bytes: change.body?.byteLength ?? 0,
				received: change.body?.byteLength ?? 0,
			})),
		};
	}

	async put(
		tx: string,
		key: DocumentKey,
		ifEtag: string | null,
		body: Body,
		offset = 0,
	): Promise<StagedAck> {
		void offset;
		this.call('put');
		const bytes = await bytesOfBody(body);
		this.server.puts.push({ key: keyOf(key), ifEtag, body: bytes.slice() });
		this.changes(tx).set(keyString(key), { key: keyOf(key), ifEtag, body: bytes });

		return { ...keyOf(key), received: bytes.byteLength };
	}

	async delete(tx: string, key: DocumentKey, ifEtag: string | null): Promise<StagedAck> {
		this.call('delete');
		this.changes(tx).set(keyString(key), { key: keyOf(key), ifEtag, body: null });

		return { ...keyOf(key), received: 0 };
	}

	async commit(tx: string): Promise<CommitResult> {
		this.call('commit');
		const changes = [...this.changes(tx).values()];
		const server = this.server;
		if (server.commitError && !server.commitAppliedFirst) {
			const code = server.commitError;
			server.commitError = null;
			throw new WorkspaceError('Fake commit failure', code);
		}
		if (server.conflicts > 0) {
			server.conflicts--;
			// Another device wrote meanwhile.
			return {
				committed: false,
				revision: server.revision,
				docs: changes.map((change) => this.current(change.key)),
			};
		}
		const conflicting = changes.filter((change) => change.ifEtag !== this.current(change.key).etag);
		if (conflicting.length > 0) {
			return {
				committed: false,
				revision: server.revision,
				docs: conflicting.map((change) => this.current(change.key)),
			};
		}

		const before = server.revision;
		for (const change of changes) {
			if (change.body === null) server.removeDocument(change.key);
			else server.storeDocument(change.key, change.body);
		}
		this.tx = null;
		if (server.revision !== before) server.pushChanged(this);
		if (server.commitError) {
			const code = server.commitError;
			server.commitError = null;
			throw new WorkspaceError('Fake commit failure after the commit point', code);
		}

		return {
			committed: true,
			revision: server.revision,
			docs: changes.map((change) => this.current(change.key)),
		};
	}

	async abort(tx: string): Promise<{ tx: string }> {
		this.call('abort');
		if (this.tx?.id === tx) this.tx = null;

		return { tx };
	}

	async forget(): Promise<void> {
		this.call('forget');
		this.server.devices.clear();
		this.open = false;
	}

	onChanged(listener: (revision: string | null) => void): () => void {
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

	emitChanged(revision: string | null): void {
		for (const listener of [...this.changed]) listener(revision);
	}

	/** The server side closes the channel: the connection is lost. */
	drop(): void {
		this.open = false;
		for (const listener of [...this.closed]) {
			listener(new WorkspaceError('The connection to the server closed', 'closed'));
		}
	}

	/** The changes of the open transaction `tx`; any other is gone. */
	private changes(tx: string): Map<string, FakeChange> {
		if (this.tx?.id !== tx) throw new WorkspaceError('No such transaction', 'noTransaction');
		return this.tx.changes;
	}

	private current(key: DocumentKey): DocumentEtag {
		return { ...keyOf(key), etag: this.server.documents.get(keyString(key))?.etag ?? null };
	}

	private welcome(): Welcome {
		const welcome: Welcome = {
			serverId: this.server.serverId,
			name: this.server.name,
			revision: this.server.revision,
			hosts: [...this.server.hosts],
		};
		if (this.server.role !== undefined) welcome.role = this.server.role;
		if (this.server.term !== undefined) welcome.term = this.server.term;
		if (this.server.subordinates) {
			welcome.subordinates = this.server.subordinates.map((s) => ({ ...s, hosts: [...s.hosts] }));
		}

		return welcome;
	}
}
