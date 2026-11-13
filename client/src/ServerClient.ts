import {
	ChannelFraming,
	bodySize,
	type Body,
	type Frame,
	type FrameChannel,
	type Incoming,
} from './ChannelFraming.js';
import { readFallbackServer, type FallbackServer } from './PairingCode.js';
import { WorkspaceError, isServerErrorCode } from './WorkspaceError.js';

/** The protocol this client speaks; the server refuses any other with `protocol`. */
export const PROTOCOL_VERSION = 1;

/** The most entries, documents and tombstones together, one list page of a server carries. */
export const LIST_PAGE = 500;

/** How long `forget` waits for the server to close the channel before giving up on it. */
const FORGET_WAIT_MS = 2000;

/** How often `listAll` starts over when the set changes between its pages. */
const LIST_ATTEMPTS = 8;

/** A server whose copy of the app diverged from its main's, and since when (RFC 3339 UTC). */
export interface DivergedServer {
	serverId: string;
	name: string;
	since: string;
}

export interface Welcome {
	serverId: string;
	name: string;
	/** The revision of the app's set of documents, or `null` when the set is empty. */
	revision: string | null;
	/** The server's `.local` name first, then its current LAN IPv4 addresses. */
	hosts: string[];
	/**
	 * `main`, or `subordinate` for a server linked to a main; absent from a
	 * server from before roles, which is a main.
	 */
	role?: string;
	/**
	 * The family's term: it goes up by one every time a subordinate becomes
	 * the main, and the higher term wins. Absent from a server whose family
	 * never switched its main, and from a server from before terms: 0.
	 */
	term?: number;
	/**
	 * A main's subordinates, which serve its devices too: absent from a
	 * subordinate and from a server from before subordinates, `[]` from a
	 * main without any. An entry this client cannot read is left out.
	 */
	subordinates?: FallbackServer[];
	/**
	 * The servers whose copy of the app diverged from their main's and waits
	 * for a device's choice (`resolve`); absent when there is none.
	 */
	diverged?: DivergedServer[];
}

/**
 * A document of an app: its folder (`''` for the app's root, otherwise
 * segments of `[a-z0-9_-]` joined by `/`), its id and its type (the file's
 * extension). The server keeps it as `<path>/<id>.<type>`.
 */
export interface DocumentKey {
	path: string;
	id: string;
	type: string;
}

/** A document of the app's set: its etag (the lowercase hex SHA-256 of its bytes) and its size. */
export interface DocumentInfo extends DocumentKey {
	etag: string;
	bytes: number;
}

/** The tombstone of a deleted document, and when it went (RFC 3339 UTC). */
export interface RemovedDocument extends DocumentKey {
	at: string;
}

/**
 * One page of a listing: the set's revision (`null` for an empty set), its
 * documents, then its tombstones, each sorted by key, and the cursor of the
 * next page, absent after the last.
 */
export interface ListPage {
	revision: string | null;
	docs: DocumentInfo[];
	removed: RemovedDocument[];
	cursor?: string;
}

/** The app's whole set, every page of one revision. */
export interface Listing {
	revision: string | null;
	docs: DocumentInfo[];
	removed: RemovedDocument[];
}

/**
 * The answer to a request made against a view the set has left: the document
 * asked for is not there with that etag, or the listing moved on. `stale` is
 * the set's revision now; list again.
 */
export interface Stale {
	stale: string | null;
}

/**
 * A document as `get` read it: the bytes past `offset`, in `body`, or handed
 * to `onPiece` as they arrived (then `body` is empty).
 */
export interface ReadDocument extends DocumentInfo {
	offset: number;
	body: Uint8Array;
}

/**
 * A change an open transaction holds, as `begin` reports it for resuming: a
 * put of `bytes` bytes, of which `received` have arrived, or a delete.
 * `ifEtag` is the etag the document must have at the commit, `null` if it
 * must not exist.
 */
