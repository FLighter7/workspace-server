import { bodySize, type Body } from './ChannelFraming.js';
import { browserDeviceName } from './deviceName.js';
import { openServerChannel } from './openServerChannel.js';
import type { FallbackServer, PairingCode, ServerTarget } from './PairingCode.js';
import type { Pairing, PairingStore, StagedBody } from './PairingStore.js';
import {
	ServerClient,
	isStale,
	listAll,
	type CommitResult,
	type DivergedServer,
	type DocumentEtag,
	type DocumentInfo,
	type DocumentKey,
	type Keep,
	type Listing,
	type StagedChange,
	type Welcome,
} from './ServerClient.js';
import { Sha256, etagOf } from './sha256.js';
import { DocumentError, WorkspaceError, type WorkspaceErrorCode } from './WorkspaceError.js';

export type SyncStatus =
	| 'unpaired'
	| 'connecting'
	| 'syncing'
	| 'connected'
	| 'unreachable'
	| 'paused'
	| 'interrupted'
	| 'error';

/**
 * A transfer under way, or a large one cut off: which way the bytes go
 * (`up`: the device writes; `down`: it reads), how many of them have gone,
 * of how many. `interrupted` is set when a transfer of more than
 * `SMALL_TRANSFER_BYTES` was cut off: sync then waits for `resumeTransfer()`
 * or `restartTransfer()`. A smaller one starts over by itself.
 */
export interface Transfer {
	direction: 'up' | 'down';
	done: number;
	total: number;
	interrupted: boolean;
}

/**
 * A divergence of the app's servers that waits for a choice: the copy of
 * the subordinate `serverId` (`name`) and its main's (`mainName`) both
 * changed apart, `since` then (RFC 3339 UTC, kept as received: it names the
 * divergence). Ask with both names, as `resolveDivergence` keeps one copy:
 * `'main'`, `mainName`'s, or `'subordinate'`, `name`'s.
 */
export interface Divergence extends DivergedServer {
	mainName: string;
}

/** What an app shows about sync. `DocCode` is the app's own `DocumentError` codes. */
export interface SyncState<DocCode extends string = never> {
	status: SyncStatus;
	/** The name of the server this device paired with. */
	serverName: string | null;
	/**
	 * The name of the server the open connection goes to when it is not the
	 * paired one but one of its fallbacks (a subordinate standing in for its
	 * main); `null` otherwise.
	 */
	via: string | null;
	lastSyncAt: string | null;
	error: WorkspaceError<WorkspaceErrorCode | DocCode> | null;
	/** The transfer under way or cut off; `null` when there is none. */
	transfer: Transfer | null;
	/**
	 * The divergences the server last named, `[]` when none waits. Kept while
	 * the connection is down; the next server reached replaces them.
	 */
	diverged: Divergence[];
}

/** The part of `ServerClient` the engine uses, so tests can stand in a fake server. */
export type SyncClient = Pick<
	ServerClient,
	| 'hello'
	| 'pair'
	| 'head'
	| 'list'
	| 'get'
	| 'begin'
	| 'put'
	| 'delete'
	| 'commit'
	| 'abort'
	| 'resolve'
	| 'forget'
	| 'onChanged'
	| 'onDiverged'
	| 'onResolved'
	| 'onClosed'
	| 'close'
>;

/** A document the engine read for the adapter: what the listing named, and its bytes. */
export interface DocumentBody extends DocumentInfo {
	body: Uint8Array;
}

/** A document to write: its key and its new bytes, in memory or a `Blob` read as it goes. */
export interface PlannedPut {
	doc: DocumentKey;
	body: Body;
}

/** What one sync does, as the app's adapter decides. */
export interface SyncPlan {
	/** Brings the local state up to the merge; leave it out when the local state already is. */
	apply?: () => Promise<void>;
	/**
	 * The documents to write. A body the server already holds (the same
	 * etag) is left out, so a plan may name every document it has.
	 */
	put?: PlannedPut[];
	/** The documents to delete; one the listing does not hold is left out. */
	delete?: DocumentKey[];
}

/**
 * Everything the engine needs to know about an app's documents. The engine
 * decides when to sync and talks to the server; the adapter decides what to
 * read and what the merge is.
 *
 * Both methods run synchronously: `select` on a fresh listing, and `plan`
 * once what it selected has been read, right after the engine reads
 * `changeCount`. The listing names each document's etag (the SHA-256 of its
 * bytes) and size, and its tombstones; an app that keeps the etag of each
 * document it holds selects only those that changed.
 */
export interface SyncAdapter {
	/**
	 * A counter that moves on every local change. A device whose set the
	 * server already holds (the revision of its last sync), and whose count
	 * has not moved since, skips listing again.
	 */
	changeCount(): number;
	/** The documents of the listing to read for the merge; keys it does not list are left out. */
	select(listing: Listing): DocumentKey[];
	/**
	 * Merges what was read with the local state. `read` holds each document
	 * `select` named, in its order, at the etag of `listing`.
	 *
	 * Read the local state here, not in `apply`, so an edit made after the
	 * merge moves the count past what was synced and the next sync picks it
	 * up. Every write is conditional on the etag of the listing: if another
	 * device wrote first, the engine lists, reads and plans again.
	 *
	 * Throw a `DocumentError` for a document this app cannot read (a newer
	 * format, a damaged one): sync halts, and nothing is written over it.
	 * Two devices settle only if both merge the same way, so on a tie the
	 * server's copy should win: every device then agrees without writing back
	 * and forth. And write only when the server lacks something: a write that
	 * local housekeeping alone causes (pruning, garbage collection) lets two
	 * devices with different settings undo each other's writes forever.
	 */
	plan(listing: Listing, read: DocumentBody[], now: Date): SyncPlan;
	/**
	 * Told, once the server holds them, the documents the plan put, each
	 * with its etag, and those it deleted, with `null`: what the commit
	 * wrote, and what the server held as planned already. An app that keeps
	 * the etag of each document it holds remembers these, so its next
	 * `select` does not read back what it wrote itself. A body the commit
	 * names with another etag than the one sent is left out. Optional.
	 */
	committed?(docs: DocumentEtag[]): void;
}

export interface SyncTimers {
	set(callback: () => void, ms: number): unknown;
	clear(handle: unknown): void;
}

export interface SyncEngineOptions<DocCode extends string = never> {
	/** This app's id on the server (`[a-z0-9-]{1,64}`). */
	app: string;
	/** Where this device keeps its pairing; see `createPairingStore`. */
	pairingStore: PairingStore;
	adapter: SyncAdapter;
	/** Told every change of state: what an app shows about sync. */
	onState: (state: SyncState<DocCode>) => void;
	/** The name this device gives itself when it pairs. Defaults to `browserDeviceName`. */
	deviceName?: () => string;
	/** Opens a client to the server. Defaults to `connectToServer(app)`: WebRTC, no signaling. */
	connect?: (target: ServerTarget) => Promise<SyncClient>;
	now?: () => Date;
	timers?: SyncTimers;
}

/** Waits between attempts while the server is unreachable; the last one repeats. */
export const RETRY_DELAYS_MS = [2000, 5000, 15000, 30000, 60000, 300000];
/**
 * Waits between attempts while the server blocks this device's address. A
 * block lasts up to 10 minutes and attempts during it do not extend it, so
 * one a minute finds its end soon without knocking often.
 */
