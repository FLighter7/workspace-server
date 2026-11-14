import { bodySize, type Body } from '../ChannelFraming.js';
import type { FallbackServer, ServerTarget } from '../PairingCode.js';
import type {
	Began,
	CommitResult,
	DivergedServer,
	DocumentEtag,
	DocumentKey,
	GetOptions,
	Keep,
	ListPage,
	ReadDocument,
	Stale,
	StagedAck,
	Welcome,
} from '../ServerClient.js';
import { Sha256 } from '../sha256.js';
import { documentPath, type SyncClient } from '../SyncEngine.js';
import { WorkspaceError, type WorkspaceErrorCode } from '../WorkspaceError.js';

function bytesOf(body: Uint8Array | string): Uint8Array {
	return typeof body === 'string' ? new TextEncoder().encode(body) : body.slice();
}

async function bytesOfBody(body: Body): Promise<Uint8Array> {
	return body instanceof Uint8Array ? body.slice() : new Uint8Array(await body.arrayBuffer());
}

function sha256(bytes: Uint8Array): string {
	return new Sha256().update(bytes).hex();
}

function keyOf(key: DocumentKey): DocumentKey {
	return { path: key.path, id: key.id, type: key.type };
}

/** A document a `FakeServer` holds. */
export interface FakeDocument {
	key: DocumentKey;
	etag: string;
	body: Uint8Array;
}

/** A tombstone a `FakeServer` holds: a document deleted from the set, and when. */
export interface FakeTombstone {
	key: DocumentKey;
	at: string;
}

/**
 * A change a fake transaction holds: a body of `bytes` bytes of which `data`
 * has arrived, or a delete.
 */
export interface FakeChange {
	key: DocumentKey;
	ifEtag: string | null;
	delete: boolean;
	bytes: number;
	data: Uint8Array;
}

/** An open transaction of a `FakeServer`: whose it is, and what it staged, by canonical key. */
export interface FakeTransaction {
	device: string;
	changes: Map<string, FakeChange>;
}

/** A `put` as a `FakeServer` took it: the body's size, where it started, and the bytes that came. */
export interface FakePut {
	key: DocumentKey;
	ifEtag: string | null;
	bytes: number;
	offset: number;
	body: Uint8Array;
}

/** A transaction id as the server takes it: 16 to 64 of `[A-Za-z0-9_-]`. */
const TX_ID = /^[A-Za-z0-9_-]{16,64}$/;

/** How many tombstones the set keeps, as the real server does. */
const TOMBSTONES = 1000;