export interface StagedChange extends DocumentKey {
	delete?: true;
	ifEtag: string | null;
	bytes: number;
	received: number;
}

export interface Began {
	tx: string;
	staged: StagedChange[];
}

/** The answer to a put or a delete: how many bytes of the body the server holds (0 for a delete). */
export interface StagedAck extends DocumentKey {
	received: number;
}

/** A document and its etag, `null` when it does not exist. */
export interface DocumentEtag extends DocumentKey {
	etag: string | null;
}

/**
 * What a commit answered. Committed: the set's new revision, and each
 * document written, with its etag, or deleted, with a `null` etag. Not
 * committed: a conflict, with the set's revision now and the current etag of
 * each document that conflicts; the transaction stays open.
 */
export interface CommitResult {
	committed: boolean;
	revision: string | null;
	docs: DocumentEtag[];
}

/** The side a `resolve` keeps when the app's servers diverged. */
export type Keep = 'main' | 'subordinate';

export interface GetOptions {
	/**
	 * Read the document only if its etag is this one (normally the etag a
	 * listing named); otherwise the answer is `Stale`. Leave it out, or pass
	 * `null`, to read the document as it is.
	 */
	etag?: string | null;
	/** Resume from this many bytes in: it needs `etag`, the etag of the body it resumes. */
	offset?: number;
	/**
	 * Takes the body piece by piece as it arrives, so it never sits whole in
	 * memory; the result's `body` is then empty. A piece is valid only during
	 * the call. Leave it out to get the body whole.
	 */
	onPiece?: (piece: Uint8Array) => void;
}

export interface ServerClientOptions {
	/**
	 * This app's id on the server (`[a-z0-9-]{1,64}`): its folder there, and
	 * what `hello` and `pair` name.
	 */
	app: string;
}

/**
 * How a request reads a frame: the value it resolves with, `'wait'` for a
 * frame it takes and waits past (`paired` before `welcome`), or `null` for
 * one it did not expect, which means the two sides disagree.
 */
type Reply<T> = { value: T } | 'wait' | null;

/** A request waiting for its reply; `handle` returns `true` once the reply is complete. */
interface Pending {
	kind: string;
	handle(incoming: Incoming): boolean;
	reject(error: unknown): void;
	/** Where the body of a `doc` answering it goes, piece by piece; `null` to have it whole. */
	receive: ((frame: Frame) => ((piece: Uint8Array) => void) | null) | null;
}

function protocolError(message: string): WorkspaceError {
	return new WorkspaceError(message, 'protocol');
}

/** What a get's `onPiece` threw, carried to the get once its body is in. */
class PieceFailure {
	constructor(readonly error: unknown) {}
}

function isRevision(value: unknown): value is string | null {
	return value === null || typeof value === 'string';
}

function isCount(value: unknown): value is number {
	return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
}

function readKey(raw: Record<string, unknown>): DocumentKey | null {
	const { path, id, type } = raw;
	if (typeof path !== 'string' || typeof id !== 'string' || typeof type !== 'string') return null;
	return { path, id, type };
}

function sameKey(a: DocumentKey, b: DocumentKey): boolean {
	return a.path === b.path && a.id === b.id && a.type === b.type;
}

