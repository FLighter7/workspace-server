# @flighter7/workspace-client

The client for [workspace-server](https://github.com/FLighter7/workspace-server): a small program you run on your own computer that keeps each app's data as one JSON document on your disk and syncs that app's devices over WebRTC on your local network.

This package is everything a TypeScript web app needs to sync through it:

- reading the **pairing code** the server's page shows, as a QR code or as text;
- reaching the server over a **WebRTC data channel with no signaling**: the device builds the server's side of the connection itself from the pairing;
- the **protocol**: hello, pair, head, get, put, forget, and the `changed` push;
- a **sync engine** that decides when to connect, when to sync and when to write, retries what can be retried, and stops on what cannot, and that syncs through a **fallback server** (a subordinate of the user's main server, such as a laptop's) when the main is out of reach;
- **test doubles** for the app's own tests.

Your app supplies one thing: how to merge its document.

The wire contract is the server's [docs/protocol.md](https://github.com/FLighter7/workspace-server/blob/main/docs/protocol.md). This package speaks `proto` 1.

## Installing

```sh
pnpm add @flighter7/workspace-client
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
} from '@flighter7/workspace-client';

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

`plan` runs once per sync with the server's document (`etag` and `body`; an empty body and a `null` etag when there is none yet). It returns what to change locally (`apply`) and what to write (`write`, or `null`). The engine writes conditionally on the etag it read; if another device wrote first, it reads and merges again, up to three times.

Rules that keep devices from writing back and forth:

- **Read the local state in `plan` itself**, not in `apply`. The engine reads `changeCount` just before calling `plan`, with no await between, so an edit made afterwards is counted past it and synced next time.
- **Let the server's copy win a tie.** Every device then settles on the same result.
- **Write only what the server lacks.** A write caused only by local housekeeping (pruning, garbage collection) makes two devices with different settings undo each other's writes forever.
- **Throw a `DocumentError` for a document this build cannot read**: a newer format, or a damaged one. Sync halts, and nothing is written over it. Its code is yours to name and translate.

### Fallback servers

A server can be linked to the user's main server as its **subordinate**: a laptop that keeps the main's data on its own disk and travels with the user. The device still pairs only with the main; the main lists its subordinates in every `welcome`, and the engine keeps them in the pairing record (`Pairing.fallbacks`, optional, so a record saved before reads as having none). Your app has nothing to do for it.

- **Connecting:** with fallbacks known, the engine connects to the main and every fallback at once. It takes the main if it answers within `MAIN_GRACE_MS` (2 s) of the start, and otherwise a fallback that answered (a main before a subordinate, then the list's order); the others are closed. It stays with that server until the connection ends or the page is hidden and shown again, which tries the main first again.
- **The same pairing:** the device id, the token and `lastEtag` are shared by every server: a subordinate admits the main's devices, and an etag is the document's hash, the same on every server holding it.
- **Errors:** only the main's refusals stop sync. A subordinate that does not know this device yet (it was paired since the subordinate last reached the main) refuses it with `badToken`, and the engine carries on as though that server were away. A `welcome` from a server that is neither the main nor a known fallback is not taken; from the main's own address it is `serverChanged`.
- **Keeping the list current:** each `welcome` from the main replaces the list; a subordinate's `welcome` carries none and leaves it as it is.
- **Following the main:** a subordinate can take the role of main over (the server's guide, "A laptop as a second server"). The main's family then has a higher `term`, which every `welcome` names (`Welcome.term`). When a fallback answers as the main at a term at least the one the paired server last named (`Pairing.term`, optional, so a record saved before reads as 0), the engine makes it the server the device is paired with, and the one it paired with before a fallback: the pairing follows the family's main through every switch, and only the new main's refusals stop sync. Of two mains answering at once (a family split for a while), it takes the higher term, unless the paired server answers as a main first. The device id, the token and `lastEtag` stay.
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
| `tooLarge`, `notJson`, `conflict`, `notAuthenticated`                                     | `error`: tries again on the next edit, push or `syncNow`                                                  |

`pair` rethrows its failure for the page to show: `badSecret` (a wrong, expired or used code), `unknownApp` (the server offers to add the app; try again after), `notMain` (the code is a subordinate's: pair with its main), `blocked`, or a connection failure. `parsePairingCode` throws `notPairingCode`, `tooNew` (the code comes from a newer server) or `damaged`.

## Testing an app

`@flighter7/workspace-client/testing` has a `FakeServer`: pass its `connect` to the engine, and several engines can share one, standing in for several devices. Switches make it fail as a real one can (`connectError`, `helloError`, `pairError`, `conflicts`, `connectGate` for one slow to answer), and `log` records every call. `FakeChannel` stands in for a data channel under `ServerClient`.

For a main and its subordinates, give each `FakeServer` its own `serverId` and `port`, set `role`, list the subordinates' `entry()` in the main's `subordinates`, let them share the main's `devices`, and pass `FakeServer.route(main, laptop)` as `connect`. Set `term` for a family that switched its main.

```ts
import { FakeServer } from '@flighter7/workspace-client/testing';

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
