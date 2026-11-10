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
	const state = { notes: [...notes].sort(), changeCount: 0, status: 'unpaired', error: null };
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
