import { ChannelFraming, type Frame, type FrameChannel, type Incoming } from './ChannelFraming.js';
import { WorkspaceError, isServerErrorCode } from './WorkspaceError.js';

/** The protocol this client speaks; the server refuses any other with `protocol`. */
export const PROTOCOL_VERSION = 1;

/** How long `forget` waits for the server to close the channel before giving up on it. */
const FORGET_WAIT_MS = 2000;

export interface Welcome {
	serverId: string;
	name: string;
	/** The current document's etag, or `null` when there is none yet. */
	etag: string | null;
	/** The server's `.local` name first, then its current LAN IPv4 addresses. */
	hosts: string[];
}

export type PutResult = { stored: true; etag: string } | { stored: false; etag: string | null };

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
	handle(incoming: Incoming): boolean;
	reject(error: WorkspaceError): void;
}

function protocolError(message: string): WorkspaceError {
	return new WorkspaceError(message, 'protocol');
}

function isEtag(value: unknown): value is string | null {
	return value === null || typeof value === 'string';
}

function readWelcome(frame: Frame): Welcome {
	const { serverId, name, etag, hosts } = frame;
	if (
		typeof serverId !== 'string' ||
		typeof name !== 'string' ||
		!isEtag(etag) ||
		!Array.isArray(hosts) ||
		!hosts.every((host) => typeof host === 'string' && host.length > 0)
	) {
		throw protocolError('A malformed welcome');
	}

	return { serverId, name, etag, hosts: [...hosts] as string[] };
}

/** An error frame as an error; a code this client does not know reads as `internal`, a temporary error. */
function readErrorFrame(frame: Frame): WorkspaceError {
	const code = isServerErrorCode(frame.code) ? frame.code : 'internal';
	const message =
		typeof frame.message === 'string' ? frame.message : 'The server reported an error';

	return new WorkspaceError(message, code);
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
	private readonly changedListeners = new Set<(etag: string) => void>();
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

	head(): Promise<string | null> {
		return this.request({ t: 'head' }, ({ frame }) =>
			frame.t === 'head' && isEtag(frame.etag) ? { value: frame.etag } : null,
		);
	}

	get(): Promise<{ etag: string | null; body: Uint8Array }> {
		return this.request({ t: 'get' }, ({ frame, body }) =>
			frame.t === 'doc' && isEtag(frame.etag) && body
				? { value: { etag: frame.etag, body } }
				: null,
		);
	}

	/** Writes `body` if the server's document still has `ifEtag` (`null`: there is none yet). */
	put(ifEtag: string | null, body: Uint8Array): Promise<PutResult> {
		return this.request<PutResult>(
			{ t: 'put', ifEtag },
			({ frame: { t, etag } }) => {
				if (t === 'stored' && typeof etag === 'string') return { value: { stored: true, etag } };
				if (t === 'conflict' && isEtag(etag)) return { value: { stored: false, etag } };
				return null;
			},
			body,
		);
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

	/** `changed` pushes: another device stored a new document. Returns an unsubscribe. */
	onChanged(listener: (etag: string) => void): () => void {
		this.changedListeners.add(listener);
		return () => this.changedListeners.delete(listener);
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

	/**
	 * Queues a request: sends `frame` (and `body`, if any) once every earlier
	 * request has settled, and resolves with the value `read` finds in a reply.
	 */
	private request<T>(
		frame: Frame,
		read: (incoming: Incoming) => Reply<T>,
		body?: Uint8Array,
	): Promise<T> {
		const run = (): Promise<T> =>
			new Promise<T>((resolve, reject) => {
				if (this.ended) {
					reject(this.ended);
					return;
				}

				const pending: Pending = {
					handle: (incoming) => {
						const reply = read(incoming);
						if (reply === null) throw protocolError(`Unexpected ${incoming.frame.t}`);
						if (reply === 'wait') return false;
						resolve(reply.value);
						return true;
					},
					reject,
				};
				this.pending = pending;

				const sending = body
					? this.framing.sendFrameWithBody(frame, body)
					: Promise.resolve().then(() => this.framing.sendFrame(frame));
				sending.catch((error: unknown) => {
					if (this.pending !== pending) return;
					this.pending = null;
					reject(
						error instanceof WorkspaceError
							? error
							: new WorkspaceError('The connection to the server closed', 'closed'),
					);
				});
			});

		const result = this.tail.then(run, run);
		this.tail = result.catch(() => undefined);

		return result;
	}

	private receive(incoming: Incoming): void {
		const { frame } = incoming;

		if (frame.t === 'changed') {
			if (typeof frame.etag === 'string') {
				for (const listener of [...this.changedListeners]) listener(frame.etag);
			}
			return;
		}

		const pending = this.pending;
		if (frame.t === 'error') {
			if (!pending) return;
			this.pending = null;
			pending.reject(readErrorFrame(frame));
			return;
		}

		try {
			if (!pending) throw protocolError(`Unexpected ${frame.t}`);
			if (pending.handle(incoming)) this.pending = null;
		} catch (error) {
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