/**
 * One in-memory workspace-server for tests of an app's sync: pass `connect`
 * as the engine's `connect`. It keeps an app's set of opaque documents, as
 * the real one does: etags are the SHA-256 of the bytes, and the revision
 * is the hash of the set's keys and etags, so two fake servers holding the
 * same documents report the same, as real ones do. A listing comes in pages
 * of `pageSize`, tombstones after the documents; a `get` at an etag the
 * document no longer has is `stale`; transactions belong to a device, stage
 * bodies that resume from what arrived, and fail a commit on a conflict
 * (left open) or on a body that has not fully arrived (`incomplete`).
 *
 * Switches make it fail as a real one can, and `log` records every call in
 * order, across clients (`hello#1`, `put#2`, …). It also models a
 * divergence of the app's servers as a device sees it: `setDiverged` names
 * the servers that wait for a choice (in every welcome, and pushed to every
 * open client), and a client's `resolve` takes the first choice for each,
 * tells the others `resolved`, records it in `resolutions`, and calls
 * `onResolve`, where a test makes the set what the choice keeps. What
 * happens between the servers themselves is not modelled.
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
	/** The app's documents, by canonical key (`path/id.type`). */
	readonly documents = new Map<string, FakeDocument>();
	/** The set's tombstones, by canonical key, oldest first. */
	readonly removed = new Map<string, FakeTombstone>();
	/** Paired devices: id to token. */
	devices = new Map<string, string>([['dev1', 'tok1']]);
	/**
	 * The open transactions, by id. As on the real server they belong to a
	 * device, not to a connection: one per device (and app), which a `begin`
	 * after a reconnect returns to, and which a `begin` with another id,
	 * from any connection of the device, discards.
	 */
	readonly transactions = new Map<string, FakeTransaction>();
	/** The cursors list pages have given: any other is refused, as a forged one is. */
	readonly cursors = new Set<string>();
	/** The most entries one list page carries; the real server's is 500. */
	pageSize = 500;
	/** The divergences that wait for a device's choice; see `setDiverged`. */
	diverged: DivergedServer[] = [];
	/** Every choice taken, first for its server, in order. */
	readonly resolutions: { server: string; keep: Keep; device: string }[] = [];
	/** Called with each choice taken: a test makes the set what it keeps here. */
	onResolve: ((server: string, keep: Keep) => void) | null = null;
	/** When a tombstone is made: a test may fix the clock. */
	now: () => Date = () => new Date();

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
	/**
	 * Cut the connection once this many more bytes of `put` bodies have
	 * arrived: the body stays staged with what came, and the put fails with
	 * `closed`, as when a connection drops while the device sends.
	 */
	cutPutAfter: number | null = null;
	/** The same for `get` bodies: the bytes before the cut reach `onPiece`, then the get fails. */
	cutGetAfter: number | null = null;
	/**
	 * Lose this many next `put` frames: the device sent the frame and its
	 * body, the server never saw them, and the connection drops. What the
	 * transaction staged for the key before stays as it was.
	 */
	losePuts = 0;
	/**
	 * The app's quota in bytes, `null` for none. As on the real server, a
	 * `put` with which the set after the transaction would pass it is refused
	 * with `quota` once its whole body has arrived, and so is a `commit`.
	 */
	quota: number | null = null;
	/** While set, `connect` waits for it: a server slow to answer, or one away until it times out. */
	connectGate: Promise<void> | null = null;

	readonly clients: FakeClient[] = [];
	readonly log: string[] = [];
	/** Every `put`, committed or not, in order. */
	readonly puts: FakePut[] = [];

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

	/** Stores a document as though another device had written it; returns its etag. */
	storeDocument(key: DocumentKey, body: Uint8Array | string): string {
		const bytes = bytesOf(body);
		const etag = sha256(bytes);
		const path = documentPath(key);
		this.documents.set(path, { key: keyOf(key), etag, body: bytes });
		this.removed.delete(path);
		return etag;
	}

	/** Removes a document as though another device had deleted it, leaving its tombstone. */
	removeDocument(key: DocumentKey): void {
		const path = documentPath(key);
		if (!this.documents.delete(path)) return;
		this.removed.delete(path);
		this.removed.set(path, { key: keyOf(key), at: this.now().toISOString() });
		for (const oldest of this.removed.keys()) {
			if (this.removed.size <= TOMBSTONES) break;
			this.removed.delete(oldest);
		}
	}

	/** The bytes of a document, `null` when there is none. */
	document(key: DocumentKey): Uint8Array | null {
		return this.documents.get(documentPath(key))?.body.slice() ?? null;
	}

	/** A document as text, `null` when there is none. */
	text(key: DocumentKey): string | null {
		const body = this.document(key);
		return body === null ? null : new TextDecoder().decode(body);
	}

	/** The etag of a document, `null` when there is none. */
	etag(key: DocumentKey): string | null {
		return this.documents.get(documentPath(key))?.etag ?? null;
	}

	/**
	 * The set's revision: the SHA-256 of `<key>\0<etag>\n` lines sorted by
	 * key, `null` for an empty set, as the real server makes it.
	 */
	get revision(): string | null {
		if (this.documents.size === 0) return null;
		const lines = [...this.documents.keys()]
			.sort()
			.map((path) => `${path}\0${this.documents.get(path)?.etag ?? ''}\n`)
			.join('');
		return sha256(new TextEncoder().encode(lines));
	}

	/** Every open client but `except` hears a `changed`, as after a commit. */
	pushChanged(except?: FakeClient): void {
		for (const client of this.clients) {
			if (client !== except && client.open) client.emitChanged(this.revision);
		}
	}

	/** Names the divergences that wait for a choice; every open client hears `diverged`. */
	setDiverged(servers: DivergedServer[]): void {
		this.diverged = servers.map((server) => ({ ...server }));
		for (const client of this.clients) {
			if (client.open) client.emitDiverged(this.diverged);
		}
	}
}