function isObject(value: unknown): value is Record<string, unknown> {
	return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/** Reads a list of entries with `read`; `null` when it is not a list or any entry does not read. */
function readList<T>(value: unknown, read: (raw: Record<string, unknown>) => T | null): T[] | null {
	if (!Array.isArray(value)) return null;
	const out: T[] = [];
	for (const raw of value) {
		const entry = isObject(raw) ? read(raw) : null;
		if (entry === null) return null;
		out.push(entry);
	}
	return out;
}

function readDiverged(value: unknown): DivergedServer[] | null {
	return readList(value, ({ serverId, name, since }) =>
		typeof serverId === 'string' && typeof name === 'string' && typeof since === 'string'
			? { serverId, name, since }
			: null,
	);
}

function readDocumentEtags(value: unknown): DocumentEtag[] | null {
	return readList(value, (raw) => {
		const key = readKey(raw);
		return key && isRevision(raw.etag) ? { ...key, etag: raw.etag } : null;
	});
}

function readPage(frame: Frame): ListPage | null {
	const docs = readList(frame.docs, (raw) => {
		const key = readKey(raw);
		return key && typeof raw.etag === 'string' && isCount(raw.bytes)
			? { ...key, etag: raw.etag, bytes: raw.bytes }
			: null;
	});
	const removed = readList(frame.removed, (raw) => {
		const key = readKey(raw);
		return key && typeof raw.at === 'string' ? { ...key, at: raw.at } : null;
	});
	if (!isRevision(frame.revision) || !docs || !removed) return null;
	if (frame.cursor !== undefined && typeof frame.cursor !== 'string') return null;

	const page: ListPage = { revision: frame.revision, docs, removed };
	if (typeof frame.cursor === 'string') page.cursor = frame.cursor;
	return page;
}

function readStaged(value: unknown): StagedChange[] | null {
	return readList(value, (raw) => {
		const key = readKey(raw);
		if (!key || !isRevision(raw.ifEtag) || !isCount(raw.bytes) || !isCount(raw.received)) {
			return null;
		}
		const change: StagedChange = {
			...key,
			ifEtag: raw.ifEtag,
			bytes: raw.bytes,
			received: raw.received,
		};
		if (raw.delete === true) change.delete = true;
		return change;
	});
}

function readWelcome(frame: Frame): Welcome {
	const { serverId, name, revision, hosts } = frame;
	if (
		typeof serverId !== 'string' ||
		typeof name !== 'string' ||
		!isRevision(revision) ||
		!Array.isArray(hosts) ||
		!hosts.every((host) => typeof host === 'string' && host.length > 0)
	) {
		throw protocolError('A malformed welcome');
	}

	const welcome: Welcome = { serverId, name, revision, hosts: [...hosts] as string[] };
	if (typeof frame.role === 'string') welcome.role = frame.role;
	if (typeof frame.term === 'number' && Number.isSafeInteger(frame.term) && frame.term >= 0) {
		welcome.term = frame.term;
	}
	if (Array.isArray(frame.subordinates)) {
		welcome.subordinates = frame.subordinates
			.map(readFallbackServer)
			.filter((server): server is FallbackServer => server !== null);
	}
	if (frame.diverged !== undefined) {
		const diverged = readDiverged(frame.diverged);
		if (!diverged) throw protocolError('A malformed welcome');
		if (diverged.length > 0) welcome.diverged = diverged;
	}

	return welcome;
}

/** An error frame as an error; a code this client does not know reads as `internal`, a temporary error. */
function readErrorFrame(frame: Frame): WorkspaceError {
	const code = isServerErrorCode(frame.code) ? frame.code : 'internal';
	const message =
		typeof frame.message === 'string' ? frame.message : 'The server reported an error';

	return new WorkspaceError(message, code);
}

/** A `stale` answer, or `null` for any other frame. */
function readStale(frame: Frame): { value: Stale } | null {
	if (frame.t !== 'stale') return null;
	if (!isRevision(frame.revision)) throw protocolError('A malformed stale');
	return { value: { stale: frame.revision } };
}

/** Whether a list or get answered `Stale`. */
export function isStale(value: object): value is Stale {
	return 'stale' in value;
}

/**
 * Reads the app's whole set, page by page, all of one revision, through
 * `list`; starts over when the set changes between pages, a few times at
 * most, and then fails with `conflict`.
 */
export async function listAll(client: Pick<ServerClient, 'list'>): Promise<Listing> {
	for (let attempt = 0; attempt < LIST_ATTEMPTS; attempt++) {
		const first = await client.list();
		if (isStale(first)) continue;
		const listing: Listing = { revision: first.revision, docs: first.docs, removed: first.removed };
		let cursor = first.cursor;
		let stale = false;
		while (cursor !== undefined) {
			const page = await client.list({ revision: listing.revision, cursor });
			if (isStale(page)) {
				stale = true;
				break;
			}
			listing.docs = [...listing.docs, ...page.docs];
			listing.removed = [...listing.removed, ...page.removed];
			cursor = page.cursor;
		}
		if (!stale) return listing;
	}

	throw new WorkspaceError('The app’s set kept changing while it was listed', 'conflict');
}

/**
 * The workspace-server protocol over one open data channel, as its
 * `docs/protocol.md` ("Messages") lays it out. One request is in flight at a
 * time; a later call waits for the one before it. Not a singleton: one per
 * connection, closed and dropped with it.
 */
export class ServerClient {
	private readonly framing: ChannelFraming;
	private pending: Pending | null = null;
	/** Settles when the request before the next one has; requests run in this order. */
	private tail: Promise<unknown> = Promise.resolve();
	private ended: WorkspaceError | null = null;
	private readonly changedListeners = new Set<(revision: string | null) => void>();
	private readonly divergedListeners = new Set<(servers: DivergedServer[]) => void>();
	private readonly resolvedListeners = new Set<(server: string) => void>();
	private readonly closedListeners = new Set<(error: WorkspaceError) => void>();
	private readonly closeWaiters = new Set<() => void>();

	private readonly onChannelClose = (): void => {
		this.end(new WorkspaceError('The connection to the server closed', 'closed'), true);
	};

	private readonly app: string;

	constructor(
		private readonly channel: FrameChannel,
		options: ServerClientOptions,
	) {
		this.app = options.app;
		this.framing = new ChannelFraming(
			channel,
			(incoming) => this.receive(incoming),
			(error) => {
				this.end(error, true);
				channel.close();
			},
			(frame) => (frame.t === 'doc' && this.pending?.receive?.(frame)) || null,
		);
		channel.addEventListener('close', this.onChannelClose);
	}

	hello(device: string, token: string): Promise<Welcome> {
		return this.request(
			{ t: 'hello', proto: PROTOCOL_VERSION, app: this.app, device, token },
			({ frame }) => (frame.t === 'welcome' ? { value: readWelcome(frame) } : null),
		);
	}

	/**
	 * Pairs with a one-time secret. Resolves once `welcome` follows `paired`;
	 * the server counts the pairing as confirmed only when this session sends
	 * its next frame, so the caller sends one at once (normally `head`).
	 */
	pair(secret: string, name: string): Promise<{ device: string; token: string; welcome: Welcome }> {
		let paired: { device: string; token: string } | null = null;

		return this.request(
			{ t: 'pair', proto: PROTOCOL_VERSION, app: this.app, secret, name },
			({ frame }) => {
				if (frame.t === 'paired' && !paired) {
					if (typeof frame.device !== 'string' || typeof frame.token !== 'string') return null;
					paired = { device: frame.device, token: frame.token };
					return 'wait';
				}

				if (frame.t !== 'welcome' || !paired) return null;
				return { value: { ...paired, welcome: readWelcome(frame) } };
			},
		);
	}

	/** The revision of the app's set, `null` for an empty set. */
	head(): Promise<string | null> {
		return this.request({ t: 'head' }, ({ frame }) =>
			frame.t === 'head' && isRevision(frame.revision) ? { value: frame.revision } : null,
		);
	}

	/**
	 * One page of the app's set: the first when `cursor` is absent. A
	 * `revision` in `options` (`null` for an empty set) asks for the page at
	 * that revision only; a next page names the revision of the page that
	 * gave its cursor. A set no longer at that revision answers `Stale`.
	 * `listAll` reads every page.
	 */
	list(options: { revision?: string | null; cursor?: string } = {}): Promise<ListPage | Stale> {
		const frame: Frame = { t: 'list' };
		if ('revision' in options) frame.revision = options.revision ?? null;
		if (options.cursor !== undefined) frame.cursor = options.cursor;

		return this.request<ListPage | Stale>(frame, ({ frame: reply }) => {
			const stale = readStale(reply);
			if (stale) return stale;
			if (reply.t !== 'list') return null;
			const page = readPage(reply);
			if (!page) throw protocolError('A malformed list');
			return { value: page };
		});
	}

	/** The app's whole set: see `listAll`. */
	listAll(): Promise<Listing> {
		return listAll(this);
	}

	/**
	 * Reads the document `key`, from `options.offset` on. A document that is
	 * not there (with the etag asked for) answers `Stale`, and nothing is
	 * read. An `onPiece` that throws gets nothing more; `get` fails with its
	 * error once the rest of the body has arrived, and the client stays usable.
	 */
	get(key: DocumentKey, options: GetOptions = {}): Promise<ReadDocument | Stale> {
		const { etag = null, offset = 0, onPiece } = options;
		if (!isCount(offset) || (offset > 0 && etag === null)) {
			return Promise.reject(
				new RangeError('An offset past 0 needs the etag of the body it resumes'),
			);
		}
		const frame: Frame = { t: 'get', path: key.path, id: key.id, type: key.type };
		if (etag !== null) frame.etag = etag;
		if (offset > 0) frame.offset = offset;

		let failure: PieceFailure | null = null;
		const receive = onPiece
			? (piece: Uint8Array): void => {
					if (failure) return;
					try {
						onPiece(piece);
					} catch (error) {
						failure = new PieceFailure(error);
					}
				}
			: null;

		return this.request<ReadDocument | Stale>(
			frame,
			({ frame: reply, body }) => {
				const stale = readStale(reply);
				if (stale) return stale;
				if (reply.t !== 'doc' || !body) return null;
				const got = readKey(reply);
				const replyOffset = reply.offset ?? 0;
				if (
					!got ||
					!sameKey(got, key) ||
					typeof reply.etag !== 'string' ||
					(etag !== null && reply.etag !== etag) ||
					!isCount(reply.bytes) ||
					replyOffset !== offset
				) {
					throw protocolError('A doc that is not the one asked for');
				}
				if (failure) throw failure;
				return {
					value: { ...key, etag: reply.etag, bytes: reply.bytes, offset, body },
				};
			},
			undefined,
			receive && (() => receive),
		);
	}

	/**
	 * Opens the transaction `tx` (16 to 64 of `[A-Za-z0-9_-]`, chosen by the
	 * client), or returns to it, and reports what it holds. It discards this
	 * device's other open transaction on the app.
	 */
	begin(tx: string): Promise<Began> {
		return this.request({ t: 'begin', tx }, ({ frame }) => {
			if (frame.t !== 'began') return null;
			const staged = readStaged(frame.staged);
			if (frame.tx !== tx || !staged) throw protocolError('A malformed began');
			return { value: { tx, staged } };
		});
	}

	/**
	 * Stages `body` for `key` in the transaction `tx`, its bytes past `offset`
	 * sent as the channel takes them (0 starts the body afresh, a larger one
	 * resumes it from what `begin` said was received, and the body's whole
	 * size only changes `ifEtag`). `ifEtag` is the etag `key` must have at
	 * the commit, `null` if it must not exist. The server answers once the
	 * whole body has arrived, so a refusal (`quota`, say) comes after it.
	 */
	put(
		tx: string,
		key: DocumentKey,
		ifEtag: string | null,
		body: Body,
		offset = 0,
	): Promise<StagedAck> {
		if (!isCount(offset) || offset > bodySize(body)) {
			return Promise.reject(
				new RangeError(`An offset of ${offset} into a body of ${bodySize(body)} bytes`),
			);
		}
		return this.request(
			{ t: 'put', tx, path: key.path, id: key.id, type: key.type, ifEtag },
			(incoming) => this.readStagedAck(incoming, key),
			{ body, offset },
		);
	}

	/** Stages the removal of `key` in the transaction `tx`; `ifEtag` is the etag it must have then. */
	delete(tx: string, key: DocumentKey, ifEtag: string | null): Promise<StagedAck> {
		return this.request(
			{ t: 'delete', tx, path: key.path, id: key.id, type: key.type, ifEtag },
			(incoming) => this.readStagedAck(incoming, key),
		);
	}

	/**
	 * Applies the transaction `tx`, all of it or nothing. A conflict is no
	 * error: the result is not `committed`, and the transaction stays open.
	 * An error after the server's commit point (`internal`) may leave the
	 * commit applied: list again; `begin` with the same `tx` tells whether it
	 * is still open.
	 */
	commit(tx: string): Promise<CommitResult> {
		return this.request({ t: 'commit', tx }, ({ frame }) => {
			if (frame.t !== 'committed' && frame.t !== 'conflict') return null;
			const docs = readDocumentEtags(frame.docs);
			if (!isRevision(frame.revision) || !docs) throw protocolError(`A malformed ${frame.t}`);
			return { value: { committed: frame.t === 'committed', revision: frame.revision, docs } };
		});
	}

	/** Discards the transaction `tx`. */
	abort(tx: string): Promise<{ tx: string }> {
		return this.request({ t: 'abort', tx }, ({ frame }) => {
			if (frame.t !== 'aborted') return null;
			if (frame.tx !== tx) throw protocolError('An aborted of another transaction');
			return { value: { tx } };
		});
	}

	/**
	 * Settles a divergence of the app's servers: `server` is the subordinate's
	 * server id, `keep` the side whose copy stays. The `onResolved` listeners
	 * hear the answer too.
	 */
	resolve(server: string, keep: Keep): Promise<{ server: string }> {
		return this.request({ t: 'resolve', server, keep }, ({ frame }) => {
			if (frame.t !== 'resolved') return null;
			if (frame.server !== server) throw protocolError('A resolved of another server');
			return { value: { server } };
		});
	}

	/**
	 * Asks the server to delete this device. Resolves once the server has
	 * closed the channel, or after a short wait — the device forgets the
	 * pairing either way.
	 */
	async forget(): Promise<void> {
		const turn = this.tail.then(async () => {
			if (this.ended) return;
			const closed = new Promise<void>((resolve) => {
				const finish = (): void => {
					clearTimeout(timer);
					this.closeWaiters.delete(finish);
					resolve();
				};
				const timer = setTimeout(finish, FORGET_WAIT_MS);
				this.closeWaiters.add(finish);
			});
			this.framing.sendFrame({ t: 'forget' });
			await closed;
		});
		this.tail = turn.catch(() => undefined);
		await turn;
	}

	/**
	 * `changed` pushes: the app's set has a new revision (`null`: it is
	 * empty), written by another device or by the server. Several may come
	 * in a row, an older one after a newer; act on the latest. Returns an
	 * unsubscribe.
	 */
	onChanged(listener: (revision: string | null) => void): () => void {
		this.changedListeners.add(listener);
		return () => this.changedListeners.delete(listener);
	}

	/**
	 * `diverged` pushes: the servers whose copy of the app diverged from
	 * their main's, which wait for a device's choice. Returns an unsubscribe.
	 */
	onDiverged(listener: (servers: DivergedServer[]) => void): () => void {
		this.divergedListeners.add(listener);
		return () => this.divergedListeners.delete(listener);
	}

	/**
	 * `resolved`: the divergence of a server has been settled, by this
	 * device's `resolve` or another's. Returns an unsubscribe.
	 */
	onResolved(listener: (server: string) => void): () => void {
		this.resolvedListeners.add(listener);
		return () => this.resolvedListeners.delete(listener);
	}

	/**
	 * Called once when the channel closes or breaks under this client — never
	 * for `close()`, which the caller asked for. Returns an unsubscribe.
	 */
	onClosed(listener: (error: WorkspaceError) => void): () => void {
		this.closedListeners.add(listener);
		return () => this.closedListeners.delete(listener);
	}

	close(): void {
		this.end(new WorkspaceError('The connection was closed', 'closed'), false);
		this.channel.close();
	}

	private readStagedAck({ frame }: Incoming, key: DocumentKey): Reply<StagedAck> {
		if (frame.t !== 'staged') return null;
		const got = readKey(frame);
		if (!got || !sameKey(got, key) || !isCount(frame.received)) {
			throw protocolError('A staged of another document');
		}
		return { value: { ...key, received: frame.received } };
	}

	/**
	 * Queues a request: sends `frame` (and `body`, if any) once every earlier
	 * request has settled, and resolves with the value `read` finds in a reply.
	 */
	private request<T>(
		frame: Frame,
		read: (incoming: Incoming) => Reply<T>,
		body?: { body: Body; offset: number },
		receive: Pending['receive'] = null,
	): Promise<T> {
		const run = (): Promise<T> =>
			new Promise<T>((resolve, reject) => {
				if (this.ended) {
					reject(this.ended);
					return;
				}

				const pending: Pending = {
					kind: frame.t,
					handle: (incoming) => {
						const reply = read(incoming);
						if (reply === null) throw protocolError(`Unexpected ${incoming.frame.t}`);
						if (reply === 'wait') return false;
						resolve(reply.value);
						return true;
					},
					reject,
					receive,
				};
				this.pending = pending;

				const sending = body
					? this.framing.sendFrameWithBody(frame, body.body, body.offset)
					: Promise.resolve().then(() => this.framing.sendFrame(frame));
				sending.catch((error: unknown) => {
					if (this.pending !== pending) return;
					this.pending = null;
					if (error instanceof WorkspaceError) {
						reject(error);
						return;
					}
					// A body that could not be read to its announced size: the
					// server would take the next frame for the rest of it.
					reject(error);
					this.end(new WorkspaceError('The body could not be read to its end', 'closed'), true);
					this.channel.close();
				});
			});

		const result = this.tail.then(run, run);
		this.tail = result.catch(() => undefined);

		return result;
	}

	private receive(incoming: Incoming): void {
		const { frame } = incoming;
		const pending = this.pending;

		try {
			switch (frame.t) {
				case 'changed':
					if (!isRevision(frame.revision)) throw protocolError('A malformed changed');
					for (const listener of [...this.changedListeners]) listener(frame.revision);
					return;
				case 'diverged': {
					const servers = readDiverged(frame.servers);
					if (!servers) throw protocolError('A malformed diverged');
					for (const listener of [...this.divergedListeners]) listener(servers);
					return;
				}
				case 'resolved':
					if (typeof frame.server !== 'string') throw protocolError('A malformed resolved');
					for (const listener of [...this.resolvedListeners]) listener(frame.server);
					if (pending?.kind !== 'resolve') return; // a push
					break;
				case 'error':
					if (!pending) return;
					this.pending = null;
					pending.reject(readErrorFrame(frame));
					return;
			}

			if (!pending) throw protocolError(`Unexpected ${frame.t}`);
			if (pending.handle(incoming)) this.pending = null;
		} catch (error) {
			if (error instanceof PieceFailure && pending && this.pending === pending) {
				// A get's onPiece failed: the client stays usable.
				this.pending = null;
				pending.reject(error.error);
				return;
			}
			this.end(error instanceof WorkspaceError ? error : protocolError('A malformed reply'), true);
			this.channel.close();
		}
	}

	/** Ends the client once: rejects what waits, and tells the listeners if asked to. */
	private end(error: WorkspaceError, notify: boolean): void {
		if (this.ended) return;
		this.ended = error;
		this.framing.dispose();
		this.channel.removeEventListener('close', this.onChannelClose);

		const pending = this.pending;
		this.pending = null;
		pending?.reject(error);

		for (const finish of [...this.closeWaiters]) finish();
		this.closeWaiters.clear();

		if (notify) {
			for (const listener of [...this.closedListeners]) listener(error);
		}
	}
}
