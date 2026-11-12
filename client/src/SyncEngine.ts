import { browserDeviceName } from './deviceName.js';
import { openServerChannel } from './openServerChannel.js';
import type { FallbackServer, PairingCode, ServerTarget } from './PairingCode.js';
import type { Pairing, PairingStore } from './PairingStore.js';
import { ServerClient, type Welcome } from './ServerClient.js';
import { DocumentError, WorkspaceError, type WorkspaceErrorCode } from './WorkspaceError.js';

export type SyncStatus =
	'unpaired' | 'connecting' | 'syncing' | 'connected' | 'unreachable' | 'paused' | 'error';

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
}

/** The part of `ServerClient` the engine uses, so tests can stand in a fake server. */
export type SyncClient = Pick<
	ServerClient,
	'hello' | 'pair' | 'head' | 'get' | 'put' | 'forget' | 'onChanged' | 'onClosed' | 'close'
>;

/** The server's document as read: its etag, `null` with an empty body when there is none yet. */
export interface ServerDocument {
	etag: string | null;
	body: Uint8Array;
}

/** What one sync does with the server's document, as the app's adapter decides. */
export interface SyncPlan {
	/** Brings the local state up to the merge; leave it out when the local state already is. */
	apply?: () => Promise<void>;
	/** The merged document to write, or `null` when the server already holds all of it. */
	write: Uint8Array | null;
}

/**
 * Everything the engine needs to know about an app's document. The engine
 * decides when to sync and talks to the server; the adapter decides what
 * the merged document is.
 */
export interface SyncAdapter {
	/**
	 * A counter that moves on every local change. A device whose document
	 * the server already holds, and whose count has not moved since its last
	 * sync, skips reading the document again.
	 */
	changeCount(): number;
	/**
	 * Merges the server's document with the local state.
	 *
	 * It runs synchronously, right after the engine reads `changeCount`: read
	 * the local state here, not in `apply`, so an edit made after the merge
	 * moves the count past what was synced and the next sync picks it up.
	 *
	 * Throw a `DocumentError` for a document this app cannot read (a newer
	 * format, a damaged one): sync halts, and nothing is written over it.
	 * Two devices settle only if both merge the same way, so on a tie the
	 * server's copy should win: every device then agrees without writing back
	 * and forth. And write only when the server lacks something: a write that
	 * local housekeeping alone causes (pruning, garbage collection) lets two
	 * devices with different settings undo each other's writes forever.
	 */
	plan(server: ServerDocument, now: Date): SyncPlan;
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
/** A last line of defence against a write loop: past this, writing stops until the app reopens. */
export const MAX_WRITES_PER_MINUTE = 10;
/** How many times one sync re-reads and re-merges after another device wrote first. */
export const MAX_CONFLICT_RETRIES = 3;
/**
 * How long, from the start of a connection, a pairing with fallback servers
 * waits for its main once another server has answered. A connection opens in
 * well under a second; a server that is away holds an attempt until it times
 * out, which is why no attempt waits for another.
 */
export const MAIN_GRACE_MS = 2000;

const WRITE_WINDOW_MS = 60_000;

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
 * device id, the token and `lastEtag` stay: every server of the family
 * admits the same device, and an etag is the document's hash.
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
 * still in flight on it. A write is always conditional on the etag read, and
 * a lost race re-reads and re-merges.
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
	private state: SyncState<string> = {
		status: 'unpaired',
		serverName: null,
		via: null,
		lastSyncAt: null,
		error: null,
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

	async syncNow(): Promise<void> {
		this.retryIndex = 0;
		await this.trigger();
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
			lastEtag: null,
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

		this.attach(client);
		this.ready = true;
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
		const generation = this.generation;
		const current = (): boolean => generation === this.generation;

		this.setState({ status: 'syncing', error: null });
		try {
			const head = await client.head();
			if (!current()) return;
			if (head === pairing.lastEtag && this.syncedCount === this.adapter.changeCount()) {
				this.setState({ status: 'connected' });
				return;
			}

			for (let attempt = 0; ; attempt++) {
				const doc = await client.get();
				if (!current()) return;

				const now = this.now();
				// Read with the merge, with no await between: the adapter reads the
				// local state in the same tick, so an edit made after this is
				// counted past it and the next sync picks it up.
				const count = this.adapter.changeCount();
				const plan = this.adapter.plan(doc, now);
				if (plan.apply) await plan.apply();
				if (!current()) return;

				let etag = doc.etag;
				if (plan.write) {
					this.guardWrite(now);
					const put = await client.put(doc.etag, plan.write);
					if (!current()) return;
					if (!put.stored) {
						if (attempt >= MAX_CONFLICT_RETRIES) {
							throw new WorkspaceError('Another device kept writing first', 'conflict');
						}
						continue;
					}
					etag = put.etag;
				}

				this.syncedCount = count;
				this.savePairing({ lastEtag: etag, lastSyncAt: now.toISOString() });
				this.retryIndex = 0;
				this.setState({ status: 'connected', error: null });
				return;
			}
		} catch (error) {
			if (!current()) return;
			this.fail(asWorkspaceError(error));
		}
	}

	/** Counts a write, refusing it past the limit. */
	private guardWrite(now: Date): void {
		const time = now.getTime();
		this.writeTimes = this.writeTimes.filter((at) => time - at < WRITE_WINDOW_MS);
		if (this.writesStopped || this.writeTimes.length >= MAX_WRITES_PER_MINUTE) {
			this.writesStopped = true;
			console.warn(
				`Server sync stopped writing: more than ${MAX_WRITES_PER_MINUTE} writes within a minute`,
			);
			throw new WorkspaceError('Too many writes within a minute', 'tooManyWrites');
		}
		this.writeTimes.push(time);
	}

	private fail(error: WorkspaceError<string>): void {
		if (error instanceof DocumentError || HALTING.has(error.code)) {
			this.halted = error;
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

		// A refusal retrying will not cure by itself (tooLarge, notJson, a
		// conflict that kept recurring…): say so, and try again on the next
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

	private savePairing(patch: Partial<Pairing>): void {
		if (!this.pairing) return;
		this.pairing = { ...this.pairing, ...patch };
		this.store.save(this.pairing);
		this.setState({});
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
		this.setState({});
	}

	private setState(patch: Partial<SyncState<string>>): void {
		this.state = {
			...this.state,
			...patch,
			serverName: this.pairing?.name ?? null,
			via: this.via,
			lastSyncAt: this.pairing?.lastSyncAt ?? null,
		};
		this.onState({ ...this.state } as SyncState<DocCode>);
	}
}