/**
 * One device's connection to a `FakeServer`. Its transactions are the
 * device's, kept by the server. A frame the real server answers with
 * `protocol` (a transaction id that breaks the rules, a forged cursor, a
 * resume that does not fit) rejects with `protocol`, and the connection then
 * closes, as the real one does.
 */
export class FakeClient implements SyncClient {
	open = true;
	/** The device this connection authenticated as; `null` before. */
	device: string | null = null;
	private readonly changed = new Set<(revision: string | null) => void>();
	private readonly divergedListeners = new Set<(servers: DivergedServer[]) => void>();
	private readonly resolvedListeners = new Set<(server: string) => void>();
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
		this.device = device;

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
		this.device = device;

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
		const { cursor } = options;
		if (cursor !== undefined && (!('revision' in options) || !this.server.cursors.has(cursor))) {
			this.refuse('A cursor the server never gave, or one without the revision of its page');
		}
		const revision = this.server.revision;
		if ('revision' in options && options.revision !== revision) return { stale: revision };
		const docs = [...this.server.documents.keys()].sort();
		const removed = [...this.server.removed.keys()].sort();
		const from = cursor === undefined ? 0 : Number(cursor.slice(cursor.indexOf('#') + 1));
		const to = from + this.server.pageSize;
		const page: ListPage = {
			revision,
			docs: docs.slice(from, to).map((path) => {
				const doc = this.server.documents.get(path) as FakeDocument;
				return { ...keyOf(doc.key), etag: doc.etag, bytes: doc.body.byteLength };
			}),
			removed: removed
				.slice(Math.max(0, from - docs.length), Math.max(0, to - docs.length))
				.map((path) => {
					const tombstone = this.server.removed.get(path) as FakeTombstone;
					return { ...keyOf(tombstone.key), at: tombstone.at };
				}),
		};
		if (to < docs.length + removed.length) {
			page.cursor = `${revision ?? 'empty'}#${to}`;
			this.server.cursors.add(page.cursor);
		}