export const BLOCKED_RETRY_MS = 60_000;
/** How long after the last local edit a sync starts. */
export const CHANGE_DEBOUNCE_MS = 3000;
/** A last line of defence against a write loop: past this many commits, writing stops until the app reopens. */
export const MAX_WRITES_PER_MINUTE = 10;
/**
 * How many times one sync lists, reads and plans again after another device
 * wrote first, a document changed between the listing and the read, or a
 * write failed in a way a fresh listing settles.
 */
export const MAX_CONFLICT_RETRIES = 3;
/**
 * How long, from the start of a connection, a pairing with fallback servers
 * waits for its main once another server has answered. A connection opens in
 * well under a second; a server that is away holds an attempt until it times
 * out, which is why no attempt waits for another.
 */
export const MAIN_GRACE_MS = 2000;
/**
 * The largest transfer that starts over by itself when it is cut off; a
 * larger one waits for `resumeTransfer()` or `restartTransfer()`. A
 * transfer is one sync's reads, or one transaction's bodies, together.
 */
export const SMALL_TRANSFER_BYTES = 1024 * 1024;

const WRITE_WINDOW_MS = 60_000;

/** A key's canonical form: `path/id.type`, or `id.type` at the app's root, as the server keeps it. */
export function documentPath(key: DocumentKey): string {
	return key.path === '' ? `${key.id}.${key.type}` : `${key.path}/${key.id}.${key.type}`;
}

function keyOf(key: DocumentKey): DocumentKey {
	return { path: key.path, id: key.id, type: key.type };
}

/**
 * The revision of a set holding these documents (canonical key to etag), as
 * the server names it: the SHA-256 of `<key>\0<etag>\n` lines sorted by
 * key, `null` for an empty set.
 */
function revisionOf(etags: Map<string, string>): string | null {
	if (etags.size === 0) return null;
	const hash = new Sha256();
	const encoder = new TextEncoder();
	for (const path of [...etags.keys()].sort()) {
		hash.update(encoder.encode(`${path}\0${etags.get(path)}\n`));
	}
	return hash.hex();
}

