# @flighter/wss-client

The client for [workspace-server](https://github.com/FLighter7/workspace-server): a small program you run on your own computer that keeps each app's files on your disk and syncs that app's devices over WebRTC on your local network.

This package is everything a TypeScript web app needs to sync through it:

- reading the **pairing code** the server's page shows, as a QR code or as text;
- reaching the server over a **WebRTC data channel with no signaling**: the device builds the server's side of the connection itself from the pairing;
- the **protocol**: hello, pair, head, list, get, begin, put, delete, commit, abort, resolve and forget, and the `changed`, `diverged` and `resolved` pushes, with bodies streamed in pieces from a `Blob` or to a callback and resumed from an offset;
- a **sync engine** for an app's set of documents: it decides when to connect, when to sync and when to write, retries what can be retried and stops on what cannot, resumes a transfer that was cut off, asks the app when the user's servers diverged, and syncs through a **fallback server** (a subordinate of the user's main server, such as a laptop's) when the main is out of reach;
- **test doubles** for the app's own tests.

Your app supplies one thing: what to read and how to merge it. An app keeps any number of documents on the server, plain files of any format, each named by a folder, an id and a type (`photos/2026/p1.jpg`).

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

const NOTES = { path: '', id: 'notes', type: 'json' }; // notes.json at the app's root

const adapter: SyncAdapter = {
	// Moves on every local edit.
	changeCount: () => notes.changeCount,
	// What to read of the listing: here the one document, whenever the set moved.
	select: () => [NOTES],
	plan(listing, read, now) {
		const remote = decode(read[0]?.body); // throws DocumentError for a document it cannot read
		const local = notes.all(); // read here, in the same tick as changeCount
		const merged = merge(remote, local); // the server's copy wins a tie

		return {
			apply: sameAs(merged, local) ? undefined : () => notes.replaceAll(merged),
			put: sameAs(merged, remote) ? [] : [{ doc: NOTES, body: encode(merged, now) }],
		};
	},
};

const engine = new SyncEngine<'newerSchema' | 'badDocument'>({
	app: 'notes', // the app's id on the server
	pairingStore: createPairingStore({ key: 'notes.server' }),
	adapter,
	onState: (state) => render(state), // status, server name, transfer, divergences, error
});

startInPage(engine); // connects while the page is on screen

// When the user scans or pastes the code from the server's page:
await engine.pair(parsePairingCode(text));

// After every local edit:
engine.localChanged();
```

`engine.syncNow()` syncs at once; `engine.disconnect()` asks the server to forget this device and forgets the pairing here. `resumeTransfer()`, `restartTransfer()` and `resolveDivergence()` answer the questions the state asks (below).

### How a sync goes

1. **`head`:** the set's revision. A sync that finds it at the revision of the last sync (`Pairing.lastRevision`), with no local change since, ends here.
2. **`list`**, page by page, all of one revision: every document with its etag (the SHA-256 of its bytes) and size, and the tombstones of deleted ones. A set that moves between pages is listed again.
3. **`select(listing)`:** the documents to read. The engine reads each at the etag the listing named; one that changed meanwhile is `stale`, and the sync lists again.
4. **`plan(listing, read, now)`:** what the merge changes here (`apply`) and on the server (`put` and `delete`).
5. **One transaction** writes the whole plan: each body conditional on the etag the listing named for its document (or on its absence), each delete on the etag listed. A body the server holds already (the same etag) and a delete of what it does not hold are left out, so a plan may name everything it has; a plan that changes nothing writes nothing. If another device wrote one of those documents first, the commit is a conflict, and the engine lists, reads and plans again, up to three times; the transaction stays open, so a body still wanted as it is goes no second time.
6. **`committed(docs)`**, optional: the etag each document the plan put now has, and `null` for each it deleted, once the server holds them: those the commit wrote, and those the server held as planned already (the same bytes, or no document to delete). A body the commit names with another etag than the one sent is left out, and the engine syncs again.

A commit checks only the documents it writes. When another device wrote other documents between the listing and the commit, the engine sees it (the revision the commit names is not the listing's plus its own changes) and syncs again, so the next `select` gets them.

### The adapter

`changeCount()`, `select(listing)` and `plan(listing, read, now)` run synchronously, so the local state is read in the same tick as the decision. `read` holds what `select` named, in its order (`{path, id, type, etag, bytes, body}`); a key the listing does not hold is left out. Bodies to write are a `Uint8Array` or a `Blob` (a `File` included), which is read a slice at a time, for hashing and for sending, never whole.

An app with one document selects it every time. An app with many keeps the etag of each document it holds, selects only those whose etag the listing names differently, and learns the etags of what it wrote from `committed`, so it never reads back its own writes. `documentPath(key)` gives a key's canonical form (`photos/2026/p1.jpg`), handy as a map key.

Rules that keep devices from writing back and forth:

- **Read the local state in `plan` itself**, not in `apply`. The engine reads `changeCount` just before calling `plan`, with no await between, so an edit made afterwards is counted past it and synced next time.
- **Let the server's copy win a tie.** Every device then settles on the same result.
- **Write only what the server lacks.** A write caused only by local housekeeping (pruning, garbage collection) makes two devices with different settings undo each other's writes forever.
- **Throw a `DocumentError` for a document this build cannot read**: a newer format, or a damaged one. Sync halts, and nothing is written over it. Its code is yours to name and translate.

### Transfers

`state.transfer` is the transfer under way, `{ direction, done, total, interrupted }`, or `null`: `'down'` for one sync's reads together, `'up'` for one transaction's bodies together, `done` and `total` in bytes.

- **Cut off** (the connection lost, the page hidden, the app reloaded), a transfer of at most `SMALL_TRANSFER_BYTES` (1 MiB) starts over by itself with the next sync, and a larger one with at most that much left goes on by itself.
- **A larger one with more left waits** for the user: `transfer.interrupted` is `true` and, once connected, `status` is `'interrupted'`; sync does nothing else meanwhile. Offer two buttons:
  - `resumeTransfer()` lists and plans again, and sends or reads only the rest: a body the transaction staged resumes from what the server received if the plan still writes the same bytes (the same etag), and a read resumes from where it stopped while the document keeps its etag. What changed meanwhile goes from the start.
  - `restartTransfer()` drops what was sent or read, and starts afresh.
- **A large upload survives a reload:** the engine keeps its transaction, the server it is open on and the etags of its bodies in the pairing record (`Pairing.upload`), saved when the upload starts, when it is cut off and once every body arrived. The next run asks as above when more than 1 MiB was left to send; otherwise it goes on by itself (a commit whose answer was lost, a conflict). The server keeps a transaction 24 hours after its last activity; on another server of the family (a fallback) the bodies go from the start. A read is kept in memory only: a reload starts it over.
- **Only what the server holds resumes:** the record names a body for a document only once the server holds that body's bytes or none, so an edit made meanwhile, or a put that never arrived, never leaves the old bytes under the new etag; `done` (bytes handed to the channel, up to 1 MiB ahead of what arrived) decides whether to ask, and the server's `began` where to resume.

### Divergence

The user's laptop can run a server linked to the main one (a subordinate). When both changed the app's documents while they could not reach each other, the two copies **diverged**: the servers merge nothing, and every device of the app is asked which copy to keep, whole. `state.diverged` lists what waits, `[{ serverId, name, since, mainName }]`: the subordinate's id and name, when it diverged, and the main's name.

- **Ask naming both servers**, "Keep {mainName}'s copy or {name}'s?": roles move with a switch of the main, so "main" and "subordinate" mean nothing to the user. `mainName` is the main the device last reached, or the server it is paired with.
- **`resolveDivergence(serverId, 'main' | 'subordinate')`** sends the choice, and resolves once the server answered; it rejects with `closed` while there is no connection. Requests on one connection go one at a time, so it waits for one in flight, a large upload's body included. The first choice on any server wins; a later one is answered all the same. Either way the entry leaves `state.diverged`, on this device and on every other connected one.
- **Afterwards** the copy not kept becomes the one kept, and the devices see it change like any other write, and merge as always.
- **Sync goes on** meanwhile: a device syncs with whichever server it reaches. The list stays while the connection is down, and the next server reached replaces it.

### Fallback servers

A server can be linked to the user's main server as its **subordinate**: a laptop that keeps the main's data on its own disk and travels with the user. The device still pairs only with the main; the main lists its subordinates in every `welcome`, and the engine keeps them in the pairing record (`Pairing.fallbacks`). Your app has nothing to do for it.

- **Connecting:** with fallbacks known, the engine connects to the main and every fallback at once. It takes the main if it answers within `MAIN_GRACE_MS` (2 s) of the start, and otherwise a fallback that answered (a main before a subordinate, then the list's order); the others are closed. It stays with that server until the connection ends or the page is hidden and shown again, which tries the main first again.
- **The same pairing:** the device id, the token and `lastRevision` are shared by every server: a subordinate admits the main's devices, and a revision is the hash of the set's keys and etags, the same on every server holding the same files.
- **Errors:** only the main's refusals stop sync. A subordinate that does not know this device yet (it was paired since the subordinate last reached the main) refuses it with `badToken`, and the engine carries on as though that server were away. A `welcome` from a server that is neither the main nor a known fallback is not taken; from the main's own address it is `serverChanged`.
- **Keeping the list current:** each `welcome` from the main replaces the list; a subordinate's `welcome` carries none and leaves it as it is, but refreshes that fallback's own hosts and name, so a laptop met on a network the main never saw is reached at its new address next time.
- **Following the main:** a subordinate can take the role of main over (the server's guide, "A laptop as a second server"). The main's family then has a higher `term`, which every `welcome` names (`Welcome.term`). When a fallback answers as the main at a term at least the one the paired server last named (`Pairing.term`), the engine makes it the server the device is paired with, and the one it paired with before a fallback: the pairing follows the family's main through every switch, and only the new main's refusals stop sync. Of two mains answering at once (a family split for a while), it takes the higher term, unless the paired server answers as a main first.
- **Showing it:** `SyncState.via` is the name of the fallback the connection goes to, `null` while it goes to the main.
- **Pairing and disconnecting** go to the main: a subordinate refuses `pair` with `notMain`. `disconnect()` while connected to a subordinate forgets the pairing here, but the main keeps the device until it is removed on the main's page.

### The pairing record

`createPairingStore({ key })` keeps one JSON record under `key` in `localStorage`: the server and how to reach it, the device id and token, `lastRevision` and `lastSyncAt`, and optionally `fallbacks`, `term` and `upload`. A record that cannot be saved (storage full or blocked) is logged with `console.warn`; sync goes on for this run. It holds the device's token, so it belongs in no backup and is never synced. A record that does not read (one written by a release before `1.0.0-rc.2`, which has no `lastRevision`, included) reads as no pairing: the device pairs again.

### States and errors

`onState` receives `{ status, serverName, via, lastSyncAt, error, transfer, diverged }`. `serverName` is the main's name, and `via` the fallback's, when the connection goes to one. `status` is one of `unpaired`, `connecting`, `syncing`, `connected`, `unreachable`, `paused`, `interrupted` (a large transfer waits for `resumeTransfer` or `restartTransfer`), `error`. `error` is a `WorkspaceError` (or your `DocumentError`); its `message` is English for a developer, and its `code` is what an interface translates.

| Code                                                                                      | What the engine does                                                                                      |
| ----------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| `connectTimeout`, `closed`                                                                | `unreachable`: retries after 2 s, 5 s, 15 s, 30 s, 1 min, then every 5 min, while the page is visible     |
| `blocked`                                                                                 | `paused`: the server blocks this address after failed attempts; retries once a minute, the pairing stands |
| `internal`, and any code it does not know                                                 | `paused`: retries on the same schedule as `unreachable`                                                   |
| `badToken`, `unknownApp`, `serverChanged`, `protocol`, `tooManyWrites`, a `DocumentError` | `error`: stops until the device pairs again or the app starts again                                       |
| `quota`, `badPath`, `incomplete`, `conflict`, `notAuthenticated`                          | `error`: tries again on the next edit, push or `syncNow`                                                  |

A plan the server refused with `quota` or `badPath` is not sent again while the plan and the set stay as they were: the next sync shows the error without sending a byte. A change to the plan or to the set, or `syncNow()` (after the user raised the quota, say), sends it again.

A write that fails with `noTransaction`, `incomplete` or `internal` (which may come after the server's commit point, with the write applied) is never committed again as it was: the engine lists, reads and plans again, within the same three attempts, and returns to the transaction only if the server still holds it open; past them, the error is handled as the table says. `tooManyWrites` is the engine's own guard: more than 10 commits within a minute stop writing until the app starts again.

`pair` rethrows its failure for the page to show: `badSecret` (a wrong, expired or used code), `unknownApp` (the server offers to add the app; try again after), `notMain` (the code is a subordinate's: pair with its main), `blocked`, or a connection failure. `parsePairingCode` throws `notPairingCode`, `tooNew` (the code comes from a newer server) or `damaged`.

## The protocol client

`ServerClient` speaks the protocol over an open data channel (`openServerChannel` opens one) for an app that drives its documents itself. A document is a `DocumentKey`, `{ path, id, type }`: the server keeps it as `<path>/<id>.<type>` in the app's folder, `path` being `''` for the app's root.

- `head()` is the set's revision; `list()` one page of the set (`docs`, `removed` tombstones, `cursor`), `listAll()` every page of one revision. The `changed` push (`onChanged`) names the new revision.
- `get(key, { etag, offset, onPiece })` reads a document at the etag a listing named: a document that changed since answers `{ stale: revision }` (`isStale`), and the app lists again. `onPiece` takes the body as it arrives, so a large one never sits whole in memory; `offset` resumes a read.
- Writes go in a transaction: `begin(tx)` (a client-chosen id, 16 to 64 of `[A-Za-z0-9_-]`; it returns to a transaction still open and reports what it holds, for resuming), `put(tx, key, ifEtag, body, offset)` with a `Uint8Array` or a `Blob` read a slice at a time, `delete(tx, key, ifEtag)`, then `commit(tx)`, all of it or nothing: `{ committed: false }` is a conflict, with the current etags, and leaves the transaction open; `abort(tx)` drops it.
- `resolve(server, keep)`, `onDiverged` and `onResolved` are for a divergence of the user's servers: when a subordinate's copy of the app and its main's both changed apart, the `welcome` (`diverged`) and the `diverged` push list the subordinates whose copy waits for a choice, and `resolve` keeps the main's copy (`'main'`) or the subordinate's (`'subordinate'`), the whole set; the first choice on any server wins, and every choice is answered `resolved`. Ask with both servers' names (roles move with a switch of the main), and keep `since` as the string received: it names the divergence. `SyncEngine` asks through `state.diverged` and `resolveDivergence`.

## Testing an app

`@flighter/wss-client/testing` has a `FakeServer`: pass its `connect` to the engine, and several engines can share one, standing in for several devices. It holds an app's set of documents in memory, with etags and revisions computed as the real server's are, list pages (`pageSize`), tombstones, `stale` reads, transactions that belong to a device and resume from what arrived, and conflicts.

- **Its set:** `storeDocument(key, body)` and `removeDocument(key)` change it as another device would; `document(key)`, `text(key)`, `etag(key)`, `revision`, `documents`, `removed` and `transactions` show what it holds, `puts` every body it took (with its `offset` and the bytes that came), and `log` every call in order (`put#1`, `commit#2`, …).
- **Failures:** `connectError`, `helloError`, `pairError`, `conflicts` (that many next commits conflict), `commitError` with `commitAppliedFirst` (before or after the commit point), `connectGate` for one slow to answer, `cutPutAfter` / `cutGetAfter`, which drop the connection once that many more body bytes went, the body staged with what arrived, as a real cut does, `losePuts` (that many next put frames never arrive, and the connection drops), and `quota`, refused once a put's body arrived and at the commit, as the real server does.
- **A divergence:** `setDiverged([{ serverId, name, since }])` names what waits, in every `welcome` and pushed to every open client; a client's `resolve` takes the first choice for each, tells the other clients `resolved`, records it in `resolutions` and calls `onResolve(server, keep)`, where a test makes the set what the choice keeps (with `storeDocument`, then `pushChanged()`). What passes between two servers is not modelled.

For a main and its subordinates, give each `FakeServer` its own `serverId` and `port`, set `role`, list the subordinates' `entry()` in the main's `subordinates`, let them share the main's `devices`, and pass `FakeServer.route(main, laptop)` as `connect`. Set `term` for a family that switched its main. `FakeChannel` stands in for a data channel under `ServerClient`.

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
