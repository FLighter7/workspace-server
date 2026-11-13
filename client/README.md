# @flighter/wss-client

The client for [workspace-server](https://github.com/FLighter7/workspace-server): a small program you run on your own computer that keeps each app's files on your disk and syncs that app's devices over WebRTC on your local network.

This package is everything a TypeScript web app needs to sync through it:

- reading the **pairing code** the server's page shows, as a QR code or as text;
- reaching the server over a **WebRTC data channel with no signaling**: the device builds the server's side of the connection itself from the pairing;
- the **protocol**: hello, pair, head, list, get, begin, put, delete, commit, abort, resolve and forget, and the `changed`, `diverged` and `resolved` pushes, with bodies streamed in pieces from a `Blob` or to a callback and resumed from an offset;
- a **sync engine** that decides when to connect, when to sync and when to write, retries what can be retried, and stops on what cannot, and that syncs through a **fallback server** (a subordinate of the user's main server, such as a laptop's) when the main is out of reach;
- **test doubles** for the app's own tests.

Your app supplies one thing: how to merge its document. The engine syncs one document of the app, `data.json` (`SYNC_DOCUMENT`); a later release brings an engine for an app that keeps several documents. An app can already drive several itself through `ServerClient` (see "The protocol client").

The wire contract is the server's [docs/protocol.md](https://github.com/FLighter7/workspace-server/blob/main/docs/protocol.md). This package speaks `proto` 1.

## Installing

```sh
pnpm add @flighter/wss-client
```

It is ESM only, ships its TypeScript types, and has no dependencies. It runs in browsers: the connection uses `RTCPeerConnection`, and the default pairing store uses `localStorage`.

## Using it

```ts
import {
	DocumentError,
	SyncEngine,
	createPairingStore,
	parsePairingCode,
	startInPage,
	type SyncAdapter,
} from '@flighter/wss-client';

const adapter: SyncAdapter = {
	// Moves on every local edit.
	changeCount: () => notes.changeCount,
	plan(server, now) {
		const remote = decode(server.body); // throws DocumentError for a document it cannot read
		const local = notes.all(); // read here, in the same tick as changeCount
		const merged = merge(remote, local); // the server's copy wins a tie

		return {
			apply: sameAs(merged, local) ? undefined : () => notes.replaceAll(merged),
			write: sameAs(merged, remote) ? null : encode(merged, now),
		};
	},
};

const engine = new SyncEngine<'newerSchema' | 'badDocument'>({
	app: 'notes', // the app's id on the server
	pairingStore: createPairingStore({ key: 'notes.server' }),
	adapter,
	onState: (state) => render(state), // status, server name, last sync, error
});

startInPage(engine); // connects while the page is on screen

// When the user scans or pastes the code from the server's page:
await engine.pair(parsePairingCode(text));

// After every local edit:
engine.localChanged();
```

`engine.syncNow()` syncs at once; `engine.disconnect()` asks the server to forget this device and forgets the pairing here.

### The adapter

`plan` runs once per sync with the server's document (`etag` and `body`; an empty body and a `null` etag when there is none yet). It returns what to change locally (`apply`) and what to write (`write`, or `null`). The engine writes in a transaction conditional on the etag it read; if another device wrote first, it reads and merges again, up to three times. A sync that finds the app's set at the revision of its last sync, with no local change since, reads nothing; one that finds only other documents of the set changed reads nothing either.

Rules that keep devices from writing back and forth:

- **Read the local state in `plan` itself**, not in `apply`. The engine reads `changeCount` just before calling `plan`, with no await between, so an edit made afterwards is counted past it and synced next time.
- **Let the server's copy win a tie.** Every device then settles on the same result.
- **Write only what the server lacks.** A write caused only by local housekeeping (pruning, garbage collection) makes two devices with different settings undo each other's writes forever.
- **Throw a `DocumentError` for a document this build cannot read**: a newer format, or a damaged one. Sync halts, and nothing is written over it. Its code is yours to name and translate.

### Fallback servers

A server can be linked to the user's main server as its **subordinate**: a laptop that keeps the main's data on its own disk and travels with the user. The device still pairs only with the main; the main lists its subordinates in every `welcome`, and the engine keeps them in the pairing record (`Pairing.fallbacks`, optional, so a record saved before reads as having none). Your app has nothing to do for it.

- **Connecting:** with fallbacks known, the engine connects to the main and every fallback at once. It takes the main if it answers within `MAIN_GRACE_MS` (2 s) of the start, and otherwise a fallback that answered (a main before a subordinate, then the list's order); the others are closed. It stays with that server until the connection ends or the page is hidden and shown again, which tries the main first again.
- **The same pairing:** the device id, the token, `lastEtag` and `lastRevision` are shared by every server: a subordinate admits the main's devices, an etag is the document's hash, and a revision the hash of the set's keys and etags, the same on every server holding the same files.
- **Errors:** only the main's refusals stop sync. A subordinate that does not know this device yet (it was paired since the subordinate last reached the main) refuses it with `badToken`, and the engine carries on as though that server were away. A `welcome` from a server that is neither the main nor a known fallback is not taken; from the main's own address it is `serverChanged`.
- **Keeping the list current:** each `welcome` from the main replaces the list; a subordinate's `welcome` carries none and leaves it as it is, but refreshes that fallback's own hosts and name, so a laptop met on a network the main never saw is reached at its new address next time.
- **Following the main:** a subordinate can take the role of main over (the server's guide, "A laptop as a second server"). The main's family then has a higher `term`, which every `welcome` names (`Welcome.term`). When a fallback answers as the main at a term at least the one the paired server last named (`Pairing.term`, optional, so a record saved before reads as 0), the engine makes it the server the device is paired with, and the one it paired with before a fallback: the pairing follows the family's main through every switch, and only the new main's refusals stop sync. Of two mains answering at once (a family split for a while), it takes the higher term, unless the paired server answers as a main first. The device id, the token, `lastEtag` and `lastRevision` stay.
- **Showing it:** `SyncState.via` is the name of the fallback the connection goes to, `null` while it goes to the main.
- **Pairing and disconnecting** go to the main: a subordinate refuses `pair` with `notMain`. `disconnect()` while connected to a subordinate forgets the pairing here, but the main keeps the device until it is removed on the main's page.

### States and errors

`onState` receives `{ status, serverName, via, lastSyncAt, error }`. `serverName` is the main's name, and `via` the fallback's, when the connection goes to one. `status` is one of `unpaired`, `connecting`, `syncing`, `connected`, `unreachable`, `paused`, `error`. `error` is a `WorkspaceError` (or your `DocumentError`); its `message` is English for a developer, and its `code` is what an interface translates.

| Code                                                                                      | What the engine does                                                                                      |
| ----------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| `connectTimeout`, `closed`                                                                | `unreachable`: retries after 2 s, 5 s, 15 s, 30 s, 1 min, then every 5 min, while the page is visible     |
| `blocked`                                                                                 | `paused`: the server blocks this address after failed attempts; retries once a minute, the pairing stands |
| `internal`, and any code it does not know                                                 | `paused`: retries on the same schedule as `unreachable`                                                   |
| `badToken`, `unknownApp`, `serverChanged`, `protocol`, `tooManyWrites`, a `DocumentError` | `error`: stops until the device pairs again or the app starts again                                       |
| `quota`, `badPath`, `incomplete`, `conflict`, `notAuthenticated`                          | `error`: tries again on the next edit, push or `syncNow`                                                  |

A write that fails with `noTransaction` or `internal` (which may come after the server's commit point, with the write applied) is not repeated as such: the engine lists and reads again, and merges, within the same three attempts; past them, the error is handled as the table says.

`pair` rethrows its failure for the page to show: `badSecret` (a wrong, expired or used code), `unknownApp` (the server offers to add the app; try again after), `notMain` (the code is a subordinate's: pair with its main), `blocked`, or a connection failure. `parsePairingCode` throws `notPairingCode`, `tooNew` (the code comes from a newer server) or `damaged`.

## The protocol client

`ServerClient` speaks the protocol over an open data channel (`openServerChannel` opens one) for an app that drives its documents itself. A document is a `DocumentKey`, `{ path, id, type }`: the server keeps it as `<path>/<id>.<type>` in the app's folder, `path` being `''` for the app's root.

- `head()` is the set's revision; `list()` one page of the set (`docs`, `removed` tombstones, `cursor`), `listAll()` every page of one revision. The `changed` push (`onChanged`) names the new revision.
- `get(key, { etag, offset, onPiece })` reads a document at the etag a listing named: a document that changed since answers `{ stale: revision }` (`isStale`), and the app lists again. `onPiece` takes the body as it arrives, so a large one never sits whole in memory; `offset` resumes a read.
- Writes go in a transaction: `begin(tx)` (a client-chosen id, 16 to 64 of `[A-Za-z0-9_-]`; it returns to a transaction still open and reports what it holds, for resuming), `put(tx, key, ifEtag, body, offset)` with a `Uint8Array` or a `Blob` read a slice at a time, `delete(tx, key, ifEtag)`, then `commit(tx)`, all of it or nothing: `{ committed: false }` is a conflict, with the current etags, and leaves the transaction open; `abort(tx)` drops it.
- `resolve(server, keep)`, `onDiverged` and `onResolved` are for a divergence of the user's servers: when a subordinate's copy of the app and its main's both changed apart, the `welcome` (`diverged`) and the `diverged` push list the subordinates whose copy waits for a choice, and `resolve` keeps the main's copy (`'main'`) or the subordinate's (`'subordinate'`), the whole set; the first choice on any server wins, and every choice is answered `resolved`. Ask with both servers' names (roles move with a switch of the main), and keep `since` as the string received: it names the divergence. `SyncEngine` does not surface it yet: an app that wants to ask its user uses `ServerClient`.

## Testing an app

`@flighter/wss-client/testing` has a `FakeServer`: pass its `connect` to the engine, and several engines can share one, standing in for several devices. It holds an app's set of documents in memory: `store(body)` writes `data.json` as another device would, `storeDocument` and `removeDocument` any other, and `body`, `etag`, `text()`, `revision`, `documents` and `puts` show what it holds and what was written. Its etags and revision depend only on the bytes, as the real server's do. Switches make it fail as a real one can (`connectError`, `helloError`, `pairError`, `conflicts`, `commitError` with `commitAppliedFirst`, `connectGate` for one slow to answer), and `log` records every call. `FakeChannel` stands in for a data channel under `ServerClient`.

For a main and its subordinates, give each `FakeServer` its own `serverId` and `port`, set `role`, list the subordinates' `entry()` in the main's `subordinates`, let them share the main's `devices`, and pass `FakeServer.route(main, laptop)` as `connect`. Set `term` for a family that switched its main.

```ts
import { FakeServer } from '@flighter/wss-client/testing';

const server = new FakeServer();
const engine = new SyncEngine({
	app: 'notes',
	pairingStore,
	adapter,
	onState,
	connect: server.connect,
});
```

## Licence

[CC BY-NC-ND 4.0](https://creativecommons.org/licenses/by-nc-nd/4.0/). Copyright (c) 2026-present Alexander Gerasimov (FLighter).