/** A new transaction id: 22 characters of base64url, from 16 random bytes. */
function newTransactionId(): string {
	const bytes = crypto.getRandomValues(new Uint8Array(16));
	let binary = '';
	for (const byte of bytes) binary += String.fromCharCode(byte);

	return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

/** The pieces of a body as one array of `bytes` bytes. */
function joined(chunks: Uint8Array[], bytes: number): Uint8Array {
	if (chunks.length === 1 && chunks[0].byteLength === bytes) return chunks[0];
	const body = new Uint8Array(bytes);
	let at = 0;
	for (const chunk of chunks) {
		body.set(chunk, at);
		at += chunk.byteLength;
	}
	return body;
}

/**
 * The failures of a write after which the sync lists and plans again: the
 * transaction went (another session of this device began one), a body did
 * not fully arrive, or the server failed on its side, possibly after its
 * commit point, so the write may have landed; the next listing says which,
 * and the next `begin` of the same transaction whether it is still open.
 */
const RETRY_WRITE: ReadonlySet<string> = new Set<WorkspaceErrorCode>([
	'noTransaction',
	'internal',
	'incomplete',
]);

/** The failures of a write after which its transaction is of no more use: the next write begins another. */
const DISCARD_TX: ReadonlySet<string> = new Set<WorkspaceErrorCode>([
	'noTransaction',
	'quota',
	'badPath',
	'badToken',
	'unknownApp',
	'protocol',
]);

/**
 * Errors after which retrying cannot help: sync stops until the device pairs
 * again or the app starts again, rather than knocking on the server forever.
 * A `DocumentError` halts too.
 */
const HALTING: ReadonlySet<string> = new Set<WorkspaceErrorCode>([
	'serverChanged',
	'badToken',
	'unknownApp',
	'protocol',
	'tooManyWrites',
]);

/** The connection itself failed: the server is away, so try again later. */
const CONNECTION_LOST: ReadonlySet<string> = new Set<WorkspaceErrorCode>([
	'connectTimeout',
	'closed',
]);

/**
 * The server refused for now: it blocks this address for a while, or failed
 * on its side (`internal`, which an unknown code is read as too). The pairing
 * stands; the connection is dropped and tried again on its own.
 */
const TEMPORARY: ReadonlySet<string> = new Set<WorkspaceErrorCode>(['blocked', 'internal']);

function asWorkspaceError(error: unknown): WorkspaceError<string> {
	if (error instanceof WorkspaceError) return error;

	return new WorkspaceError(error instanceof Error ? error.message : String(error), 'internal');
}

/**
 * The default `connect`: a WebRTC data channel to the server, with no
 * signaling, and a `ServerClient` for `app` over it.
 */
export function connectToServer(app: string): (target: ServerTarget) => Promise<SyncClient> {
	return async (target) => {
		const { channel, close } = await openServerChannel(target);
		const client = new ServerClient(channel, { app });
		client.onClosed(() => close());

		return client;
	};
}

const browserTimers: SyncTimers = {
	set: (callback, ms) => setTimeout(callback, ms),
	clear: (handle) => clearTimeout(handle as number),
};

/** A change one transaction stages: a body to put, with its size and etag, or a delete. */
interface Change {
	key: DocumentKey;
	path: string;
	/** The etag the listing named, which the document must still have at the commit. */
	ifEtag: string | null;
	put: { body: Body; bytes: number; etag: string } | null;
}

/** A body read in part or whole, kept across attempts and cut-off connections in this run. */
interface PartialRead {
	etag: string;
	chunks: Uint8Array[];
	received: number;
}

/** The transaction this device writes in while it is open on the server, and the bodies it staged. */
interface OpenTransaction {
	id: string;
	/** The server it is open on: another holds nothing of it, or something else under its id. */
	server: string;
	/**
	 * By canonical key: the body the server stages for the key, in part or
	 * whole, which a later write of the same bytes resumes. A key is named
	 * here with a body only once the server holds that body or nothing for
	 * it, never while it may still hold another body's bytes.
	 */
	bodies: Map<string, StagedBody>;
	/** Whether its bodies together are larger than `SMALL_TRANSFER_BYTES`: it is then saved to resume. */
	persist: boolean;
	total: number;
}

/** What `changesOf` makes of a plan: what to write, and what the server holds as planned already. */
interface Changes {
	changes: Change[];
	/** Puts the server holds with the same bytes, and deletes of what it does not hold. */
	held: DocumentEtag[];
}

/** A plan the server refused (`quota`, `badPath`), not sent again while nothing changes. */
interface Refused {
	signature: string;
	revision: string | null;
	error: WorkspaceError<string>;
}

/** A server that answered `hello`: its client, its welcome, and which one it is. */
interface Reached {
	client: SyncClient;
	welcome: Welcome;
	server: FallbackServer;
}

/**
 * The fallbacks a welcome teaches: the subordinates a main lists, without
 * the paired server, and keeping the answering server itself when it is
 * not the paired one (a main a device reached as a fallback lists its
 * subordinates, not itself).
 */
function fallbacksFrom(
	pairing: Pairing,
	answering: string,
	subordinates: FallbackServer[],
): FallbackServer[] {
	const own = (pairing.fallbacks ?? []).filter(
		(server) => server.serverId === answering && answering !== pairing.serverId,
	);
	const seen = new Set<string>([pairing.serverId]);

	return [...own, ...subordinates].filter((server) => {
		if (seen.has(server.serverId)) return false;
		seen.add(server.serverId);
		return true;
	});
}

/**
 * `pairing` moved over to the main the device reached as a fallback: the
 * main of the family is the server the device pairs with, so that only its
 * refusals stop sync and the device follows the family through every switch
 * of its main. The server it paired with before becomes a fallback. The
 * device id, the token and `lastRevision` stay: every server of the family
 * admits the same device, and a revision is the hash of the set's keys and
 * etags. An upload kept to resume stays too: on a server that does not hold
 * its transaction, `begin` opens it empty, and its bodies go from the start.
 */
function movedTo(pairing: Pairing, main: FallbackServer, welcome: Welcome): Pairing {
	const before: FallbackServer = {
		serverId: pairing.serverId,
		name: pairing.name,
		password: pairing.password,
		fingerprint: pairing.fingerprint,
		port: pairing.port,
		hosts: [...pairing.hosts],
	};
	const next: Pairing = {
		...pairing,
		serverId: main.serverId,
		name: welcome.name,
		password: main.password,
		fingerprint: main.fingerprint,
		port: main.port,
		hosts: [...welcome.hosts],
	};
	if (welcome.term) next.term = welcome.term;
	else delete next.term;
	const others = (pairing.fallbacks ?? []).filter((server) => server.serverId !== main.serverId);

	return withFallbacks(next, [before, ...others]);
}

/** `pairing` with `fallbacks` as given; an empty list leaves the field out, as a record from before had it. */
function withFallbacks(pairing: Pairing, fallbacks: FallbackServer[]): Pairing {
	const next = { ...pairing };
	if (fallbacks.length > 0) next.fallbacks = fallbacks;
	else delete next.fallbacks;

	return next;
}

/**
 * Runs server sync for one app run: when to connect, when to sync, when to
 * write, and what to do when something fails. Stateful, so not a singleton:
 * an app creates one.
 *
 * The rules: the channel is open only while the app is on screen; a
 * `changed` push syncs at once and a local edit after a short pause; a
 * failed connection retries on a growing delay while the app is visible,
 * and so does a temporary refusal (a blocked address, once a minute); there
 * is only ever one connection, and closing it invalidates whatever was
 * still in flight on it.
 *
 * A sync asks for the set's revision; when it moved, or something changed
 * here, it lists the set page by page, reads what the adapter selects at the
 * etags the listing names, has the adapter plan, and writes the plan in one
 * transaction conditional on those etags. A read that finds a document
 * changed (`stale`), a conflict at the commit, and a failure that may have
 * come after the commit point all list and plan again; a conflict keeps the
 * transaction, so bodies that are still wanted are not sent again.
 *
 * A transfer cut off with the connection resumes or starts over: one of
 * `SMALL_TRANSFER_BYTES` or less starts over by itself, a larger one waits
 * for the app's `resumeTransfer()` or `restartTransfer()`. A large upload is
 * saved in the pairing record, so it resumes after the app reloads too; a
 * read is kept in memory only.
 */
export class SyncEngine<DocCode extends string = never> {
	private readonly store: PairingStore;
	private readonly adapter: SyncAdapter;
	private readonly onState: (state: SyncState<DocCode>) => void;
	private readonly deviceName: () => string;
	private readonly connectTo: (target: ServerTarget) => Promise<SyncClient>;
	private readonly now: () => Date;
	private readonly timers: SyncTimers;

	private pairing: Pairing | null = null;
	private client: SyncClient | null = null;
	/** The name of the fallback server the client goes to; `null` for the paired one. */
	private via: string | null = null;
	/** Ends a connection to several servers that is under way, when the client is dropped. */
	private abortReach: (() => void) | null = null;
	/** Whether the client has authenticated (`welcome` received) and may sync. */
	private ready = false;
	private unsubscribers: (() => void)[] = [];
	/** Moves whenever the client is dropped, so anything still running on it stands down. */
	private generation = 0;
	/** The generation a connection attempt is under way in; stale once the client is dropped. */
	private connectingIn: number | null = null;
	private visible = false;
	private halted: WorkspaceError<string> | null = null;
	/** Set once the write guard trips; only a new engine (the app reopening) clears it. */
	private writesStopped = false;
	private retryIndex = 0;
	private retryTimer: unknown = null;
	private debounceTimer: unknown = null;
	/** The change count at the last successful sync; `null` until one in this run. */
	private syncedCount: number | null = null;
	private running: Promise<void> | null = null;
	private again = false;
	private writeTimes: number[] = [];
	/** The transfer under way or cut off. */
	private transfer: Transfer | null = null;
	/** `transfer.done` when the state last told it. */
	private toldDone = 0;
	/** Reads kept for the next attempt, by canonical key, until a plan takes them. */
	private readonly reads = new Map<string, PartialRead>();
	/** The transaction this device writes in, while it may still be open on the server. */
	private openTx: OpenTransaction | null = null;
	/** The server the open connection reached, by id. */
	private connectedTo: string | null = null;
	/** The last plan the server refused, not sent again while nothing changes. */
	private refused: Refused | null = null;
	/** Set by `syncNow`: a refused plan is sent again once, as the user asked. */
	private retryRefused = false;
	/** The divergences the server last named. */
	private diverged: DivergedServer[] = [];
	/** The name of the main, for the question about a divergence. */
	private mainName: string | null = null;
	private state: SyncState<string> = {
		status: 'unpaired',
		serverName: null,
		via: null,
		lastSyncAt: null,
		error: null,
		transfer: null,
		diverged: [],
	};

	constructor(options: SyncEngineOptions<DocCode>) {
		this.store = options.pairingStore;
		this.adapter = options.adapter;
		this.onState = options.onState;
		this.deviceName = options.deviceName ?? browserDeviceName;
		this.connectTo = options.connect ?? connectToServer(options.app);
		this.now = options.now ?? (() => new Date());
		this.timers = options.timers ?? browserTimers;
	}

	start(visible: boolean): void {
		this.visible = visible;
		this.pairing = this.store.load();
		if (!this.pairing) {
			this.setState({ status: 'unpaired', error: null });
			return;
		}

		// A large upload cut off in an earlier run: the next sync returns to it.
		// With more than SMALL_TRANSFER_BYTES left to send it waits for the
		// app's choice; with less (every body sent, the commit's answer lost),
		// it goes on by itself.
		const upload = this.pairing.upload;
		if (upload) {
			this.openTx = {
				id: upload.tx,
				server: upload.server,
				bodies: new Map(upload.bodies.map((body) => [documentPath(body), body])),
				persist: true,
				total: upload.total,
			};
			if (upload.total - upload.done > SMALL_TRANSFER_BYTES) {
				this.transfer = {
					direction: 'up',
					done: upload.done,
					total: upload.total,
					interrupted: true,
				};
			}
		}

		this.setState({ status: 'connecting', error: null });
		if (visible) void this.connect();
	}

	setVisible(visible: boolean): void {
		this.visible = visible;
		if (!visible) {
			this.cancelTimers();
			this.dropClient();
			return;
		}

		this.retryIndex = 0;
		if (!this.client && !this.isConnecting()) void this.connect();
	}

	online(): void {
		if (!this.visible) return;
		this.retryIndex = 0;
		if (!this.client && !this.isConnecting()) void this.connect();
	}

	localChanged(): void {
		if (!this.pairing || this.halted || !this.visible) return;
		this.retryIndex = 0;
		if (this.debounceTimer !== null) this.timers.clear(this.debounceTimer);
		this.debounceTimer = this.timers.set(() => {
			this.debounceTimer = null;
			void this.trigger();
		}, CHANGE_DEBOUNCE_MS);
	}

	/**
	 * Syncs at once. A plan the server refused (`quota`, `badPath`) is sent
	 * again by this, and otherwise only once something changed.
	 */
	async syncNow(): Promise<void> {
		this.retryIndex = 0;
		this.retryRefused = true;
		await this.trigger();
	}

	/**
	 * Resumes a large transfer that was cut off (`transfer.interrupted`): the
	 * next sync lists and plans again, and sends or reads only what the
	 * server or this device does not hold yet. Does nothing when no transfer
	 * waits.
	 */
	async resumeTransfer(): Promise<void> {
		if (!this.transfer?.interrupted) return;
		this.transfer = null;
		this.retryIndex = 0;
		this.setState({});
		await this.trigger();
	}

	/**
	 * Starts a large transfer that was cut off (`transfer.interrupted`) over:
	 * what was sent or read of it is dropped, and the next sync sends or reads
	 * everything afresh. Does nothing when no transfer waits.
	 */
	async restartTransfer(): Promise<void> {
		if (!this.transfer?.interrupted) return;
		this.transfer = null;
		this.reads.clear();
		// The next write begins another transaction, which discards this one on the server.
		this.forgetTransaction();
		this.retryIndex = 0;
		this.setState({});
		await this.trigger();
	}

	/**
	 * Answers a divergence (`state.diverged`) for the whole set: keeps the
	 * copy of the main (`'main'`) or of the subordinate `serverId`
	 * (`'subordinate'`). Ask the user with both servers' names. Resolves once
	 * the server has answered, the first choice on any server winning; the
	 * divergence then leaves the state, and the copy not kept changes like
	 * any other write. Rejects with `closed` while there is no connection.
	 * Requests on one connection go one at a time, so it waits for one in
	 * flight, a large upload's body included.
	 */
	async resolveDivergence(serverId: string, keep: Keep): Promise<void> {
		const client = this.ready ? this.client : null;
		if (!client) throw new WorkspaceError('Not connected to a server', 'closed');
		await client.resolve(serverId, keep);
		if (client !== this.client) return;
		this.diverged = this.diverged.filter((entry) => entry.serverId !== serverId);
		this.setState({});
	}

	/**
	 * Pairs with the server a code names, then syncs at once — the first frame
	 * after `welcome` is what confirms the pairing to the server. A failure
	 * leaves the previous state as it was and is rethrown for the page to show.
	 */
	async pair(code: PairingCode): Promise<void> {
		this.cancelTimers();
		this.dropClient();

		let client: SyncClient | null = null;
		let paired: Awaited<ReturnType<SyncClient['pair']>>;
		try {
			client = await this.connectTo(code);
			paired = await client.pair(code.secret, this.deviceName());
		} catch (error) {
			client?.close();
			if (this.pairing && this.visible && !this.halted) void this.connect();
			throw asWorkspaceError(error);
		}

		const { welcome } = paired;
		const pairing: Pairing = {
			serverId: welcome.serverId,
			name: welcome.name,
			device: paired.device,
			token: paired.token,
			password: code.password,
			fingerprint: code.fingerprint,
			port: code.port,
			hosts: welcome.hosts,
			lastRevision: null,
			lastSyncAt: null,
		};
		this.pairing = withFallbacks(
			pairing,
			fallbacksFrom(pairing, welcome.serverId, welcome.subordinates ?? []),
		);
		this.store.save(this.pairing);
		this.halted = null;
		this.syncedCount = null;
		this.retryIndex = 0;
		this.resetTransfers();

		this.attach(client);
		this.ready = true;
		this.heard(welcome);
		this.setState({ status: 'syncing', error: null });
		await this.requestSync();
	}

	/** Asks the server to forget this device when it can, and always forgets the pairing here. */
	async disconnect(): Promise<void> {
		this.cancelTimers();
		const client = this.ready ? this.client : null;
		this.dropClient(false);

		if (client) {
			try {
				await client.forget();
			} catch {
				// The pairing goes here regardless; the server's page can remove the device.
			}
		}
		client?.close();

		this.pairing = null;
		this.halted = null;
		this.syncedCount = null;
		this.resetTransfers();
		this.diverged = [];
		this.mainName = null;
		this.store.clear();
		this.setState({ status: 'unpaired', error: null });
	}

	/**
	 * A sync over the open connection, or a connection first when there is
	 * none. A connection under way syncs once it is up, which covers this too.
	 */
	private async trigger(): Promise<void> {
		if (!this.pairing || this.halted || this.isConnecting()) return;
		if (this.client && this.ready) await this.requestSync();
		else if (this.visible) await this.connect();
	}

	private isConnecting(): boolean {
		return this.connectingIn === this.generation;
	}

	private async connect(): Promise<void> {
		const pairing = this.pairing;
		if (!pairing || this.halted || !this.visible) return;

		this.cancelRetry();
		this.dropClient();
		const generation = this.generation;
		this.connectingIn = generation;
		this.setState({ status: 'connecting', error: null });

		if (pairing.fallbacks?.length) {
			await this.connectToAny(pairing, generation);
			return;
		}

		try {
			const client = await this.connectTo(pairing);
			if (generation !== this.generation) {
				client.close();
				return;
			}
			this.attach(client);

			const welcome = await client.hello(pairing.device, pairing.token);
			if (generation !== this.generation) return;
			if (welcome.serverId !== pairing.serverId) {
				throw new WorkspaceError('A different server answered at this address', 'serverChanged');
			}

			this.ready = true;
			this.connectingIn = null;
			this.learn(pairing.serverId, welcome);
		} catch (error) {
			if (generation !== this.generation) return;
			// The server closes the channel after refusing a hello; drop it
			// first, so that close is not read as a lost connection.
			this.dropClient();
			this.fail(asWorkspaceError(error));
			return;
		}

		await this.requestSync();
	}

	/** `connect` for a pairing with fallback servers: the best of them that answers. */
	private async connectToAny(pairing: Pairing, generation: number): Promise<void> {
		let reached: Reached;
		try {
			reached = await this.reachAny(pairing, generation);
		} catch (error) {
			if (generation !== this.generation) return;
			this.dropClient();
			this.fail(asWorkspaceError(error));
			return;
		}
		if (generation !== this.generation) {
			reached.client.close();
			return;
		}

		this.attach(reached.client);
		this.ready = true;
		this.connectingIn = null;
		const { server, welcome } = reached;
		if (
			server.serverId !== pairing.serverId &&
			welcome.role === 'main' &&
			(welcome.term ?? 0) >= (pairing.term ?? 0)
		) {
			// The family's main moved: the device follows it.
			this.pairing = movedTo(pairing, server, welcome);
			this.store.save(this.pairing);
		}
		this.via = server.serverId === this.pairing?.serverId ? null : welcome.name;
		this.learn(server.serverId, welcome);
		await this.requestSync();
	}

	/**
	 * Connects to the paired server and every fallback at once, and says
	 * hello on each. The paired server answering as a main is taken at once.
	 * Any other answer waits, until `MAIN_GRACE_MS` after the start or until
	 * every attempt has ended, and then the best one is taken: a main before
	 * a subordinate, the higher term among mains (two mains at once, while a
	 * family is split), then the paired server, then the list's order. A
	 * welcome from a server outside the set is not taken (`serverChanged`).
	 * The clients not taken are closed.
	 *
	 * Fails with the paired server's own error when no server answered, and
	 * at once when the paired server refuses with an error that halts: a
	 * fallback's refusal never does, since a subordinate learns of a new
	 * device only at its next link.
	 */
	private reachAny(pairing: Pairing, generation: number): Promise<Reached> {
		const servers: FallbackServer[] = [pairing, ...(pairing.fallbacks ?? [])];
		const known = new Map(servers.map((server) => [server.serverId, server]));
		interface Attempt {
			client: SyncClient | null;
			welcome: Welcome | null;
			error: WorkspaceError<string> | null;
			done: boolean;
			unsubscribe?: () => void;
		}
		const attempts: Attempt[] = servers.map(() => ({
			client: null,
			welcome: null,
			error: null,
			done: false,
		}));
		const paired = attempts[0];
		/** Lower is better: a main before a subordinate, then the paired server. */
		const rank = (welcome: Welcome): number =>
			(welcome.role === 'subordinate' ? 2 : 0) + (welcome.serverId === pairing.serverId ? 0 : 1);
		/** Whether `a` is better than `b`: by rank, and between two mains by term first. */
		const better = (a: Welcome, b: Welcome): boolean => {
			const mains = a.role !== 'subordinate' && b.role !== 'subordinate';
			if (mains && (a.term ?? 0) !== (b.term ?? 0)) return (a.term ?? 0) > (b.term ?? 0);
			return rank(a) < rank(b);
		};

		return new Promise<Reached>((resolve, reject) => {
			let settled = false;
			let graceOver = false;
			let grace: unknown = null;

			const finish = (winner: Attempt | null, error: WorkspaceError<string> | null): void => {
				settled = true;
				if (grace !== null) this.timers.clear(grace);
				if (this.abortReach === abort) this.abortReach = null;
				for (const attempt of attempts) {
					attempt.unsubscribe?.();
					if (attempt !== winner) attempt.client?.close();
				}
				const server = winner?.welcome ? known.get(winner.welcome.serverId) : undefined;
				if (winner?.client && winner.welcome && server) {
					resolve({ client: winner.client, welcome: winner.welcome, server });
				} else {
					reject(error ?? new WorkspaceError('No server answered', 'connectTimeout'));
				}
			};

			const decide = (): void => {
				if (settled) return;
				if (generation !== this.generation) {
					finish(null, new WorkspaceError('The connection was closed', 'closed'));
					return;
				}
				if (paired.welcome && rank(paired.welcome) === 0) {
					finish(paired, null);
					return;
				}
				if (paired.error && HALTING.has(paired.error.code)) {
					finish(null, paired.error);
					return;
				}
				const answered = attempts.filter((attempt) => attempt.welcome !== null);
				const allDone = attempts.every((attempt) => attempt.done);
				if (answered.length > 0 && (graceOver || allDone)) {
					// reduce keeps the first of equal ranks: the list's order.
					const best = answered.reduce((a, b) =>
						better(b.welcome as Welcome, a.welcome as Welcome) ? b : a,
					);
					finish(best, null);
					return;
				}
				if (allDone) finish(null, paired.error);
			};

			const abort = (): void => decide();
			this.abortReach = abort;
			grace = this.timers.set(() => {
				grace = null;
				graceOver = true;
				decide();
			}, MAIN_GRACE_MS);

			servers.forEach((server, i) => {
				const attempt = attempts[i];
				void (async () => {
					try {
						const client = await this.connectTo(server);
						attempt.client = client;
						if (settled) {
							client.close();
							return;
						}
						// A server that answered and then went away while the
						// grace runs is no longer one to take.
						attempt.unsubscribe = client.onClosed((error) => {
							attempt.welcome = null;
							attempt.error = error;
							decide();
						});
						const welcome = await client.hello(pairing.device, pairing.token);
						if (!known.has(welcome.serverId)) {
							throw new WorkspaceError(
								'A different server answered at this address',
								'serverChanged',
							);
						}
						attempt.welcome = welcome;
					} catch (error) {
						attempt.error = asWorkspaceError(error);
						attempt.client?.close();
					} finally {
						attempt.done = true;
						decide();
					}
				})();
			});
		});
	}

	private attach(client: SyncClient): void {
		this.client = client;
		this.unsubscribers = [
			client.onChanged(() => {
				if (client === this.client && this.ready) void this.requestSync();
			}),
			client.onDiverged((servers) => {
				if (client !== this.client) return;
				this.diverged = servers;
				this.setState({});
			}),
			client.onResolved((server) => {
				if (client !== this.client) return;
				this.diverged = this.diverged.filter((entry) => entry.serverId !== server);
				this.setState({});
			}),
			client.onClosed((error) => {
				if (client === this.client) this.fail(error);
			}),
		];
	}

	/** Drops the client and invalidates anything still running on it. */
	private dropClient(close = true): void {
		this.generation++;
		for (const unsubscribe of this.unsubscribers) unsubscribe();
		this.unsubscribers = [];
		if (close) this.client?.close();
		this.client = null;
		this.ready = false;
		this.via = null;
		// A connection to several servers under way ends now, closing them.
		const abort = this.abortReach;
		this.abortReach = null;
		abort?.();
	}

	/** Runs a sync, or once more after the one running now. */
	private requestSync(): Promise<void> {
		if (this.running) {
			this.again = true;
			return this.running;
		}

		this.running = (async () => {
			try {
				do {
					this.again = false;
					await this.syncOnce();
				} while (this.again && this.client && this.ready && !this.halted);
			} finally {
				this.running = null;
			}
		})();

		return this.running;
	}

	private async syncOnce(): Promise<void> {
		const client = this.client;
		const pairing = this.pairing;
		if (!client || !this.ready || !pairing || this.halted) return;
		// `syncNow` sends a refused plan again with the sync it starts, and
		// only that one, whatever it comes to.
		const retryRefused = this.retryRefused;
		this.retryRefused = false;
		if (this.transfer?.interrupted) {
			// A large transfer was cut off: the app chooses to resume or restart it.
			this.setState({ status: 'interrupted', error: null });
			return;
		}
		const generation = this.generation;
		const current = (): boolean => generation === this.generation;

		this.setState({ status: 'syncing', error: null });
		try {
			const head = await client.head();
			if (!current()) return;
			if (
				head === pairing.lastRevision &&
				this.syncedCount === this.adapter.changeCount() &&
				!this.openTx
			) {
				this.setState({ status: 'connected' });
				return;
			}

			for (let attempt = 0; ; attempt++) {
				/** Lets the loop go round again, or throws `error` once the attempts are used up. */
				const retry = (error: WorkspaceError<string>): void => {
					if (attempt >= MAX_CONFLICT_RETRIES) throw error;
				};

				const listing = await listAll(client);
				if (!current()) return;
				const read = await this.read(client, this.selected(listing), current);
				if (read === null) return;
				if (read === 'stale') {
					retry(new WorkspaceError('The documents kept changing while they were read', 'conflict'));
					continue;
				}

				const now = this.now();
				// Read with the merge, with no await between: the adapter reads the
				// local state in the same tick, so an edit made after this is
				// counted past it and the next sync picks it up.
				const count = this.adapter.changeCount();
				const plan = this.adapter.plan(listing, read, now);
				this.reads.clear();
				if (plan.apply) await plan.apply();
				if (!current()) return;
				const { changes, held } = await this.changesOf(plan, listing);
				if (!current()) return;

				let revision = listing.revision;
				let reported = held;
				if (changes.length > 0) {
					const signature = changes
						.map((change) => `${change.path} ${change.ifEtag} ${change.put?.etag ?? '-'}`)
						.join('\n');
					const refused = this.refused;
					if (
						refused &&
						!retryRefused &&
						refused.signature === signature &&
						refused.revision === listing.revision
					) {
						// The same plan against the same set: the server would refuse it again.
						this.fail(refused.error);
						return;
					}
					let written: CommitResult | WorkspaceError<string> | null;
					try {
						written = await this.write(client, changes, now, current);
					} catch (error) {
						const failure = asWorkspaceError(error);
						if (current() && (failure.code === 'quota' || failure.code === 'badPath')) {
							this.refused = { signature, revision: listing.revision, error: failure };
						}
						throw failure;
					}
					if (written === null) return;
					if (written instanceof WorkspaceError) {
						retry(written);
						continue;
					}
					this.refused = null;
					revision = written.revision;
					const docs = this.landed(changes, written.docs);
					reported = [...docs, ...held];
					if (docs.length < written.docs.length) {
						// A body landed that is not the one sent: write it again.
						revision = listing.revision;
						this.again = true;
					} else if (revision !== this.revisionAfter(listing, changes)) {
						// A commit checks only the documents it writes: another device
						// wrote others between the listing and it, which this device has
						// not read. Its revision is not synced here; sync again.
						revision = listing.revision;
						this.again = true;
					}
				} else if (this.openTx) {
					// Nothing left to write: a transaction kept for resuming is of no more use.
					const tx = this.openTx.id;
					this.forgetTransaction();
					await client.abort(tx);
					if (!current()) return;
				}

				if (changes.length === 0) this.refused = null;
				if (reported.length > 0) this.adapter.committed?.(reported);
				this.syncedCount = count;
				this.savePairing({ lastRevision: revision, lastSyncAt: now.toISOString() });
				this.retryIndex = 0;
				this.setState({ status: 'connected', error: null });
				return;
			}
		} catch (error) {
			if (!current()) return;
			this.fail(asWorkspaceError(error));
		}
	}

	/**
	 * The documents of a commit that hold what was sent: a put whose new etag
	 * is not the etag of the body sent is left out, and logged. It never
	 * happens while the transaction's bookkeeping holds; this is the guard
	 * that an adapter is never told a body landed that did not.
	 */
	private landed(changes: Change[], docs: DocumentEtag[]): DocumentEtag[] {
		const sent = new Map(changes.map((change) => [change.path, change.put?.etag ?? null]));
		return docs.filter((doc) => {
			const etag = sent.get(documentPath(doc));
			if (etag === undefined || etag === doc.etag) return true;
			console.warn(`Server sync: ${documentPath(doc)} landed with another body than the one sent`);
			return false;
		});
	}

	/** The revision `listing` has once `changes` are applied to it. */
	private revisionAfter(listing: Listing, changes: Change[]): string | null {
		const etags = new Map(listing.docs.map((doc) => [documentPath(doc), doc.etag]));
		for (const change of changes) {
			if (change.put) etags.set(change.path, change.put.etag);
			else etags.delete(change.path);
		}
		return revisionOf(etags);
	}

	/** The documents of `listing` the adapter selects, each once, as the listing names them. */
	private selected(listing: Listing): DocumentInfo[] {
		const listed = new Map(listing.docs.map((doc) => [documentPath(doc), doc]));
		const out = new Map<string, DocumentInfo>();
		for (const key of this.adapter.select(listing)) {
			const path = documentPath(key);
			const doc = listed.get(path);
			if (doc && !out.has(path)) out.set(path, doc);
		}
		return [...out.values()];
	}

	/**
	 * Reads `wanted` at the etags the listing named, resuming from what an
	 * earlier attempt read of the same etag. Resolves with the documents, with
	 * `'stale'` when one changed since the listing, or with `null` once the
	 * client was dropped; any other failure is thrown.
	 */
	private async read(
		client: SyncClient,
		wanted: DocumentInfo[],
		current: () => boolean,
	): Promise<DocumentBody[] | 'stale' | null> {
		const etags = new Map(wanted.map((doc) => [documentPath(doc), doc.etag]));
		for (const [path, partial] of this.reads) {
			if (etags.get(path) !== partial.etag) this.reads.delete(path);
		}
		if (wanted.length === 0) return [];

		let done = 0;
		for (const partial of this.reads.values()) done += partial.received;
		const total = wanted.reduce((sum, doc) => sum + doc.bytes, 0);
		this.beginTransfer('down', done, total);

		const out: DocumentBody[] = [];
		try {
			for (const doc of wanted) {
				const path = documentPath(doc);
				let partial = this.reads.get(path);
				if (!partial) {
					partial = { etag: doc.etag, chunks: [], received: 0 };
					this.reads.set(path, partial);
				}
				if (partial.received < doc.bytes) {
					const into = partial;
					const got = await client.get(keyOf(doc), {
						etag: doc.etag,
						offset: into.received,
						onPiece: (piece) => {
							into.chunks.push(piece.slice());
							into.received += piece.byteLength;
							this.moved(piece.byteLength);
						},
					});
					if (!current()) {
						this.cutOff();
						return null;
					}
					if (isStale(got)) {
						this.reads.delete(path);
						this.endTransfer();
						return 'stale';
					}
				}
				out.push({ ...doc, body: joined(partial.chunks, doc.bytes) });
			}
		} catch (error) {
			if (!current() || CONNECTION_LOST.has(asWorkspaceError(error).code)) {
				this.cutOff();
				if (!current()) return null;
			} else {
				this.endTransfer();
			}
			throw error;
		}

		this.endTransfer();
		return out;
	}

	/**
	 * What `plan` changes on the server, against `listing`: each body with
	 * its etag, left out when the server holds the same; each delete of a
	 * document the listing holds, left out for a document the plan also
	 * puts. Of several puts of one key, the last counts.
	 */
	private async changesOf(plan: SyncPlan, listing: Listing): Promise<Changes> {
		const listed = new Map(listing.docs.map((doc) => [documentPath(doc), doc.etag]));
		const puts = new Map<string, PlannedPut>();
		for (const put of plan.put ?? []) {
			const path = documentPath(put.doc);
			puts.delete(path);
			puts.set(path, put);
		}
		const changes: Change[] = [];
		const held: DocumentEtag[] = [];
		for (const [path, { doc, body }] of puts) {
			const etag = await etagOf(body);
			const ifEtag = listed.get(path) ?? null;
			if (etag === ifEtag) {
				held.push({ ...keyOf(doc), etag });
				continue;
			}
			changes.push({ key: keyOf(doc), path, ifEtag, put: { body, bytes: bodySize(body), etag } });
		}
		const deleting = new Set<string>();
		for (const doc of plan.delete ?? []) {
			const path = documentPath(doc);
			if (puts.has(path) || deleting.has(path)) continue;
			deleting.add(path);
			const ifEtag = listed.get(path);
			if (ifEtag === undefined) held.push({ ...keyOf(doc), etag: null });
			else changes.push({ key: keyOf(doc), path, ifEtag, put: null });
		}
		return { changes, held };
	}

	/**
	 * Writes `changes` in one transaction: the one kept open from before when
	 * it is open on this server and holds nothing else (a conflict, a cut-off
	 * upload, a failed commit), so each body it staged that is still wanted
	 * resumes from what the server received; a new one otherwise. Resolves
	 * with the commit, with the error to list and plan again after (a
	 * conflict, or a failure in `RETRY_WRITE`), or with `null` once the
	 * client was dropped meanwhile; any other failure is thrown.
	 *
	 * The transaction's record of its bodies (`OpenTransaction.bodies`, saved
	 * as `Pairing.upload`) only ever names a body the server holds, in part or
	 * whole, or one it holds nothing for yet: a staged body that is not the
	 * one recorded for its key is replaced by a delete before the key is
	 * recorded with the body to send. So a put that never reached the server
	 * cannot leave the record naming bytes the server does not hold.
	 */
	private async write(
		client: SyncClient,
		changes: Change[],
		now: Date,
		current: () => boolean,
	): Promise<CommitResult | WorkspaceError<string> | null> {
		// Refused before any byte goes, though counted at the commit.
		this.guardWrite(now, false);
		const total = changes.reduce((sum, change) => sum + (change.put?.bytes ?? 0), 0);
		const wanted = new Map(changes.map((change) => [change.path, change]));
		let sending = false;

		try {
			let tx = this.openTx;
			let staged: StagedChange[] = [];
			// Another server holds none of this transaction, or another's under its id.
			if (tx && tx.server !== this.connectedTo) tx = null;
			if (tx) {
				staged = (await client.begin(tx.id)).staged;
				if (!current()) return null;
				// A change it staged that is no longer wanted cannot be taken back.
				if (staged.some((change) => !wanted.has(documentPath(change)))) tx = null;
			}
			if (!tx) {
				this.forgetTransaction();
				tx = {
					id: newTransactionId(),
					server: this.connectedTo ?? '',
					bodies: new Map(),
					persist: false,
					total: 0,
				};
				staged = (await client.begin(tx.id)).staged;
				if (!current()) return null;
			}
			this.openTx = tx;

			// A body staged that is not the recorded one for what is to be sent
			// goes first: a put that then never arrives leaves a delete, never
			// another body's bytes under the new body's etag.
			const resumable = new Map<string, number>();
			for (const entry of staged) {
				if (entry.delete) continue;
				const path = documentPath(entry);
				const change = wanted.get(path);
				const had = tx.bodies.get(path);
				if (
					change?.put &&
					had?.etag === change.put.etag &&
					had.bytes === change.put.bytes &&
					entry.bytes === had.bytes
				) {
					resumable.set(path, entry.received);
					continue;
				}
				tx.bodies.delete(path);
				if (change?.put) {
					await client.delete(tx.id, change.key, change.ifEtag);
					if (!current()) return null;
				}
			}

			let offsets = changes.map((change) => resumable.get(change.path) ?? 0);
			tx.bodies = new Map(
				changes.flatMap((change) =>
					change.put
						? [[change.path, { ...change.key, bytes: change.put.bytes, etag: change.put.etag }]]
						: [],
				),
			);
			tx.total = total;
			tx.persist = total > SMALL_TRANSFER_BYTES;
			let done = offsets.reduce((sum, offset) => sum + offset, 0);
			// A small one starts over when it is cut off: nothing to resume after a reload.
			if (!tx.persist) {
				this.dropUploadRecord();
			} else if (!this.saveUpload(done)) {
				// The record could not be saved for this plan, and an older one
				// may name other bodies under this transaction's id. Another
				// transaction, kept in memory only, discards this one on the
				// server, so that no record ever resumes it after a reload.
				this.dropUploadRecord();
				tx = { ...tx, id: newTransactionId(), persist: false };
				staged = (await client.begin(tx.id)).staged;
				if (!current()) return null;
				this.openTx = tx;
				offsets = changes.map(() => 0);
				done = 0;
			}

			sending = true;
			this.beginTransfer('up', done, total);
			for (const [i, change] of changes.entries()) {
				if (!change.put) {
					await client.delete(tx.id, change.key, change.ifEtag);
				} else {
					let position = offsets[i];
					await client.put(tx.id, change.key, change.ifEtag, change.put.body, position, (at) => {
						this.moved(at - position);
						position = at;
					});
					this.moved(change.put.bytes - position);
				}
				if (!current()) {
					this.cutOff();
					return null;
				}
			}
			sending = false;
			this.endTransfer();
			// Every body arrived: a reload before the commit's answer has nothing to ask.
			if (tx.persist) this.saveUpload(total);

			this.guardWrite(this.now(), true);
			const result = await client.commit(tx.id);
			if (!current()) return null;
			if (result.committed) {
				this.forgetTransaction();
				return result;
			}
			// A conflict leaves the transaction open with its bodies: the next
			// attempt returns to it, and sends again only what changed.
			return new WorkspaceError('Another device wrote first', 'conflict');
		} catch (error) {
			const failure = asWorkspaceError(error);
			if (!current() || CONNECTION_LOST.has(failure.code)) {
				if (sending) this.cutOff();
				if (!current()) return null;
				throw failure;
			}
			this.endTransfer();
			if (DISCARD_TX.has(failure.code)) this.forgetTransaction();
			// A failure after the commit point may have applied the write and
			// ended the transaction; one before it leaves the transaction
			// open. Either way the next listing tells what the set holds, and
			// the next begin of the same id whether it is still open.
			if (RETRY_WRITE.has(failure.code)) return failure;
			throw failure;
		}
	}

	/** A transfer starts: `done` of its `total` bytes are already there. */
	private beginTransfer(direction: Transfer['direction'], done: number, total: number): void {
		this.transfer = { direction, done, total, interrupted: false };
		this.toldDone = done;
		this.setState({});
	}

	/** `bytes` more of the transfer went; the state is told every 1% or 64 KiB, and at the end. */
	private moved(bytes: number): void {
		const transfer = this.transfer;
		if (!transfer || bytes === 0) return;
		transfer.done += bytes;
		const step = Math.max(64 * 1024, transfer.total / 100);
		if (transfer.done - this.toldDone >= step || transfer.done >= transfer.total) {
			this.toldDone = transfer.done;
			this.setState({});
		}
	}

	private endTransfer(): void {
		if (!this.transfer) return;
		this.transfer = null;
		this.setState({});
	}

	/**
	 * The transfer under way was cut off with its connection. One of at most
	 * `SMALL_TRANSFER_BYTES` starts over by itself: its reads are dropped, and
	 * the next write begins another transaction. A larger one with more than
	 * that left to go waits for the app's choice; with less left, it goes on
	 * by itself with the next sync. `done` counts what went into the
	 * channel's buffer, which may be up to 1 MiB ahead of what arrived: it
	 * decides only whether to ask, never where to resume.
	 */
	private cutOff(): void {
		const transfer = this.transfer;
		if (!transfer || transfer.interrupted) return;
		if (transfer.total <= SMALL_TRANSFER_BYTES) {
			this.transfer = null;
			if (transfer.direction === 'down') this.reads.clear();
			else this.forgetTransaction();
		} else {
			const ask = transfer.total - transfer.done > SMALL_TRANSFER_BYTES;
			this.transfer = ask ? { ...transfer, interrupted: true } : null;
			if (transfer.direction === 'up') this.saveUpload(transfer.done);
		}
		this.setState({});
	}

	/**
	 * Saves the open transaction to resume after a reload, with `done` of its
	 * bytes sent. It is saved when it starts, when it is cut off, and once
	 * every body arrived, never on progress: the record may be large, and
	 * storage slow or full.
	 */
	private saveUpload(done: number): boolean {
		const tx = this.openTx;
		if (!tx?.persist || !this.pairing) return true;
		return this.savePairing({
			upload: {
				tx: tx.id,
				server: tx.server,
				total: tx.total,
				done,
				bodies: [...tx.bodies.values()],
			},
		});
	}

	/** Forgets the transaction kept open, here and in the pairing record. */
	private forgetTransaction(): void {
		this.openTx = null;
		this.dropUploadRecord();
	}

	/** Removes a large upload from the pairing record. */
	private dropUploadRecord(): void {
		if (!this.pairing?.upload) return;
		const next = { ...this.pairing };
		delete next.upload;
		this.pairing = next;
		this.store.save(next);
	}

	/** Forgets every transfer: for a new pairing, or none. */
	private resetTransfers(): void {
		this.transfer = null;
		this.reads.clear();
		this.openTx = null;
		this.refused = null;
	}

	/**
	 * The write guard: past `MAX_WRITES_PER_MINUTE` commits within a minute,
	 * writing stops until the app starts again. `count` counts a commit;
	 * without it, this only refuses a write once the limit is reached.
	 */
	private guardWrite(now: Date, count: boolean): void {
		const time = now.getTime();
		this.writeTimes = this.writeTimes.filter((at) => time - at < WRITE_WINDOW_MS);
		if (this.writesStopped || this.writeTimes.length >= MAX_WRITES_PER_MINUTE) {
			this.writesStopped = true;
			console.warn(
				`Server sync stopped writing: more than ${MAX_WRITES_PER_MINUTE} writes within a minute`,
			);
			throw new WorkspaceError('Too many writes within a minute', 'tooManyWrites');
		}
		if (count) this.writeTimes.push(time);
	}

	private fail(error: WorkspaceError<string>): void {
		if (error instanceof DocumentError || HALTING.has(error.code)) {
			this.halted = error;
			this.reads.clear();
			this.cancelTimers();
			this.dropClient();
			this.setState({ status: 'error', error });
			return;
		}

		if (CONNECTION_LOST.has(error.code)) {
			this.dropClient();
			this.setState({ status: 'unreachable', error });
			this.scheduleRetry();
			return;
		}

		if (TEMPORARY.has(error.code)) {
			// The server closes the channel after refusing a hello; after a
			// refused request on an open one, a fresh connection is as good.
			this.dropClient();
			this.setState({ status: 'paused', error });
			this.scheduleRetry(error.code === 'blocked' ? BLOCKED_RETRY_MS : undefined);
			return;
		}

		// A refusal retrying will not cure by itself (quota, a conflict that
		// kept recurring…): say so, and try again on the next
		// trigger. An open connection stays open for it; one that closed is
		// reconnected then.
		this.setState({ status: 'error', error });
	}

	/** Retries after `delayMs`, or on the growing schedule when none is given. */
	private scheduleRetry(delayMs?: number): void {
		if (!this.visible || this.halted || !this.pairing) return;
		this.cancelRetry();
		let delay = delayMs;
		if (delay === undefined) {
			delay = RETRY_DELAYS_MS[Math.min(this.retryIndex, RETRY_DELAYS_MS.length - 1)];
			this.retryIndex++;
		}
		this.retryTimer = this.timers.set(() => {
			this.retryTimer = null;
			void this.connect();
		}, delay);
	}

	private cancelRetry(): void {
		if (this.retryTimer !== null) this.timers.clear(this.retryTimer);
		this.retryTimer = null;
	}

	private cancelTimers(): void {
		this.cancelRetry();
		if (this.debounceTimer !== null) this.timers.clear(this.debounceTimer);
		this.debounceTimer = null;
	}

	/** Saves `patch` into the pairing record; `false` when the store could not. */
	private savePairing(patch: Partial<Pairing>): boolean {
		if (!this.pairing) return true;
		this.pairing = { ...this.pairing, ...patch };
		const saved = this.store.save(this.pairing) !== false;
		this.setState({});
		return saved;
	}

	/**
	 * Keeps what a welcome says about the server that sent it (its hosts and
	 * name: the paired server's, or that fallback's), and the fallbacks a
	 * main lists. A welcome without a list (a subordinate's, or an older
	 * server's) leaves the fallbacks as they are.
	 */
	private learn(serverId: string, welcome: Welcome): void {
		const pairing = this.pairing;
		if (!pairing) return;
		const seen = { hosts: welcome.hosts, name: welcome.name };
		let next: Pairing =
			serverId === pairing.serverId
				? {
						...pairing,
						...seen,
						...(welcome.role === 'main' && welcome.term ? { term: welcome.term } : {}),
					}
				: withFallbacks(
						pairing,
						(pairing.fallbacks ?? []).map((server) =>
							server.serverId === serverId ? { ...server, ...seen } : server,
						),
					);
		if (welcome.subordinates) {
			next = withFallbacks(next, fallbacksFrom(next, serverId, welcome.subordinates));
		}
		this.pairing = next;
		this.store.save(next);
		this.heard(welcome);
	}

	/**
	 * Takes the divergences a welcome names, which replace those of any
	 * server before, and the main's name for asking about them: the server's
	 * own when it answers as a main, otherwise the paired server's, the main
	 * this device last heard of.
	 */
	private heard(welcome: Welcome): void {
		this.connectedTo = welcome.serverId;
		this.diverged = welcome.diverged ?? [];
		this.mainName =
			welcome.role === 'subordinate' ? (this.pairing?.name ?? welcome.name) : welcome.name;
		this.setState({});
	}

	private setState(patch: Partial<SyncState<string>>): void {
		const mainName = this.mainName ?? this.pairing?.name ?? '';
		this.state = {
			...this.state,
			...patch,
			serverName: this.pairing?.name ?? null,
			via: this.via,
			lastSyncAt: this.pairing?.lastSyncAt ?? null,
			transfer: this.transfer ? { ...this.transfer } : null,
			diverged: this.diverged.map((entry) => ({ ...entry, mainName })),
		};
		this.onState({ ...this.state } as SyncState<DocCode>);
	}
}
