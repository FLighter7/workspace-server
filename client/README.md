# @flighter7/workspace-client

The client for [workspace-server](https://github.com/FLighter7/workspace-server): a small program you run on your own computer that keeps each app's data as one JSON document on your disk and syncs that app's devices over WebRTC on your local network.

This package is everything a TypeScript web app needs to sync through it:

- reading the **pairing code** the server's page shows, as a QR code or as text;
- reaching the server over a **WebRTC data channel with no signaling**: the device builds the server's side of the connection itself from the pairing;
- the **protocol**: hello, pair, head, get, put, forget, and the `changed` push;
- a **sync engine** that decides when to connect, when to sync and when to write, retries what can be retried, and stops on what cannot;
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

### States and errors

`onState` receives `{ status, serverName, lastSyncAt, error }`. `status` is one of `unpaired`, `connecting`, `syncing`, `connected`, `unreachable`, `paused`, `error`. `error` is a `WorkspaceError` (or your `DocumentError`); its `message` is English for a developer, and its `code` is what an interface translates.

| Code                                                                                      | What the engine does                                                                                      |
| ----------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| `connectTimeout`, `closed`                                                                | `unreachable`: retries after 2 s, 5 s, 15 s, 30 s, 1 min, then every 5 min, while the page is visible     |
| `blocked`                                                                                 | `paused`: the server blocks this address after failed attempts; retries once a minute, the pairing stands |
| `internal`, and any code it does not know                                                 | `paused`: retries on the same schedule as `unreachable`                                                   |
| `badToken`, `unknownApp`, `serverChanged`, `protocol`, `tooManyWrites`, a `DocumentError` | `error`: stops until the device pairs again or the app starts again                                       |
| `tooLarge`, `notJson`, `conflict`, `notAuthenticated`                                     | `error`: tries again on the next edit, push or `syncNow`                                                  |

`pair` rethrows its failure for the page to show: `badSecret` (a wrong, expired or used code), `unknownApp` (the server offers to add the app; try again after), `blocked`, or a connection failure. `parsePairingCode` throws `notPairingCode`, `tooNew` (the code comes from a newer server) or `damaged`.

## Testing an app

`@flighter7/workspace-client/testing` has a `FakeServer`: pass its `connect` to the engine, and several engines can share one, standing in for several devices. Switches make it fail as a real one can (`connectError`, `helloError`, `pairError`, `conflicts`), and `log` records every call. `FakeChannel` stands in for a data channel under `ServerClient`.

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