		return page;
	}

	async get(key: DocumentKey, options: GetOptions = {}): Promise<ReadDocument | Stale> {
		this.call('get');
		const doc = this.server.documents.get(documentPath(key));
		const etag = options.etag ?? null;
		const offset = options.offset ?? 0;
		if (offset > 0 && (etag === null || (doc && offset > doc.body.byteLength))) {
			this.refuse('An offset without an etag, or past the document');
		}
		if (!doc || (etag !== null && doc.etag !== etag)) return { stale: this.server.revision };
		let body = doc.body.slice(offset);
		const cut = this.server.cutGetAfter;
		if (cut !== null && cut < body.byteLength) {
			this.server.cutGetAfter = null;
			body = body.slice(0, cut);
			if (options.onPiece && body.byteLength > 0) options.onPiece(body);
			this.drop();
			throw new WorkspaceError('The connection to the server closed', 'closed');
		}
		if (cut !== null) this.server.cutGetAfter = cut - body.byteLength;
		if (options.onPiece) {
			if (body.byteLength > 0) options.onPiece(body);
			body = new Uint8Array(0);
		}

		return { ...keyOf(key), etag: doc.etag, bytes: doc.body.byteLength, offset, body };
	}

	async begin(tx: string): Promise<Began> {
		this.call('begin');
		this.checkTx(tx);
		const device = this.device ?? '';
		const transactions = this.server.transactions;
		let open = transactions.get(tx);
		if (open && open.device !== device) {
			throw new WorkspaceError('Another device holds that transaction', 'noTransaction');
		}
		if (!open) {
			for (const [id, other] of transactions) {
				if (other.device === device) transactions.delete(id);
			}
			open = { device, changes: new Map() };
			transactions.set(tx, open);
		}

		return {
			tx,
			staged: [...open.changes.values()].map((change) => ({
				...keyOf(change.key),
				...(change.delete ? { delete: true as const } : {}),
				ifEtag: change.ifEtag,
				bytes: change.bytes,
				received: change.data.byteLength,
			})),
		};
	}

	async put(
		tx: string,
		key: DocumentKey,
		ifEtag: string | null,
		body: Body,
		offset = 0,
		onProgress?: (position: number) => void,
	): Promise<StagedAck> {
		this.call('put');
		this.checkTx(tx);
		const size = bodySize(body);
		const changes = this.changes(tx);
		if (this.server.losePuts > 0) {
			this.server.losePuts--;
			this.drop();
			throw new WorkspaceError('The connection to the server closed', 'closed');
		}
		const path = documentPath(key);
		const staged = changes.get(path);
		if (
			offset > 0 &&
			(!staged || staged.delete || staged.bytes !== size || offset > staged.data.byteLength)
		) {
			this.refuse('A resumed body that does not fit what the transaction staged');
		}
		const bytes = await bytesOfBody(body);
		let sent = bytes.slice(offset);
		const cut = this.server.cutPutAfter;
		const cutting = cut !== null && cut < sent.byteLength;
		if (cutting) {
			this.server.cutPutAfter = null;
			sent = sent.slice(0, cut);
		} else if (cut !== null) {
			this.server.cutPutAfter = cut - sent.byteLength;
		}
		const data = new Uint8Array(offset + sent.byteLength);
		if (offset > 0 && staged) data.set(staged.data.subarray(0, offset));
		data.set(sent, offset);
		this.server.puts.push({ key: keyOf(key), ifEtag, bytes: size, offset, body: sent.slice() });
		const change: FakeChange = { key: keyOf(key), ifEtag, delete: false, bytes: size, data };
		if (!cutting && this.overQuota(changes, change)) {
			throw new WorkspaceError('The set would pass the app’s quota', 'quota');
		}
		changes.set(path, change);
		onProgress?.(offset + sent.byteLength);
		if (cutting) {
			this.drop();
			throw new WorkspaceError('The connection to the server closed', 'closed');
		}

		return { ...keyOf(key), received: data.byteLength };
	}

	async delete(tx: string, key: DocumentKey, ifEtag: string | null): Promise<StagedAck> {
		this.call('delete');
		this.checkTx(tx);
		this.changes(tx).set(documentPath(key), {
			key: keyOf(key),
			ifEtag,
			delete: true,
			bytes: 0,
			data: new Uint8Array(0),
		});

		return { ...keyOf(key), received: 0 };
	}

	async commit(tx: string): Promise<CommitResult> {
		this.call('commit');
		this.checkTx(tx);
		const changes = [...this.changes(tx).values()];
		const server = this.server;
		if (changes.some((change) => !change.delete && change.data.byteLength < change.bytes)) {
			throw new WorkspaceError('A body has not fully arrived', 'incomplete');
		}
		if (this.overQuota(this.changes(tx))) {
			throw new WorkspaceError('The set would pass the app’s quota', 'quota');
		}
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
			if (change.delete) server.removeDocument(change.key);
			else server.storeDocument(change.key, change.data);
		}
		server.transactions.delete(tx);
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
		this.checkTx(tx);
		// Answered even when the transaction is gone, or another device's, as
		// the real server does; only the device's own is discarded.
		if (this.server.transactions.get(tx)?.device === (this.device ?? '')) {
			this.server.transactions.delete(tx);
		}

		return { tx };
	}

	async resolve(serverId: string, keep: Keep): Promise<{ server: string }> {
		this.call('resolve');
		const server = this.server;
		const waiting = server.diverged.some((entry) => entry.serverId === serverId);
		if (waiting) {
			// The first choice wins; any later one changes nothing and is answered all the same.
			server.diverged = server.diverged.filter((entry) => entry.serverId !== serverId);
			server.resolutions.push({ server: serverId, keep, device: this.device ?? '' });
			for (const client of server.clients) {
				if (client === this || !client.open) continue;
				client.emitResolved(serverId);
				client.emitDiverged(server.diverged);
			}
			server.onResolve?.(serverId, keep);
		}
		this.emitResolved(serverId);

		return { server: serverId };
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

	onDiverged(listener: (servers: DivergedServer[]) => void): () => void {
		this.divergedListeners.add(listener);
		return () => this.divergedListeners.delete(listener);
	}

	onResolved(listener: (server: string) => void): () => void {
		this.resolvedListeners.add(listener);
		return () => this.resolvedListeners.delete(listener);
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

	emitDiverged(servers: DivergedServer[]): void {
		for (const listener of [...this.divergedListeners]) {
			listener(servers.map((server) => ({ ...server })));
		}
	}

	emitResolved(server: string): void {
		for (const listener of [...this.resolvedListeners]) listener(server);
	}

	/** The server side closes the channel: the connection is lost. */
	drop(): void {
		this.open = false;
		for (const listener of [...this.closed]) {
			listener(new WorkspaceError('The connection to the server closed', 'closed'));
		}
	}

	/** The changes of this device's open transaction `tx`; any other is gone. */
	private changes(tx: string): Map<string, FakeChange> {
		const open = this.server.transactions.get(tx);
		if (!open || open.device !== (this.device ?? '')) {
			throw new WorkspaceError('No such transaction', 'noTransaction');
		}
		return open.changes;
	}

	/** Refuses a transaction id the real server would not take. */
	private checkTx(tx: string): void {
		if (!TX_ID.test(tx)) this.refuse(`An invalid transaction id ${JSON.stringify(tx)}`);
	}

	/**
	 * Answers `protocol` and closes the connection after it, as the real
	 * server does: the rejection is seen before the close.
	 */
	private refuse(message: string): never {
		setTimeout(() => {
			if (this.open) this.drop();
		}, 0);
		throw new WorkspaceError(message, 'protocol');
	}

	/**
	 * Whether the set after the transaction's `changes` (and `extra`) would
	 * pass the quota while being larger than now: a set over its quota can
	 * always shrink.
	 */
	private overQuota(changes: Map<string, FakeChange>, extra?: FakeChange): boolean {
		const quota = this.server.quota;
		if (quota === null) return false;
		const sizes = new Map(
			[...this.server.documents].map(([path, doc]) => [path, doc.body.byteLength]),
		);
		const now = [...sizes.values()].reduce((sum, size) => sum + size, 0);
		const all = new Map(changes);
		if (extra) all.set(documentPath(extra.key), extra);
		for (const [path, change] of all) {
			if (change.delete) sizes.delete(path);
			else sizes.set(path, change.bytes);
		}
		const after = [...sizes.values()].reduce((sum, size) => sum + size, 0);
		return after > quota && after > now;
	}

	private current(key: DocumentKey): DocumentEtag {
		return { ...keyOf(key), etag: this.server.etag(key) };
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
		if (this.server.diverged.length > 0) {
			welcome.diverged = this.server.diverged.map((server) => ({ ...server }));
		}

		return welcome;
	}
}
