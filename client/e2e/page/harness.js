// A device for the e2e tests: the built package, driven the way an app
// drives it. Its document is {"notes": [...]}, merged as a union.
import {
	ServerClient,
	SyncEngine,
	WorkspaceError,
	createPairingStore,
	openServerChannel,
	parsePairingCode,
} from '/dist/index.js';

const encode = (notes) => new TextEncoder().encode(JSON.stringify({ notes }));
const decode = (body) =>
	body.byteLength === 0 ? [] : JSON.parse(new TextDecoder().decode(body)).notes;
const same = (a, b) => a.length === b.length && a.every((note, i) => note === b[i]);

/** A device holding `notes`, with a sync engine for the app `todo`. */
export function createDevice(notes) {
	const state = {
		notes: [...notes].sort(),
		changeCount: 0,
		status: 'unpaired',
		via: null,
		error: null,
	};
	const engine = new SyncEngine({
		app: 'todo',
		pairingStore: createPairingStore({ key: 'e2e.server' }),
		adapter: {
			changeCount: () => state.changeCount,
			plan(server) {
				const remote = [...decode(server.body)].sort();
				const merged = [...new Set([...remote, ...state.notes])].sort();

				return {
					apply: same(merged, state.notes)
						? undefined
						: async () => {
								state.notes = merged;
							},
					write: same(merged, remote) ? null : encode(merged),
				};
			},
		},
		onState: (next) => {
			state.status = next.status;
			state.via = next.via;
			state.error = next.error?.code ?? null;
		},
	});
	engine.start(true);

	return {
		state,
		async pair(code) {
			try {
				await engine.pair(parsePairingCode(code));
				return 'paired';
			} catch (error) {
				return error instanceof WorkspaceError ? error.code : String(error);
			}
		},
		add(note) {
			state.notes = [...state.notes, note].sort();
			state.changeCount++;
			engine.localChanged();
		},
		/** The app hidden and shown again: the engine reconnects. */
		reconnect() {
			engine.setVisible(false);
			engine.setVisible(true);
		},
	};
}

/** Pairs with the protocol client alone, for `app`; returns `paired` or the error code. */
export async function pairAs(app, code) {
	const target = parsePairingCode(code);
	const { channel, close } = await openServerChannel(target);
	try {
		await new ServerClient(channel, { app }).pair(target.secret, 'e2e');
		return 'paired';
	} catch (error) {
		return error instanceof WorkspaceError ? error.code : String(error);
	} finally {
		close();
	}
}

/**
 * Pairs with the protocol client alone for `app`, and drives the documents
 * of its set: a body of `size` bytes from a Blob into a folder, read back
 * piece by piece and from an offset, then deleted. Returns what it saw, or
 * the error code.
 */
export async function documents(app, code, size) {
	const target = parsePairingCode(code);
	const { channel, close } = await openServerChannel(target);
	const client = new ServerClient(channel, { app });
	const key = { path: 'photos/2026', id: 'p1', type: 'bin' };
	const bytes = Uint8Array.from({ length: size }, (_, i) => (i * 7) % 251);
	const same = (a, b) => a.length === b.length && a.every((byte, i) => byte === b[i]);
	try {
		await client.pair(target.secret, 'e2e documents');
		const before = await client.head();

		const tx = 'e2e-tx-0123456789ab';
		const began = await client.begin(tx);
		const staged = await client.put(tx, key, null, new Blob([bytes]));
		const committed = await client.commit(tx);
		const etag = committed.docs[0].etag;

		const listing = await client.listAll();
		const listed = listing.docs.find((doc) => doc.path === key.path && doc.id === key.id);

		const pieces = [];
		const read = await client.get(key, { etag, onPiece: (piece) => pieces.push(piece.slice()) });
		const whole = new Uint8Array(pieces.reduce((n, piece) => n + piece.byteLength, 0));
		pieces.reduce((at, piece) => (whole.set(piece, at), at + piece.byteLength), 0);
		const tail = await client.get(key, { etag, offset: size - 10 });
		const stale = await client.get(key, { etag: '0'.repeat(64) });

		const tx2 = 'e2e-tx-0123456789cd';
		await client.begin(tx2);
		await client.delete(tx2, key, etag);
		const conflict = await client.put(
			tx2,
			{ path: '', id: 'other', type: 'txt' },
			'f'.repeat(64),
			new Uint8Array([1]),
		);
		const refused = await client.commit(tx2);
		await client.abort(tx2);
		await client.begin(tx2);
		await client.delete(tx2, key, etag);
		const deleted = await client.commit(tx2);
		const after = await client.listAll();

		return {
			before,
			began: began.staged.length,
			received: staged.received,
			committed: committed.committed,
			listedBytes: listed?.bytes ?? null,
			listedEtag: listed?.etag === etag,
			revision: listing.revision === committed.revision,
			streamed: same(whole, bytes) && read.body.byteLength === 0 && pieces.length > 1,
			tail: same(tail.body, bytes.subarray(size - 10)) && tail.offset === size - 10,
			stale: 'stale' in stale,
			conflictReceived: conflict.received,
			refused: refused.committed,
			deleted: deleted.committed && deleted.docs[0].etag === null,
			removed: after.removed.some((doc) => doc.path === key.path && doc.id === key.id),
			left: after.docs.some((doc) => doc.path === key.path),
		};
	} catch (error) {
		return error instanceof WorkspaceError ? error.code : String(error);
	} finally {
		client.close();
		close();
	}
}
