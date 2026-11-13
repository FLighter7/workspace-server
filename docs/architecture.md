# Architecture

How `workspace-server` works, why it was built this way, what limits it has, and what has been verified where.

- **Audience:** developers and agents.
- **Related documents:**
  - the user guide: [guide.md](guide.md);
  - the wire contract with clients: [protocol.md](protocol.md);
  - open work: [TODO.md](TODO.md).
- **History:** this file condenses the original design (2026-11-07), the Go rewrite plan, its reviews, the Windows checks, the start/stop/delete plan (2026-11-09) and the local hub plan (2026-11-11/12: a subordinate server on the laptop, handing it out to devices, switching the main). The full texts are in git: the spec and plan up to `ad18bef`; the Windows reports in `c925c5e` (mDNS) and `a99ba9e` (server, lock, autostart); the start/stop/delete plan and its check lists in `af855e0`. The local hub plan was a working file outside git: its decisions are L1–L34 below, its limits under "Limits", and what it left open in [TODO.md](TODO.md); its commits run from `9a8cb7e` to `24e3149`. The "Resume" control for an app the write guard stopped, the user's choice of 2026-11-13, is L35. The client package was renamed from `@flighter7/workspace-client` to `@flighter7/wss-client` and set to 1.0.0 on 2026-11-12, before its first publish; nothing a device stores carries the package's name, so no device pairs again. On 2026-11-13 the user chose to publish it first as a release candidate, `1.0.0-rc.1` (server tag `v1.0.0-rc.1`), and `1.0.0` only after checking that by hand. The same day, still before its first publish, it became `@flighter/wss-client`: the scope `@flighter7` was not the user's npm account (`flighter`), and npm refused the publish with `Scope not found`. The version stayed `1.0.0-rc.1`, and again nothing a device stores changed. The files plan of 2026-11-13 (a working file outside git, like the hub plan) turns an app's one JSON document into a set of plain files: stage 0 checked its risks on Windows (reports in `tmp/reports/files-stage0/`), stage 1 rewrote the store, and stage 2 the protocol, the hub, the link and both clients, in one contract commit; their decisions are under "Storage", "Sessions", "Hub", "The sync rule" and F1–F9 below. Stages 3 (a device chooses when the servers diverged, the family's quota), 4 (the page), 5 (the client package's engine for several documents) and 6 (todo) are open.

## What it is and what it is not

- **What it does:** it keeps **each app's documents**, a set of plain files in any format, in a folder on disk, and it is the meeting point through which that app's devices sync.
- **What it knows:** nothing about any app's data. It stores bytes as plain files, checks no format, and tells versions apart by their SHA-256.
- **Who merges:** the apps. They keep their own local copy, merge themselves, and treat the server as one more peer: the one with a disk that is always on.
- **Goals:**
  - Sync with no user action after a one-time pairing per device.
  - Devices need not be online at the same time.
  - A durable copy of each app's data as a plain file.
  - One server for several independent single-user apps.
  - No internet dependency: no domain, no certificate authority, no relay.
- **Non-goals:**
  - several users or accounts per app;
  - reaching the server from outside the LAN;
  - knowing an app's data model;
  - merging;
  - a history of documents;
  - IPv6.

## Why WebRTC and not HTTPS

The Node server this repository held before 2026-11-07 served HTTPS, which cost three things:

- **Mixed content.** A page served over HTTPS cannot call plain HTTP, and Safari blocks that even to `http://localhost`. So the server had to speak HTTPS.
- **A root CA on every device.** HTTPS meant mkcert, and every device needed mkcert's root CA. On iOS that takes two steps, and the second one, full trust in Settings → General → About → Certificate Trust Settings, is easy to miss.
- **A pinned IP.** A certificate names an IP, so the IP had to be pinned with a DHCP reservation.

WebRTC avoids all three:

- it is not subject to mixed-content rules;
- its DTLS is authenticated by a certificate fingerprint pinned at pairing, so nothing is installed on a phone;
- the pairing code carries the addresses and a `.local` name, so nothing is reserved.

### What the spike established (2026-11-07)

The spike was a throwaway Go + Pion server with a static client page: [`FLighter7/webrtc-direct-spike`](https://github.com/FLighter7/webrtc-direct-spike), file `server/main.go`. On the author's Mac it is also at `../webrtc-direct-spike/server/main.go`.

**Results:**

- These connected with no code exchange, by IP and by `<uuid>.local`:
  - iPhone Safari, in a tab and installed to the Home Screen;
  - iPhone Chrome;
  - Mac Chrome.
- A connection opens in 150–200 ms, occasionally about 1.2 s.
- 1 MB took 0.2 s over Wi-Fi.
- A server restart with the same identity needed no re-pairing.
- No permission prompt appeared.
- Mac Safari and Android were not tested.

**Findings:**

1. **No signaling is needed.**
   - The server is ICE-lite with a fixed ICE password and certificate.
   - The client picks the server's ufrag per connection and builds the server's SDP answer from a template.
   - The server sees the unknown ufrag in the first STUN binding request. It verifies MESSAGE-INTEGRITY against its password, and only then creates a peer connection.
   - Pion's `ice.UDPMuxParams.OnUnhandledPacket` makes this possible without patching Pion.
2. **The `.local` name must be a UUID and IPv4-only.**
   - WebKit connects to a `.local` candidate only when it looks like a browser-generated mDNS name. The computer's own name and `rtc-spike.local` failed on iPhone.
   - libwebrtc prefers IPv6 when both record types exist.
3. **A browser on the server's own machine** resolves the computer's name to loopback first, and Chrome does not pair a LAN candidate with loopback. The UUID name plus explicit LAN IPs avoids this. The server still listens on `127.0.0.1`.
4. **Pion refuses an expired certificate,** so the certificate is valid for 20 years.
5. **Timing:** a vanished client is noticed after about 11 s. A client back from the background finds its connection `failed` and opens a new one in about 150 ms.
6. **A full-tunnel VPN on the server** does not interfere. Clients get the LAN addresses explicitly, and the LAN route bypasses the tunnel.

## Code map

Module `github.com/FLighter7/workspace-server`, Go 1.27. Dependencies:

- Pion `webrtc/v4`, `ice/v4` and `stun/v4`;
- `golang.org/x/net` (mDNS packets);
- `golang.org/x/sys` (locks, socket options);
- `skip2/go-qrcode`.

| Package | Job |
| --- | --- |
| `cmd/workspace-server` | `main.go` dispatches the commands (none, `run`, `start`, `stop`, `delete`, `version`); `commands.go` implements them; `control.go` finds a running server and asks it to stop. `server.go` owns the server folder and runs the server, restarting it in-process for a reset or a data-folder move; `backend.go` is the web page's backend. `datafolder.go` checks, moves, resets and deletes folders. `logfile.go` keeps `server.log` within 1 MiB. `lock_*.go` holds the server-folder lock; `console_*.go` releases the Windows console. |
| `internal/datadir` | The default server folder per OS. |
| `internal/identity` | `identity.json`: serverId, certificate, ICE password, mDNS name. |
| `internal/config` | `config.json`: name, UDP and HTTP ports. |
| `internal/apps` | `apps.json`: the app registry. It is reloaded when the file changes. |
| `internal/devices` | `devices.json`: paired devices and their token hashes, and the devices removed (tombstones); a server-kind device (a subordinate) has no app, and keeps what it announced. On a subordinate, also the main's devices it admits (the replica, in memory). Merging a registry of the family into this one, and minting a credential for another server. |
| `internal/store` | An app's documents as plain files: the index, the revision, transactions committed through a journal, the quota. |
| `internal/fsutil` | `WriteFileAtomic`, shared by every file writer. |
| `internal/pairing` | The pairing code, secrets and refused pairings, in memory. |
| `internal/protocol` | One session over a data channel: framing, the message state machine, the ordered send queue; a `put` body streamed into the backend's sink, a `doc` body streamed from the file the backend opened; list paging. |
| `internal/hub` | The protocol's backend: authentication, rate limits, the documents' frames answered from the store (`documents.go`: listing, reads, transactions, and the link's own transactions, `LinkTx`), divergences listed to the devices and their choices taken (`divergence.go`; a main keeps its subordinates' in `.wss/divergences.json`), a reconcile at every `hello`, broadcast of `changed`, closing sessions on removal; server-kind devices, link sessions, the app list, `announce`, the subordinates in `welcome` and the device registry pushed to subordinates; `pair` refused on a subordinate; a pairing code for a subordinate's browser (`invite`); the term in `welcome`, `term` frames handed to the link, `takeOver`, the registry handed over by a main that stepped down. |
| `internal/rtc` | The UDP listener and the signaling-free accept path. It creates a peer connection per unknown ufrag. |
| `internal/mdns` | The minimal mDNS responder for the `<uuid>.local` name, and a one-shot resolver for a main's name. |
| `internal/wsclient` | A Go client of the protocol: the answer, the pairing code, framing and the messages, as in `client/`, with bodies read from an `io.Reader` and written to an `io.Writer` (`documents.go`), and `Dial` with Pion as the offerer. |
| `internal/upstream` | A subordinate's link to its main: `upstream.json`, the link and app sessions, the sync rule (`sync.go`: each app's set compared document by document with the base in `.wss/link.json`, pulled and pushed as resumable transactions), a divergence waiting for a device's choice and carried out once one comes (`diverge.go`, `.wss/diverged.json`), the write guard and resuming an app it stopped; announcing this server and keeping the main's device registry; asking the main for a pairing code (`invite`); unlinking. Switching the main (`family.go`): `family.json`, becoming the main, following a higher main, stepping down, telling the family. |
| `internal/netaddr` | LAN IPv4 addresses and interfaces. |
| `internal/web` | The localhost page: serves the page built from `client-wss/` (embedded from `ui/dist`), the API, the guard. |
| `internal/autostart` | LaunchAgent, `systemd --user` unit, Windows logon task; opening the page. |
| `internal/program` | The program file itself: the Windows Apps & features entry, and deleting the file. |
| `internal/conformance` | Reads the contract vectors in `testdata/conformance/`, for tests. |

### The web page (`client-wss/`)

The page `internal/web` serves: Vue 3, Vuetify 4 and vue-i18n, built with Vite. It is a private pnpm package of its own, beside `client/` and not a workspace with it, and it does not depend on the client package: it speaks only the page's JSON API. Its conventions follow the todo app: tabs, single quotes, width 100; SFC blocks in the order style, template, script; `<style lang="scss" module>`, `<script setup lang="ts">`, the `@/` alias.

- **The build goes to `internal/web/ui/dist/`, which git never holds.** `pnpm --dir client-wss build` writes it; `go build` embeds it. A binary built without it still compiles (`ui/.gitkeep` keeps `//go:embed all:ui` valid) and answers `/` with 503 and how to build the page; the API works. The release builds the page once and refuses to build a binary without it.
- **The strings** are in `src/i18n/locales/{en,ru,pl}.json`, precompiled by `@intlify/unplugin-vue-i18n` (the runtime compiler needs `eval`, which the CSP forbids). The type checker holds every `t()` key to the English catalog; the unit tests hold the other languages to the same keys and placeholders, and fail on a key nothing names.
- **Language and theme are the viewer's:** the language comes from the choice saved in this browser, else `navigator.languages`, else English; the theme is system, light or dark, `system` following the OS live. Two buttons at the right of the header open their pickers, as in the todo app. The choice lives in `localStorage['workspace-server.preferences']`, every access guarded. The origin includes the port, so a new HTTP port, or `127.0.0.1` instead of `localhost`, starts from the defaults again; accepted for a page on loopback.
- **The sections are links, not tabs:** a `<nav>` of `VBtn` links, centred, to `#server`, `#devices`, `#apps`, the current one with `aria-current="page"`. `VTabs` would give each link `role="tab"`, `aria-selected` and `tabindex="-1"`, which are wrong for navigation.
- **Pairing is a full-screen dialog** opened by the button at the top of Devices, not a section of its own: the panel inside mounts only while the dialog is open, so opening it issues a code (unless the one on screen is still valid) and the refusals are polled only then. The dialog focuses its title on open. "Pair this computer's browser" on a subordinate is the same kind of dialog (`FullScreenDialog.vue`), around the main's code instead of this server's (see "The page" under "Server modes and the link").
- **The Upstream server subsection** is part of Server, an `h2` region after the data folder and before the danger zone: the link is what this server is to the others, a setting of the server like its name, and too rarely used on a main to earn a link in the header. It polls `GET /api/upstream` while Server is shown.
- **The camera** reads a pairing code with `qr-scanner`, loaded only when the user presses "Scan with the camera". The package starts its decoder as a Worker from a `blob:` URL, which the CSP refuses (a worker's source falls back to `default-src 'self'`); a small Vite plugin in `vite.config.ts` replaces its worker module at build time with one that starts the same code from `/assets/qr-scanner-worker-<hash>.js`, a file of the page's own, so the CSP stays as it is. The code is taken from the package's module by running it with stand-ins for `Worker`, `URL` and `Blob`, so it follows the package's updates; the dev server, which sends no CSP, keeps the blob. Its overlays (`highlightScanRegion`, `highlightCodeOutline`) stay off: they are SVG with `style` attributes, which the CSP refuses too. The e2e reads a QR code through Chromium's fake camera and checks that the worker file was loaded and nothing was blocked.
- **Managing the server is a danger zone:** its buttons sit in a `<fieldset>` outlined in the theme's error colour, the section's `h2` inside the `<legend>`.
- **Nothing is parsed as HTML:** names, device names and refused ids are text in `<bdi>`, and `vue/no-v-html` is an error.
- **Dialogs** focus their safe button (Cancel) or their input on open and give the focus back on close; when the element that had it is gone with its row (a deleted app, a removed device), the section's `h1` gets it, never the body.
- **Development:** `pnpm --dir client-wss dev` proxies `/api` to `WSS_TARGET` (default `http://127.0.0.1:47810`, a throwaway server). The guard wants its own `Host` and `Origin`, so the proxy rewrites both; the server still sends no CORS headers. The boot block is then unfilled, and the page falls back to the defaults in `src/boot.ts`, which mirror `internal/apps`.
- **Tests:** Vitest for the logic and components (happy-dom), Playwright e2e against real servers built with the page embedded (two of them, a main and a server linked to it, for the Upstream server section), which also checks that no CSP violation happens on any section, dialog or theme, the camera included. For Resume, a small Go device (`e2e/device`, built with Go like the server, on the Go client `internal/wsclient`) pairs with the main and writes through the linked server until its write guard stops the app: the real guard, not a stand-in status. It is a Go package of the module, so the Go checks build and vet it too.

### The client package (`client/`)

`@flighter/wss-client` is the client a TypeScript web app syncs through. It is a pnpm package of its own, not a workspace: the root stays a Go module, and Go ignores the folder. Its README is the documentation for app developers.

| Module | Job |
| --- | --- |
| `PairingCode.ts` | Parses a `wsrv1.` code into what the device keeps. |
| `serverAnswer.ts` | Builds the server's SDP answer from the pairing, with no signaling. |
| `openServerChannel.ts` | `RTCPeerConnection` with no ICE servers, one data channel labelled `workspace`; a closed channel closes the peer connection. |
| `ChannelFraming.ts` | Text frames, bodies in 16 KiB pieces from an offset, sent from a `Uint8Array` or a `Blob` read a slice at a time, received whole or handed on piece by piece; flow control at 1 MiB / 256 KiB. |
| `ServerClient.ts` | The messages, one request at a time: the set (`head`, `list`, `listAll`), reads (`get` at an etag, from an offset, whole or in pieces), transactions (`begin`, `put`, `delete`, `commit`, `abort`), `resolve`; `changed`, `diverged` and `resolved` pushes; an unknown error code reads as `internal`. |
| `PairingStore.ts` | The pairing record in Web Storage under the app's key, with the main's subordinates as optional fallback servers and the revision last synced; every access guarded. |
| `SyncEngine.ts` | When to connect, sync and write; retries, halts, the write guard, the conflict loop; the main and its subordinates at once, preferring the main. It syncs one document of the set, `data.json` (F7); the app's `SyncAdapter` merges it, and its `DocumentError` halts sync. |
| `startInPage.ts` | Starts the engine and follows the page's visibility, the back-forward cache and the network. |
| `WorkspaceError.ts` | The error codes, by kind: server, pairing code, transport, engine. |
| `testing/` | `FakeServer` and `FakeChannel`, exported as `@flighter/wss-client/testing` for apps' tests. |

**The contract as data:** `testdata/conformance/` holds the contract as vectors. The client package's `conformance.test.ts` plays them against `ServerClient`, `parsePairingCode` and `buildServerAnswer`. The Go tests play the same files against the `Session` (`internal/protocol`), `EncodeCode` (`internal/pairing`), the reference answer builder (`internal/rtc`) and the Go client (`internal/wsclient`). `frames.json` keeps the exchanges only servers speak (server-kind pairing, link sessions, `apps`, `announce` and `devices`) in a section of their own, `serverKind`, which the client package does not read. A change on one side that the other does not follow fails a test. A change to the contract changes `docs/protocol.md`, the vectors and both sides in one commit.

| File | Holds |
| --- | --- |
| `constants.json` | `proto`, the code prefix, the chunk size, the flow-control marks, the largest list page |
| `error-codes.json` | Every code the server sends |
| `pairing-codes.json` | Valid codes with their fields and what a client keeps; invalid ones with the client's error code |
| `answers.json` | Offer, pairing data, ufrag and time, to the exact SDP answer |
| `frames.json` | Every request and answer as frames, with the backend state that causes it; frames each side tolerates |
| `chunks.json` | Body sizes and offsets to the binary message sizes |

## Running

### Start-up order (`runServer`)

`runServer` owns the **server folder** for the whole process (`runOwned`), and runs the server itself in `serveOnce`, once or, after a reset or a data-folder move, again in the same process.

1. Create the server folder.
2. Take its lock (`server.lock`).
   - If another process holds it, fail at once with `another workspace-server is already using <dir>` (the error matches `errAlreadyRunning`). Exit 1 and touch nothing else, `server.log` included.
   - Any other lock error, such as a file system without locks, is logged, and the server starts without the lock.
3. Open `server.log` (see "The log" below).
   - Every log line goes to the file and to stderr.
   - A failed stderr write does not drop the file line: a server started at login has no terminal.
   - The standard logger is redirected into the same sink while the server runs.
4. `serveOnce`: load or create, in this order:
   - `identity.json`;
   - `config.json`, whose default name is the host name, trimmed and cut to 64 runes, with fallback `workspace-server`;
   - the **data folder**: `dataDir` from `config.json`, or the server folder. A missing data folder is created when its parent exists; otherwise the server does not start (`cannot start: the data folder … is not available` in the log). Creating it under a missing parent would show an empty server where the user's data used to be, on an unmounted disk. Then its `WSS-Data` folder, and `README.txt` in it when it is missing;
   - `apps.json` (a first start writes the `todo` entry), plus the apps found in an adopted data folder;
   - `devices.json`;
   - `upstream.json`, the link to an upstream server, when there is one (see "Server modes and the link"), or else `family.json`, a main's term (see "Switching the main").
5. Apply the flag overrides, then validate the result. The flags can set equal ports, for example. On an in-process restart the ports in effect at the end of the previous run take the flags' place, so a port changed on the page survives a reset or a move.
6. Start the UDP listener, then the web page on `127.0.0.1:<httpPort>`, then the mDNS responder, then the link to the upstream server, if any. Stopping closes the link first, since it writes through the hub.
   - The responder may fail; the server then works by IP only, and the page shows the responder as down.
   - Interactive mode's `OnReady` (open the browser) runs once, after the first start.
7. Run the watchers until a signal arrives (Ctrl+C, SIGTERM, SIGHUP, a closed console window on Windows) or the page asks the server to stop, reset or move its data.
8. Stop everything and log `stopping` and `stopped`. Then, with the lock and the log still held:
   - **reset:** delete `identity.json`, `apps.json`, `devices.json`, `upstream.json`, `family.json` and the server's app folders in the data folder's `WSS-Data` (with `README.txt` and `WSS-Data` when nothing else is left in it; step 4 makes them again); keep `config.json`, `server.log` and the lock; go to step 4;
   - **move:** copy the data to the new folder, save `config.json`, delete the old copy (see "Changing the data folder"); go to step 4;
   - **stop or delete:** close the log, release the lock, then perform the removals (see "Stopping and removing the server").

A reset or move that fails is logged and shown by the page (`/api/status` `lastError`); the server comes back on the old state.

A restart that cannot start (a port taken in the gap, say) is retried every 2 s for a minute, keeping the lock and the log; once it starts, the page shows why the first attempt failed. Only then does the process end with the error, where only autostart would bring it back (launchd and systemd restart on failure; the Windows task does not). The first start is not retried: its error goes to the terminal or the log at once.

### Watchers

- **`apps.json`:** checked every 2 s; a hand edit takes effect.
  - An invalid file keeps the last good list; each distinct failure is logged once.
  - An app dropped from the file has its sessions closed: every app with sessions that is not listed, not only those listed before.
  - A key the server does not read (a misspelt `histroy`, say) is logged with the file's path and ignored, not refused, so that a file from a later version still loads. Keys match without regard to case, as `encoding/json` does.
  - Every change of the list, by hand or on the page, is pushed to the link sessions of server-kind devices (`apps`). A link session has no app, so the watcher never closes it.
  - **A page edit and a hand edit:** Add and Remove first take in a hand edit the watcher has not read yet, and apply their change to the file as it is now; the next check reports the list as changed. An invalid hand edit refuses an Add (400) and is replaced by a Remove, whose app's data may already be gone. The registry remembers its own writes by the new file's stamp, taken before the rename, so an edit right after a write is never mistaken for it.
- **LAN addresses:** checked every 30 s. On a change, the listener and the responder are rebuilt, so every live session ends and clients reconnect; a subordinate's link reconnects to its main at once.
  - A listener that cannot reopen is retried every 30 s, and the log says devices cannot connect. `/api/status` `listening` is false meanwhile, and the page shows a warning under the addresses.
  - A scan that fails (the system does not list its interfaces, or the addresses of one that could be on the LAN) keeps the addresses known before: a passing failure must not look like every address gone, which would end every session twice. It is logged when it first appears or changes, and the recovery once. At start, a failed scan leaves the listener on `127.0.0.1` until the next check.
  - `MultiUDPMuxDefault` cannot add or remove sockets, which is why the whole listener is rebuilt.
- **The responder:** if it is down, it is retried at the address check.

### The log

`server.log` never holds more than 1 MiB, at start and while the server runs, in one file. A line that would take it past the limit first cuts the file to its last 512 KiB at a line start: the writer closes the file, replaces it atomically with the kept part, reopens it and writes. The file must be closed for the replacement, since an open handle blocks the rename on Windows. A cut happens once per 512 KiB of new log. A line longer than the limit keeps its end. A cut or reopen that fails drops lines and never stops the server; the stderr copy still gets them. Pion's own messages stay on stderr only.

### Changing the ports from the page

- **The HTTP port:** the new port is listening before `PUT /api/config` answers 204, and the page moves to it after 1.5 s.
- **The UDP port:** the listener moves at once, and every device must pair again. The page warns the user and asks for confirmation. Opening "Pair a device" after the change issues a fresh code, since the old code carries the old port.

### Changing the data folder

The page asks `GET /api/data-dir?path=P` first; the server answers a verdict and the apps' data on each side. P is the folder the user chose; every verdict looks at its `WSS-Data` folder (see "The server folder and the data folder").

- **The server's app folders** are the folders under `WSS-Data/apps/` with a valid app id that hold the store's `.wss/` folder. Someone may put other things there, and a `WSS-Data` folder chosen itself may have held an `apps` of its own, so nothing else under `apps/` is data, and nothing else is ever moved or deleted: a reset, a move and `delete` remove only the server's app folders (and empty ones with a valid id), `apps/` only when it is then empty, and `README.txt` and `WSS-Data` only when nothing else is left in `WSS-Data`.
- **"Has data"** means a regular file in one of the server's app folders outside its `.wss/` folder: a document, or anything else put among them. The store's own files are not data, but a move copies them with the rest (an index, open transactions, a journal still to apply), symbolic links still skipped.

| Verdict | When | `PUT /api/config {"dataDir": P, "verdict": V}` |
| --- | --- | --- |
| `same` | the current folder, or one whose `WSS-Data` is the current one (the current `WSS-Data` itself, or its parent when the current data folder is a `WSS-Data` chosen itself), in any case where the file system ignores it | 204, nothing |
| `create` | missing, its parent exists | create it, move the data if any |
| `move` | exists without data; the current one has data | move the data |
| `switch` | neither has data | switch only |
| `adopt` | only P has data | switch, and add every `apps/<id>` with a valid id to the app list |
| `conflict` | both have data | 409 `dataExists` |
| `unavailable` | relative, not a folder, not writable, its `WSS-Data` and the current one overlap (one inside the other, or its `WSS-Data` is the current data folder), its `WSS-Data` is a file or not writable, or its parent is missing | 400 `invalid` |

- **`PUT` carries the verdict the page showed** (`verdict`, required with `dataDir` and allowed only with it) and checks the folders again, since they may have changed: `conflict` and `unavailable` are refused as in the table, and any other verdict that differs from the page's is 409 `changed`, so the user never gets a move they were not shown. Otherwise it answers 202; the handler flushes the answer and only then restarts the server.
- **One change at a time:** a `PUT` while a change waits for its restart, or while the server stops, is 409 `busy`.
- **The move checks a third time,** once the server has stopped: a verdict that changed since the `PUT` (data appeared in the current folder for a `switch`, say, which would stay behind unseen) changes nothing and goes to `lastError`.
- **The move** is copy, then switch, then delete: copy the server's app folders to `P/WSS-Data/apps.partial` (each file fsynced), put them in place (rename `apps.partial` to `apps`, or each app into an existing `WSS-Data/apps`), save `config.json`, write `README.txt` if it is missing (best effort: the start that follows writes it too), delete the old app folders, the old `README.txt` and `WSS-Data` if nothing else is left in it, and then the old folder itself if it is empty and is not the server folder. A crash before the save leaves the old folder current; after it, the new one current and a stale copy behind. A failure up to the save adds nothing to P and keeps the old folder current: a failed copy removes `apps.partial` and the `WSS-Data` it made; a failed save (or a `config.json` that cannot be read) removes the copied app folders, and `apps` and `WSS-Data` only if the move made them, so an empty `WSS-Data` the user made stays; a folder made for `create` goes again while empty.
- **Both `WSS-Data` folders are resolved once,** by the third check, and the move works on those paths: what `wssDir` makes of a folder depends on what it holds, and writing `README.txt` into one could turn a nested `WSS-Data` into one taken as is halfway through. A `move`, or a `create` whose source held data, fails (and changes nothing) when the source no longer holds any, rather than switching without the data; the copy also refuses two folders one inside the other.
- A data folder equal to the server folder is saved as an empty `dataDir`.
- The data folder holds only `WSS-Data`; the log stays in the server folder, so it can say why an unreachable data folder stopped the server.

## The UDP listener and sessions (`internal/rtc`, `internal/protocol`, `internal/hub`)

### Sockets

- One UDP socket per LAN IPv4 address, plus `127.0.0.1`, all on `udpPort`.
  - A LAN address belongs to an interface that is up, and is not loopback, point-to-point or link-local.
  - A duplicate address, the same IP on two interfaces, is bound once.
- Each socket is wrapped in `ice.NewUDPMuxDefault` with `OnUnhandledPacket`, and the sockets are combined with `ice.NewMultiUDPMuxDefault`.

### Accepting a connection

The callback must not block.

1. A STUN binding request arrives. Its `USERNAME` is `<serverUfrag>:<clientUfrag>`, its MESSAGE-INTEGRITY verifies against `icePwd`, and its server ufrag is not in use and did not end within the last minute (at most 1024 such ufrags are remembered). A late check from a client's old connection would otherwise start a ghost connection that holds a slot until the 20 s deadline.
2. A goroutine starts a peer connection:
   - a `SettingEngine` with:
     - `SetLite(true)`;
     - `SetICECredentials(serverUfrag, icePwd)`;
     - `DisableCertificateFingerprintVerification(true)`;
     - `SetAnsweringDTLSRole(DTLSRoleServer)`;
     - the shared mux;
     - `SetNetworkTypes(UDP4)`;
     - mDNS mode disabled;
   - the identity's certificate;
   - a stand-in offer as the remote description, then `CreateAnswer` and `SetLocalDescription`.
3. The browser retransmits its check, and the retransmission reaches the new connection.

Requests with a wrong password are dropped silently.

**The stand-in offer** (CRLF-joined):

```
v=0
o=- 0 2 IN IP4 127.0.0.1
s=-
t=0 0
a=group:BUNDLE 0
a=msid-semantic: WMS
m=application 9 UDP/DTLS/SCTP webrtc-datachannel
c=IN IP4 0.0.0.0
a=ice-ufrag:<clientUfrag>
a=ice-pwd:placeholderplaceholder00
a=fingerprint:sha-256 <32 × "00", colon hex>
a=setup:actpass
a=mid:0
a=sctp-port:5000
a=max-message-size:262144
```

### Lifetime and teardown

- **Deadlines:**
  - a peer connection whose data channel has not opened within 20 s is closed;
  - a session that has not authenticated within 10 s of the channel opening is closed.
- **When a session ends,** the peer connection is closed only after the data channel finishes closing, or after 2 s. That way the last frame, an error say, reaches the client.
- **Never close Pion objects inside a Pion callback:** it can deadlock. The data-channel adapter closes with `go dc.Close()`.

### Sessions (`internal/protocol`)

- **One ordered send queue per session,** drained by the session's own sender goroutine.
  - Replies and pushes never block their caller.
  - No text frame is ever sent inside a `doc` body.
  - The queue holds at most 1 MiB of text frames and at most 4 `doc` bodies besides the one being sent (`maxQueuedText`, `maxQueuedStreams`). A body waits as the open file the backend gave, never as its bytes, so a session's memory no longer grows with a document. An item that would take the queue past either limit closes the session, without an error frame, which could not go ahead of the queue anyway; a text frame that finds the queue empty is always taken. The close runs on its own goroutine: `PushChanged` queues under the hub's app lock.
- **Pairing confirmation:** a successful `pair` leaves the session unconfirmed until the client sends any further frame. A session that ends unconfirmed (closed during `Pair`, `paired` dropped from the queue or failing to send, or the client gone before it answered) calls the backend's `Forget` for the new device. This is the only way the server learns that the token arrived: a frame handed to SCTP may still be lost.
- **Flow control:** pause above 1 MiB buffered, resume at 256 KiB.
- **Sending bodies:** the sender reads a `doc` body from its file one 16 KiB chunk at a time, into one buffer the channel copies from, from the offset the `get` asked for, and closes the file after it. A file that cannot be read to its announced end closes the session: the client could not tell the frames that follow from the body.
- **Receiving bodies:** a `put` body goes into the `Sink` the backend gives (a file in the transaction's staging folder), each binary message written in the message handler as it arrives, before the next is read: SCTP's back pressure then slows the sender, and nothing piles up in memory (the files plan's stage 0 found this costs no speed on an SSD). A `put` the backend refuses is answered once its body has arrived, which the session reads and discards; a protocol error closes the session at once.
- **Files closed on close:** a session that ends closes the sink of a body still arriving (what arrived stays staged, synced, for a `put` that resumes it) and every `doc` body still queued.
- **List paging:** the session pages the backend's listing itself, 500 entries a page, documents then tombstones, with a cursor naming the last entry of the page before (`d:<key>` or `r:<key>`), and answers a page asked at a revision the set has left with `stale`.

### Hub (`internal/hub`)

- **Registering a session:** on a successful `hello`/`pair`, the session is registered under its app **before** the welcome's revision is read (register, then read).
- **Reconciling on `hello`:** before it reads that revision, the hub takes in the files of the app changed by hand (`Reconcile`, `store.Reconcile`). A reconcile that fails is logged, and the welcome names the revision the index holds: nothing in the app's folder keeps a device out.
- **Announcing revisions:** the hub keeps, per app, the revision it last told the app's sessions (seeded from the store when the hub starts; an app added later starts from an empty set). Whenever it finds the store's revision another (`announceLocked`, under the app's lock), it pushes `changed` to every session of the app and tells `Deps.Changed` once the lock is released. A commit (a device's or the link's), a reconcile and a `get` (whose open reconciles a file changed by hand) announce as they finish; the committing session is left out when its answer, `committed` or `conflict`, names the revision. Any other move is reported by the store (`Store.SetOnMove`, called after any operation that moved the revision of a loaded app: a journal applied again by whichever operation came next, a commit made straight into the store) and announced on a goroutine of its own (`storeMoved`). Every push goes through one comparison under the app's lock, so the pushes follow the order of the revisions, whatever moved them.
- **Holding `changed`:** a `changed` that arrives before the welcome is held and sent right after it, unless it equals the welcome's revision. Together these mean no missed change and no change before `welcome`.
- **Locking:** a per-app lock covers admission; a commit and its answer (`Session.AnswerCommitted`, `AnswerConflict`, queued under it, so no `changed` of an older revision follows the answer), a reconcile, a `get`'s open, and the announcement each makes; `begin`, `put` (its staging, not its body), `delete` and `abort`, so that none recreates the staging of an app being removed; and removal. `Head` and `List` read the index with no hub lock. The store's own locks are taken under the hub's, never the other way round.
- **Rate limit:** an IP is blocked at its 10th failure within a minute, for 10 minutes. While it is blocked, `hello` and `pair` get `blocked` without any check.
- **The session cap** (64) lives in the listener.
- **Removals:**
  - removing a device or an app closes its sessions;
  - `forget` closes every session of the device;
  - a transaction of a device removed meanwhile commits nothing (`badToken`), and neither `begin`, `put` nor `delete` takes one;
  - a device this server no longer admits loses its open transactions (`Store.AbortDevice`), on its app, or on every app for a server-kind one: removed here, dropped by the main's registry (`Replicate`), by a merged registry (`Adopt`), or handed over and not admitted by the main (`ClearDevices`).
- **Internal errors** reach devices as a generic message; the detail goes to the log. The store's own refusals become protocol codes (`ErrBadPath` `badPath`, `ErrQuota` `quota`, `ErrNoTransaction` `noTransaction`, `ErrIncomplete` `incomplete`); `ErrBadTx` and `ErrBadBody`, a client's wrong view of its transaction, are `protocol`, which closes the session, and the client's next `begin` reports what is staged.
- **A commit that fails after the store's commit point** is `internal`, and the store has ended the transaction: a commit of it again is `noTransaction`. Every session of the app, the committing one included, is told the revision if it moved, and the client lists again (protocol.md, "Transactions").
- **Server-kind devices** (subordinates) are admitted to every listed app and to link sessions, which have no app and carry the app list. Their sessions span every app, so their removal cannot take one app's lock: admissions and commits read-lock a `servers` lock, and removing a server-kind device write-locks it. Lock order: an app's lock, then `servers`, then the session set's lock.
- **The link's transactions** (`BeginLink`, `LinkTx`): the link writes what it takes from the main as a transaction of the store like a device's, staged under the device id `~link` (a device id is 1 to 64 of `[A-Za-z0-9_-]`, so it never is one), so an app has at most one, and it survives a restart as a device's does: `BeginLink` with the same id returns to it. Its bodies go straight to the staging folder through the store's sinks. Its commit announces the new revision to every session of the app (none of them wrote it); its refusals (`ErrIncomplete`, `ErrQuota`, `ErrBadPath`, a conflict) leave it open, and any other error may come after the commit point, so the link lists again. `Replace` and the `Stored` hook of the single document are gone.
- **`Deps.Changed`** is told every new revision the hub announces, whoever made it: a device's commit, the link's own, a reconcile, a `get` that took in a hand edit, a commit that failed after its commit point, a journal applied again. It is called once the hub's locks are released and must not block. A subordinate passes it to its link (`LocalWrite`), which compares the set with its main's and its base: its own commits come back to it too, and find the two in step at no cost. **`CloseServers`** ends every session of a server-kind device; the server calls it when it becomes a subordinate itself.

## The `.local` responder (`internal/mdns`)

The server runs its own minimal responder for `mdnsName`, built on `x/net/ipv4` and `x/net/dns/dnsmessage`.

### Socket

- One UDP socket on `0.0.0.0:5353` with address/port reuse:
  - `SO_REUSEADDR` and `SO_REUSEPORT` on Unix;
  - `SO_REUSEADDR` on Windows.
- It coexists with mDNSResponder on macOS, Dnscache on Windows and Avahi on Linux.
- It joins `224.0.0.251` on every LAN interface.

### Arrival interface

The responder reads the arrival interface from control messages.

- Windows does not provide it (`not implemented on windows/amd64`, logged at start).
- The responder then takes the interface holding the source address itself (a query from this machine), or else the one whose subnet holds it, the narrowest if several do. A source on no subnet of ours is not on the link, and its packet is dropped (RFC 6762 §11). The masks come from `netaddr`, which keeps them with the addresses.
- **Copies of one query:** one socket joined on several interfaces can be handed a datagram once per interface, and without the arrival interface every copy maps to the same guess. The same query from the same source on the same interface within 250 ms is answered once; a resolver's retry, about 1 s later, is answered again.
- **Read errors** are logged at the first, then at most once a minute with a count of those held back; reads are retried every 100 ms.

### Answers

- **A or ANY query:** one A record per IPv4 address of the arrival interface.
  - TTL 120, class IN with the cache-flush bit.
  - Header ID 0, authoritative.
  - IP TTL 255.
- **Any other type:** an NSEC record asserting that only A exists (RFC 6762 §6.1).
- **With A records,** when no other type was asked, the same NSEC goes in the additional section (RFC 6762 §6.1, a SHOULD). It does not shorten Windows' 1 s wait for AAAA.
- **Where answers go:**
  - to `224.0.0.251:5353` on that interface;
  - or unicast to the asker when the QU bit is set or the query came from a port other than 5353.
- **Legacy unicast** (a source port other than 5353, RFC 6762 §6.7): the answer echoes the query's ID and question, with TTL 10 and no cache-flush bit.

### Announcements and other names

- The A records are announced twice, 1 s apart, on start and on an address change.
- Every other name is ignored.
- There is no probing: a random UUID does not collide.

### Windows

- Dnscache waits about 1 s for AAAA before it returns the A result, whatever the responder answers. A nonexistent name with no responder costs the same.
- This is the Windows client's floor, not a defect: the responder answers NSEC in about 3 ms.

## Pairing internals (`internal/pairing`)

The code format and the outcomes are in [protocol.md](protocol.md).

- **The secret** is in memory only. A new secret revokes the previous one.
- **Opening "Pair a device"** issues a code at once, unless an unexpired one is already shown. "New code" issues another.
- **Refused pairings** (`unknownApp`):
  - the server keeps the last 5 distinct (app id, device name) pairs, for 10 minutes (an entry exactly 10 minutes old is still shown), with the time of the latest attempt; a device that keeps retrying takes one place;
  - the page offers "Add it?" only for a valid app id, and escapes the device name;
  - adding the app clears the refusals for it.
- **No confirmation button:** a valid secret proves the user stood at the server's screen.

## Server modes and the link (`internal/upstream`, `internal/wsclient`)

A server is a **main** or a **subordinate**. A main has no upstream server. A subordinate is linked to one main and mirrors its apps on its own disk: a laptop that keeps a PC's documents when it travels. A main and its subordinates are a **family**; a subordinate can take the role of main over ("Switching the main", below).

**The design of the whole hub** (at home, away, switching the main) is drawn in [images/connections.svg](images/connections.svg); all of it is built.

- **The role** is a subordinate's while `upstream.json` exists in the server folder. Every `welcome` names it (`role`), and `/api/status` shows it.
- **Only a main can be an upstream.** The link checks the role in every `welcome` it gets and refuses any but `main`, a missing one included (a server from before roles): subordinates form a star of depth one around a main, which cannot hold a cycle. A main that answers as a `subordinate` stepped down to another main: the link looks for the new main ("Switching the main"). A main that steps down closes the sessions of its own server-kind devices; its subordinates reconnect, read `subordinate`, and look for the new main.

### Linking

- **How a server is told to link:** `POST /api/upstream {"code": "<the main's pairing code>"}` on its page's API, guarded like every route, which the page's Upstream server section sends with the code pasted or scanned. It answers once the pairing is done (within 30 s) with the link's status; `GET /api/upstream` reports the status at any time, with `devices`, the number of the main's devices this server admits, and `subordinates`, the number of servers linked to this one. A halted link, or a link to the wrong main, is undone by unlinking (below).
- **A main with subordinates of its own does not link** (409 `hasSubordinates`): only a main may be an upstream, so its subordinates would stop for good, and every device's way to them with them. Its page says so and offers no link until they are removed. Should one pair while it links, its sessions are closed (`Hub.CloseServers`): it reconnects, reads `subordinate`, and looks for a main among its family's servers, finding none. Handing the role on with its family is a switch of the main ("Switching the main", below), not a link.
- **The steps:**
  1. parse the code as the client package does (the vectors pin it), and refuse this server's own code;
  2. reach the main and `pair` as a device of kind `server`, named with this server's name;
  3. check the `welcome`: the code's `serverId`, and role `main`. Otherwise close the session without confirming, which undoes the pairing on the other side;
  4. save `upstream.json` (0600): the main's id, name, ICE password, fingerprint, port and hosts, the device id and its token;
  5. confirm the pairing by asking for the app list (`apps`), and start the link.

  The pairing is confirmed only after the token is saved, so the main never keeps a pairing whose token this server lost. If the confirmation fails, `upstream.json` is deleted again: the main drops an unconfirmed pairing.
- **Refusals:** 400 `invalid` (not a pairing code, or this server's own), 409 `exists` (already linked), 409 `busy` (a link is being made or undone), 409 `hasSubordinates`, 502 `upstream` with a `reason`: a protocol code (`badSecret`, `blocked`, `unknownApp` from a main too old to know server-kind pairing), `connectTimeout`, `closed`, or `notMain` and `serverChanged`.

### Sessions to the main

- **One link session** (a `hello` without app), which is told the main's app list on asking and on every change, and **one session per app** the main lists (a `hello` for that app with the same token). A subordinate with N apps holds N + 1 of the main's 64 sessions.
- **The app list:** the subordinate adds to its own `apps.json` every app the main lists that it lacks, with the main's name for it, gives every app the main lists the main's quota (`apps {apps: [{id, name, quota}]}`, at every list and every change; a quota out of this server's range is left as it was, and logged), and syncs exactly the main's apps.
  - An app the main drops stops syncing; its data and its entry here stay, and its base (`.wss/link.json`) is forgotten, so a later return counts as a first link. The base goes only once the app's worker has stopped (the link session waits for it), so a sync in flight never saves it again.
  - An app removed here while the main still lists it comes back with the next list (a reconnect, or a change on the main). Its sync with the main stops before anything is removed (`Hub.RemoveApp` calls `Deps.RemovingApp`, which is `Link.AppRemoving`: the app's worker is stopped and waited for, and none starts until the removal ends), and then the link forgets what it held of the app (its base, divergence and the main's revision as last heard), and tells the main that no divergence of it waits. So its return is a first link, the empty set here taking the main's; the base kept in memory once made the empty set look like the deletion of everything the main held, and pushed it (fixed 2026-11-14).
  - Apps only the subordinate lists are not synced.
- **Every `welcome`** must come from the main's `serverId` with role `main`; one with role `subordinate` sends the link looking for the new main, and any other stops it. Its `term`, when higher than the one this server knew, is kept.
- **Hosts** come from the pairing code first; the hosts of each link session's `welcome` replace them.

### Transport (`internal/wsclient`)

- **A Go port of the client package:** `BuildAnswer` (`serverAnswer.ts`), `ParseCode` (`PairingCode.ts`), framing and flow control (`ChannelFraming.ts`), and `Client` (`ServerClient.ts`): one request at a time, `changed`, `diverged`, `resolved` and `apps` pushes to listeners, a server code it does not know read as `internal`. A `put` reads its body from an `io.Reader` as the channel takes it, and a `get` writes the body to an `io.Writer` as it arrives, so the link never holds a document in memory. It plays every vector file.
- **`Dial`:** Pion is the offerer. One data channel labelled `workspace`; the answer is built from the target with a fresh ufrag; no ICE servers; UDP4 only; Pion's own mDNS off; loopback candidates on, so a main on the same machine (and the tests) can be reached at `127.0.0.1`. The channel must open within 10 s.
- **Hosts become candidates:** each `.local` name is resolved first with `mdns.Resolve`, a legacy query (sent from a port other than 5353) multicast on every LAN interface, which responders answer by unicast. It collects A records until 150 ms after the first answer, for at most 1.5 s. The resolved addresses come first, then the IPv4 addresses of the list; a name that does not resolve is left out. The list is plain data, so another kind of host (`127.0.0.1` for the same machine, say) is one more entry.
- **Pion objects are never closed inside a Pion callback:** the channel's `Close` closes on a goroutine of its own.

### Reconnecting

- The link session's end ends every app session, and all reconnect after a backoff: 1 s, doubling up to 30 s, back to 1 s after a session that lasted 30 s; a minute after `blocked`. An app session that fails alone retries alone, with the same backoff.
- An address change (the 30 s check) reconnects at once.
- **The link stops** (until the server starts again; the status says why) on `badToken` (the main removed this server), `unknownApp` on the link session (a main too old), `protocol`, a `welcome` with no role (`notMain`: a server from before roles), and another server at the main's addresses (`serverChanged`). A main that answers as a subordinate does not stop it: the status says `mainSteppedDown` while the link looks for the new main, and it keeps trying with the backoff.

### The sync rule

No server merges, a subordinate included: the devices do, as they always have. The link compares an app's set document by document, by key, three ways: the **base**, this server's set (L) and the main's (U). The base is the set of etags (key → etag) on which L and U last agreed, kept in the app's `.wss/link.json` with the main's server id (the files plan's decision 6: a set can be large, so it is not in `upstream.json`). An etag is the SHA-256 of the bytes on both sides, so a document copied either way keeps its etag, and nothing is rewritten in circles. Per key, with its etag in the base `b`, on L `l` and on U `u` (absent where it does not exist):

| Situation | Action |
| --- | --- |
| `l` = `u` | agreed: the base takes it |
| only U moved (`l` = `b`) | **pull:** U's document (or its deletion) is written into L |
| only L moved (`u` = `b`) | **push:** L's document (or its deletion) is written on U |
| both moved, differently | the two **diverged**: nothing moves until a device chooses which set to keep (below) |

- **A first link** (no base: never agreed, an app the main dropped and listed again, or a base agreed with another main): a side whose set is empty takes the other's whole, both ways; two different non-empty sets have diverged. Keys on which they already agree are agreed either way.
- **A divergence waits for a device's choice** (the user's decisions, 2026-11-13; "Divergence" below): L keeps its own set, the app stops syncing with U both ways, and every device of the app, on L and on U, is asked to keep U's set or L's, the whole of it (the files plan's decision 5). Nothing is merged on a server.
- **In step ends the round:** a sync first checks whether L's revision and U's (as last heard) are both the revision of the base, with no link transaction in flight; then there is nothing to read on either side, and nothing is listed. This is what ends a loop: the link's own commits on L come back to it through `Deps.Changed` and find the two in step. Its commits on U do not come back at all: U tells every session of the app but the committing one, and the link takes U's new revision from the `committed` answer.
- **Writes are transactions,** all of an app's pulls in one on L (`LinkTx`) and all its pushes in one on U (`begin`, `put`, `delete`, `commit`), every change naming the etag it expects (`ifEtag`). So a device that wrote first on either side costs a decision, never a document: the commit is a conflict (or a `get` is `stale`), and the link lists again and decides again, a few times at most; whoever wrote also left a reason to sync again (a `changed`, or a commit on L).
- **Resumable:** both transactions are recorded in `link.json` (`pull`, `push`: the transaction id, the etag of each body it puts and the keys it deletes) before they start, so one cut off, by a dropped connection or a restart, resumes where its bodies stopped: `begin` with the same id reports what arrived, and the bodies go on from there. A transaction is taken up again when every change it staged is still wanted (a staged change cannot be unstaged, so one no longer wanted means a new transaction). Its bodies recorded at the etag wanted now resume; a body staged for another etag is first replaced by a `delete` of its key (at once on L; on U the `staged` answer confirms it), and only then does the record name the new etag, under which the new body is put from 0. So a body staged for one etag is never resumed as another's (a body mixed from two versions), even after a cut between recording the new etags and staging the new bodies, and a large body keeps what it staged when a device changes a small document of the same transaction. A transaction the plan no longer writes through (a divergence dropped the pushes, say) is aborted, and its record goes with it; so is one left once the two are in step without it, so its staging goes at once rather than at its expiry.
- **Streaming:** a pull writes what U's `get` sends straight into the staging file on L, and a push reads L's file as the channel takes it (`fsutil.OpenShared`, so a commit on L may replace it meanwhile, and the read still sees what it opened). No document is ever whole in memory, on either server.
- **When the link decides:** at every `welcome` of an app session, at every `changed` on it, and at every new revision on L (`Deps.Changed` wakes that app's worker). The `welcome` is what pushes the writes made on L while U was out of reach, or while the link was down: nothing is queued, since L's current set is all there is to push. A worker runs one sync at a time; changes that arrive meanwhile make one more sync.
- **A key U lists that is not valid here** (a main of another version, say) is left out, and logged: it can be neither written nor compared. In step is then judged on what L can hold: U's revision, as the link listed it, is compared with the base as the revision of U's set without those keys (`setComparable`). A `changed` from U names the full revision again, so the next sync lists once more before it finds the two in step; and without it, a main holding such a key would never be in step, and a planned switch would always fail.
- **The base after a sync:** what both sides agree on is agreed, whatever the base said; a key neither side holds is agreed to be gone; a write that commits records its keys at the etags the commit reports (`committed`'s documents on U, the store's on L), never the ones the link meant to write, so a body that is not the one meant shows as a difference the next sync settles (and is logged). The new base is saved only once a transaction commits: one saved before would tell a later sync that what no commit has written yet was agreed.
- **A lost base record:** a crash between a commit and saving the base, or a save that fails, leaves the base before the commit and the transaction's record. When the transaction is gone on the side it writes (a `begin` of its id there finds nothing staged, as after its commit), the next sync takes the record as the base of each key where that side holds the etag it put, or lacks the key it deleted (`effectiveBase`). Without that, a device that changed one of those keys in the meantime would make the key look changed on both sides: a false divergence, which U's set would win. The record of a transaction still open there is not used: it never committed, and a side that came back to the etag it names would otherwise hide a real divergence; nor does a record outlive the plan that replaced it (see "Resumable"). Left open: a key the transaction wrote and the other side changed again before the next sync is still taken for both moved.
- **The quota** is U's: L takes it with the app list (above), so both refuse the same sets. A pull that would still take L past it (a quota U changed meanwhile, say) is refused there; L's own changes are pushed all the same, then the sync fails, the app's session to U ends, and the app reconnects after the backoff (1 s doubling to 30 s), listing both sets again each time, until the pull fits.
- **The loss window:** the choice after a divergence replaces one server's whole set with the other's. A change only the replaced set held comes back when the device that wrote it merges and writes again, which it does at the `changed` that follows, or when it next connects. If that device has vanished first (wiped, or never opened again), the change is lost: the store keeps no previous versions since stage 1 of the files plan (2026-11-13). The person who chose saw which server's copy they kept.
- **Logged:** every pull and push, with the keys; a divergence, with why and the keys; a choice, who made it, and its outcome; a decision taken again.

### Divergence

When the two sets diverged, the subordinate (L) keeps its own, its main (U) its own, and the devices choose (the user's decisions, 2026-11-13: keep the subordinate's own, pause, ask every device on either server, the first answer wins, no merging; L36–L41). The contract is in [protocol.md](protocol.md), "Divergence" and "Divergence between servers".

- **Recording it** (`diverge.go`): the sync that finds the divergence aborts the link's transactions in flight, keeps the base as it was, writes `.wss/diverged.json` on L (`{since, reason, keys, main: {serverId, name}, keep?, chosen?}`: when, `changed` or `firstLink`, at most 10 of the keys that differ, and the main it diverged from), logs why, and ends the app's session to U. The app's worker then waits, as for an app the write guard stopped, with no session to U: no pulls and no pushes for that app; other apps go on, and the devices of each server go on syncing with it.
- **Telling the devices:** the hub lists every divergence that waits for a choice in the `welcome` of the app and pushes `diverged` at every change. On L the list is L itself (`Deps.OwnDiverged`, read from `diverged.json`). On U it is every subordinate that told U its copy waits, kept in U's `.wss/divergences.json` (`{servers: [{serverId, name, since, keep?, chosen?}]}`), each named as it last announced itself; a server removed from U, or one no longer its own server-kind device (U stepped down), is left out, and the file is removed when U links to a main (`Hub.ForgetDivergences`). A session holds a `diverged` pushed before its `welcome` and sends it right after, since the `welcome`'s list may have been read before the push.
- **Telling the main:** L's link session sends `diverged {app, since}` for every app U lists when it opens, and again whenever the state changes (`divTell`): `since` while the app waits for a choice, `null` otherwise. U takes the first `since` it hears for that server as a new divergence, and forgets the server's entry at `null`; a `since` other than the one it holds replaces the entry, and a choice made for the one before with it.
- **A choice on L** (`Hub.Resolve` for L's own id → `Link.ChooseOwn`): under the link's `divMu`, the first one is written into `diverged.json` (`keep`, `chosen`); the hub tells L's other sessions `resolved` and the list without L; the worker is woken, and the link session tells U `null`. A later one finds a choice made and changes nothing.
- **A choice on U** (`Hub.Resolve` for a subordinate's id): under the app's lock, the first one is written into `divergences.json`, the other sessions are told `resolved` and the list without it, and it is pushed to L's link session (`resolve {app, keep, since}`) if L is connected. Otherwise it waits in the file, a restart of U included: when L next tells `diverged` with that `since`, U pushes it again. L takes it if no choice was made there for that divergence (`Link.takeChoice`: the same `choose`, which then tells L's sessions through `Hub.OwnResolved`), ignores it otherwise, and tells U `null` either way, which drops U's entry.
- **First wins, as L sees it:** every choice goes through `choose` on L under one lock, and `diverged.json` keeps the first. A choice made on U while L is away loses to one made on L meanwhile; the devices on U were answered `resolved`, and then sync the set L kept.
- **Carrying it out:** the worker opens a session to U, and `sync` plans the choice with `whole`: every key where the two sets differ goes the chosen way, as one ordinary transaction, keep `main` pulled into L through `LinkTx`, keep `subordinate` pushed to U through the client's transaction (it counts against the write guard, like any push). It is recorded in `link.json` and resumable like any pull or push, across a dropped session or a restart; a device that writes meanwhile on either side costs a decision taken again (a conflict, or `stale`), and the plan is made again from both sets as they are then. Once it commits, the base is what the commit settled, `diverged.json` goes, and the next round syncs by the table. Should the two already hold the same set when a choice comes, the base takes it, and nothing is written.
- **Restarts:** `diverged.json` on L, with the choice once made, and `divergences.json` on U, with a choice that has not reached L, survive a restart of either server. A restart of L while it waits keeps the app waiting; one while it carries a choice out resumes the transaction.
- **The main L follows changes:** a `diverged.json` from another main is no divergence from this one, as a base agreed with another main is no base (F5): it is removed the first time it is read, and the first link with the new main compares the two sets afresh. A forced switch forgets L's divergences with its bases (the new main has none), and so does unlinking; the hub is told, and pushes the lists again.
- **A planned switch** waits for the whole family (L41): it is refused while any app of L diverged, a choice waiting or being carried out, while U's sessions of an app last told L that another subordinate's divergence waits (`ErrDiverged`, 409 `diverged` on the page's API), and, when U hands its role over, while U keeps any divergence of a subordinate, a choice not yet taken there included (`hub.ErrDiverged`). The documents are not the same, and waiting would not make them so. One case is left: a choice made on U for another subordinate that is away is not visible to L, so a switch that L starts goes through, U's step-down forgets the choice, and that subordinate, linking to the new main as a first link, finds whatever still differs and its devices are asked again.

### The write guard

A last line of defence against a write loop, as in the client package's engine: the link counts **one app's pushes to U**, each one transaction committed there (a push U refused with a conflict does not count), over a sliding minute, and a push past 10, checked before it starts, stops that app's sync, both ways, **until it is resumed on the page or the server starts again** (a restart, a reset or a data-folder move opens a new link). Its session to U closes; the documents on both sides stay as they are; its devices go on syncing with each other through L. The log says so, and `GET /api/upstream` lists the app under `stopped` (and leaves it out of `apps`). Other apps go on, and the link is not halted. A resume or the next start settles what moved meanwhile by the table above.

- **Resuming** (`Link.Resume`, the page's "Resume" beside each stopped app, `POST /api/upstream/resume {app}`; L35): the guard forgets the app's stop and its pushes in the sliding minute, so the app has a fresh 10, and the app's worker, which waits while the app is stopped instead of ending, opens a new session to U at once; its `welcome` syncs the app by the table above, which usually pushes what L changed, each change with its `ifEtag`. While U is out of reach, the stop is cleared all the same, and the app syncs at the next `welcome`. The guard itself does not change: a loop that goes on stops the app again after 10 more pushes per press. Other apps, their counts included, are left alone. Logged, saying whether the app syncs now or once U is reachable. A halted link syncs nothing until the server starts again, so it resumes nothing (502 `upstream` with the reason it halted, as `invite`); the page shows no button there.
- **Pushes only:** every other write the link makes is into L, and needs a new revision on U; every new revision on U comes from U's devices, each under its own guard, or from L's pushes, under this one. So counting pushes bounds the link.
- **Why it stops rather than slows:** a loop is a bug, and one throttled to 10 writes a minute would go on replacing both servers' documents for ever, unseen. Stopping keeps both still and shows it on the page.
- **Why both ways:** with pushes stopped and pulls going on, every change on U would replace L's unpushed document again. Stopped, L's copy stays current on L, where its devices still sync.
- **Per app:** each app is its own set and its own loop, as each app has its own engine on a device.

### Handing subordinates out to devices

Devices pair only with the main, which hands its subordinates out to them; a phone that travels with the laptop then syncs with the laptop's server without a second pairing. The contract is in [protocol.md](protocol.md), "Servers" and "Subordinates: fallback servers for a device".

- **Announcing:** every link session, once the main's `welcome` shows that it lists subordinates (the field is there), sends `announce` with this server's connection data: its id, name, ICE password, fingerprint (uppercase colon hex), UDP port and hosts (the `.local` name, then at most 15 LAN addresses), read live. It announces again on the same session after the page changes its name or UDP port (`Link.SelfChanged`); an address change reconnects the link, and the new session announces. The main keeps the data with the server-kind device in `devices.json` (written only when it changed) and lists it in every `welcome` it sends as a main, of every session, `[]` included.
- **The device registry:** the main answers `announce` with its devices (id, app, kind, name, token hash, pairing time, and what a server-kind device announced) and the devices removed (tombstones), and pushes the registry again to every link session that announced after each change it makes: a pairing, its undoing, a removal, an app's removal, a subordinate's new announce. The answer goes out through the same push, under the same lock, so the last registry a subordinate hears is the newest. Server-kind devices are in it too ("Switching the main"): they let the servers of a family reach one another, and a subordinate that becomes the main admit them all.
- **Admitting the main's devices:** the link keeps the registry in `upstream.json` and hands it to the hub (`Hub.Replicate`), which makes it the registry's replica: `Devices.Authenticate` and `Get` see it after this server's own devices, `List` and `devices.json` never do. A device of the replica is admitted to its own app by the SHA-256 of the token it presents: tokens are 32 random bytes, so the hash proves the token with no salt, and a stolen hash does not connect. A server-kind device of the replica (another server of the family) is admitted to a link session only, where it may send `term` and nothing else (a `family` session in the hub). `upstream.json` is read at start, before the main is reached, so the main's devices sync here while it is away, a restart included. A device the registry drops has its sessions closed at once; its in-flight `put` gets `badToken`.
- **Removals reach a subordinate late:** while the link is down, a device removed on the main is still admitted here, until the next link brings the registry. Accepted for a single user. A subordinate the main removed (its link halts on `badToken`) clears the registry: the main's devices are no longer admitted here.
- **`pair` on a subordinate** is `notMain`, of either kind, before anything is checked: no secret is used and no failure counted. So its page issues no code of its own: `POST /api/pairing` answers 409 `notMain`, and Devices points to the main, or to a code of the main for a browser here (`invite`, below).
- **`forget` through a subordinate** closes the device's sessions here and does not reach the main, which keeps the device until it is removed on its page; this server cannot delete a device of the replica.
- **On the device** (the client package): see "The client package" below and its README. The engine connects to the main and every subordinate at once and prefers the main.

### A browser on the subordinate's computer (`invite`)

A browser on the laptop pairs with the main like any device, but the laptop's page cannot show a code of its own (`notMain`), and the user may have no phone at hand. So the subordinate asks the main for one over its link session: `invite`, answered with `invited {code, expiresAt}` (see [protocol.md](protocol.md)).

- **The main issues an ordinary code,** the one its page's Pair a device shows (`Hub.Invite` calls `Deps.Invite`, which is `IssuePairing`): a new secret, good for one pairing within 10 minutes, which revokes the code on the main's page. It logs that it issued one for the subordinate.
- **It names no app,** as no code does: the browser's app names itself in `pair`. An app the main does not serve gets `unknownApp`, and the refusal shows on the main's page, which offers to add it.
- **Only a link session of a server-kind device** gets one; any other session gets `protocol` and closes. A subordinate answers `notMain`, and the session stays open.
- **On the subordinate,** `POST /api/upstream/invite` hands the request to the link session's loop, which asks the main beside the loop (10 s at most) and checks that the code is the main's own. It needs the main: a link that is not connected now is 502 `upstream` with reason `unreachable` at once, a halted one names why it halted, and a server that is not linked is 409 `notLinked`. The answer has the shape of `POST /api/pairing`'s, QR code included.
- **The browser pairs with the main,** hears of the subordinate in the main's `welcome`, and is admitted on the subordinate once the main's registry reaches it, at once while linked.

### Unlinking

`DELETE /api/upstream` ends the link, and the server is a main again:

1. the link stops (nothing syncs, reconnects or saves `upstream.json` any more);
2. `upstream.json` is deleted, and every app's base with it (`link.json`); if the deletion fails, the link starts again as it was, and the request fails;
3. the replica is cleared (`Admit(nil)`, `Hub.Replicate(nil)`): the main's devices are no longer admitted, and their sessions here close;
4. if the link was connected, a link session of its own says `hello` and `forget` to the main (5 s at most), which deletes the server-kind device: the main stops listing it in `welcome`, so devices drop it as a fallback.

- **The data stays:** the apps taken from the main stay in `apps.json` with their documents, and the main keeps its own.
- **The answer** is `{"forgotten": bool}`: whether the main no longer knows this server, because it was told now, or had removed it already (the link halted on `badToken`). A main that could not be told keeps the server-kind device until it is removed on its page; the page says so. A device that still tries this server as a fallback meanwhile gets `badToken` here, which does not stop its sync (only the paired server's refusals do).
- **Refusals:** 409 `notLinked`; 409 `busy` while a link is being made or undone. Linking again afterwards needs a new code.

### The page

- **The Upstream server subsection** (in Server; see "The web page" above for where and why):
  - on a main, what linking does, a field for the main's code and "Scan with the camera"; before linking it says that nothing is deleted, and that where both servers hold a document the main's wins and this server's goes to its saved versions (stale since the store keeps no history: the main's replaces it; stage 4 of the files plan reworks the page). A main with subordinates gets an explanation instead of the field;
  - on a subordinate, the main's name and id, the state (connected, not connected, looking for the new main, or stopped, with what the reason means and what to do), when it was linked, the last sync, the apps synced, the apps the write guard stopped, each with a "Resume" button (named "Resume <app>" for a screen reader, with no question; none on a halted link) and a line saying that Resume syncs an app again at once, that a restart resumes them all, and that an app that keeps writing that often stops again, and the number of the main's devices admitted; then "Pair this computer's browser" (not on a halted link, which reaches no main), "Make this server the main" and "Unlink", whose confirmation says that no data is deleted;
  - "Make this server the main" asks first (the documents are made the same first, the main hands its role over, devices follow without pairing again, nothing is deleted) and switches as planned (`POST /api/upstream/promote {}`); while the main is out of reach, or when the planned switch finds it so (502 `upstream`, reason `unreachable`), it asks again with what happens next (both act as main until they meet, devices sync with whichever they reach, nothing is lost, the one made main later wins and the old main steps down by itself) and switches by force (`{"force": true}`);
  - on a main that took the role over, the servers it still waits for, by name, with a line saying that one gone for good can be removed under Devices;
  - results go to a `role="status"` line, so a screen reader hears them; after linking, unlinking or resuming the focus moves to the subsection's heading, since the button that had it is gone. A resume answers with the link's status, so the list follows at once; a refused one reloads it (another tab, or a restart, may have resumed the app) and says why.
- **Devices on a main** lists server-kind devices first, under "Linked servers", by the name they announced (their pairing name until they have), with their server id, hosts and UDP port; removing one says that the server on that computer stops syncing with this one and that its data stays there. "Make it the main" (once the server announced itself) asks first and hands the role over (`POST /api/upstream/hand-over {"serverId"}`, 409 `notConnected` when it has no link session now). `GET /api/devices` gives a server-kind device `kind: "server"` and `server: {serverId, name, port, hosts}`, what it announced, without its password and fingerprint.
- **Devices on a subordinate** shows no "Pair a device": it names the main, says that devices pair there, and offers "Pair this computer's browser".

### Switching the main

What "main" means: the server whose copy wins a divergence, the one devices prefer, and the source of the device registry. The data does not depend on it, since the devices merge, so a switch is cheap for the data; what it settles is who the main is. The contract is in [protocol.md](protocol.md), "Switching the main".

- **The term:** a family's term starts at 0 and goes up by one every time a subordinate becomes the main. A main keeps its own in `family.json`, a subordinate its main's in `upstream.json`; every `welcome` names it (left out at 0). Of two mains, the higher `(term, serverId)` wins: a server that meets a main higher than its own main follows it, and a main that meets one steps down and links to it. A main never stops being one without a successor: there is no plain toggle.
- **Credentials between servers:** a subordinate reaches its main with its server-kind device and token, as before. Since the registry carries every server-kind device's token hash and connection data, every server of the family admits every other to a link session, and knows where to reach it. A subordinate that becomes the main keeps the device and token it had (`family.json` `credential`): the others, holding the old main's registry, still admit it, so it can tell them it is the main now. Its former main has no credential anywhere (a main never paired with anyone), so the new main mints one for it, a server-kind device of its own (`Registry.MintServer`), and keeps the token (`family.json` `handout`) until it hands it over. This needs no new kind of credential and no new pairing: the one new token is minted by the one server that needs to admit it.
- **Becoming the main** (`Link.Promote`):
  1. planned only: the link must be connected, no divergence may wait anywhere in the family (409 `diverged`, see "Divergence"): neither this server's own (a choice waiting or being carried out) nor, as the main last said on this server's app sessions (`welcome` and `diverged`), another subordinate's, and every app it syncs (but those the write guard stopped) must hold the same set on both servers, as both last agreed (`inStep`, within 10 s, else 409 `notInStep`): this server's revision and the main's, as last heard, both the revision of the base, and no link transaction in flight;
  2. the link stops;
  3. the main's registry (the replica, with its removals) becomes this server's own (`Hub.Adopt`, a merge, below), less this server's own entries: every device the main admitted is admitted here, and no device pairs again;
  4. a server-kind device is minted for the former main, with its connection data from `upstream.json`;
  5. `family.json` is written: the term (the old one plus one), when, the credential, the handout, and the servers it waits for to follow it (the former main and every other server-kind device it adopted);
  6. `upstream.json` is deleted, every app's base forgotten (`link.json` removed: a main has no base, and should it follow another main later, that is a first link, never a comparison with what it agreed on long before), and every divergence with it (`diverged.json`: it was from the main this server no longer follows), and the replica cleared (its devices are this server's own now);
  7. planned: it tells the former main `term` at once (15 s at most) and reports whether it followed; either way the family watcher (below) goes on.

  A crash between the steps leaves a server that is still a subordinate (until step 6) and does it again when asked; minting again re-keys the same device.
- **Telling the family** (the watcher, on a main whose `family.json` holds a credential): round after round, at once after becoming the main and after a network change, then with the link's backoff up to 10 times its longest (5 minutes), it says `hello` to every server it waits for that is not linked to it now, with its credential, at the same time. A server linked to it, or removed on its page, is no longer waited for. From each `welcome`:
  - a main higher than this one: this server steps down and links to it, with the credential it just used;
  - otherwise: it sends `term`, with the handout when the server is its former main. A server that answers that it follows this one is no longer waited for.

  When it waits for nobody, the credential and the handout are dropped; `family.json` keeps the term.
- **Following a higher main** (`Link.Follow`, the hub's `Follow` for a `term` frame): when the sender's `(term, serverId)` is higher than the main this server follows,
  - a subordinate points `upstream.json` at the sender (its connection data, the term) and reconnects, with its own credential. A base records the main it was agreed with, and one agreed with another main is no base, so the first sync with the sender follows the first-link rules: sets that are the same agree at once, an empty side takes the other's, and two different non-empty sets diverge, and wait for a device's choice; a divergence from the main it followed before is moot, and goes;
  - a main steps down (`stepDown`): it writes `upstream.json` for the sender, with the credential given, or with the one it kept, and `handover` set; deletes `family.json`; starts the link; and closes the sessions of its own server-kind devices but the sender's, whose answer must still leave. Its first `announce` there carries its own registry; once the new main answers, it forgets its own devices (`Hub.ClearDevices`) and admits them on the new main's word, like any subordinate. Its documents then follow the sync rule, with no base (it forgot its own when it became the main): in step after a planned switch, so they agree at once; otherwise, where both sets hold documents and differ, the two diverged, as on any first link: the server that stepped down keeps its own, and the devices of the family choose which to keep (see "Divergence"). It also forgets what its own subordinates told it of their divergences (`Hub.ForgetDivergences`): they follow the new main.
- **Looking for the new main** (`findMain`, a subordinate whose main answers as a subordinate): it says `hello` to every server-kind device of its registry with connection data (but itself and the old main) at once, with its own credential, and follows the highest that answers as a main above the main it followed. If none answers, it tries again with the backoff.
- **Forced:** the same, without the main and without the documents' check (a divergence included). If the old main still runs, both act as main until they meet; each admits the family's devices, and devices sync with whichever they reach. When the new main reaches the old one, the old one steps down by itself. Two subordinates made the main apart, at the same term, settle by the server id: the lower steps down when either reaches the other. The same path goes on without a PC that is gone for good: remove it on the new main's Devices, and it is no longer waited for.
- **The registry merges by union with tombstones** (`Registry.Merge`): a removal is kept as the device's id and when (`removed`, the newest 1,000), on the main and, through the registry, on every subordinate. A device id is random and never reused, so a removal is final, and the merge needs no clock: the result is every device of either side, less every id removed on either side, each removal kept. A device both sides have keeps the merging server's own entry; a server it already has under another id (two credentials minted for one server by two mains) is taken once; an entry for the merging server itself is left out. So a device paired on either side during a split is admitted on both afterwards, and one removed on either side stays removed. It runs when a subordinate becomes the main (its main's registry) and when a main takes over the registry of one that stepped down to it.
- **The main's `devices.json` holds token hashes, as a subordinate's replica does**: no server holds another's tokens, so a new main admits the old main's devices by the same hashes. The tokens servers hold are their own credentials: in `upstream.json`, and on a promoted main in `family.json`.
- **Devices** keep their pairing through every switch. A device paired with the old main dials it and its fallbacks at once: the old main answers as a subordinate (or not at all) and the new main as a main, which the client package then makes the server it is paired with (the README, "Following the main"). Devices paired with the new main learn the old one as a fallback from its `welcome`, since the old main is one of its server-kind devices.
- **`takeOver`** ("Make it the main" on a main's page): the main first refuses while any of its subordinates' divergences is in its `divergences.json`, a choice waiting or one made here that has not reached the subordinate (`hub.ErrDiverged`, 409 `diverged`); otherwise it pushes `takeOver` on the subordinate's link session (`Hub.TakeOver`); the subordinate runs a planned switch (`Link.Promote`) beside its link; the main's page waits until the main is a subordinate (30 s at most).
- **Logged:** the switch, the term, every server told and whether it follows, a step-down, the handover, and the search for a new main.

### Away from home: the network (not checked yet)

Two risks of the road decide whether a device reaches the laptop's server at all. Neither has been checked on a real network (see "What has been verified, where" and [TODO.md](TODO.md)), and nothing was built for either: the user chose to build on what can be checked on one machine and to check these at the end (2026-11-12). The host list is plain data (in the pairing code, every `welcome` and every entry of `subordinates`), so a fix that needs another kind of host is one more entry.

- **No network at all.** A server's hosts are its `<uuid>.local` name and its LAN IPv4 addresses. With every network gone only `127.0.0.1` is left, which no device is told, and browsers (Chrome at least) gather no loopback candidates, so a browser on the laptop may not reach the laptop's own server. The app keeps working from its own storage meanwhile, and syncs once a network is back. A possible fix is a `127.0.0.1` host for a browser on the same machine, if browsers pair a loopback candidate at all; the Go client already dials with loopback candidates.
- **The travel network.** Hotel and café Wi-Fi usually isolate clients from each other, which no host list gets past. On a phone's hotspot the laptop has an address the phone has never seen, so a phone paired at home finds the laptop's server only by its `.local` name, over mDNS on the hotspot; once connected, the server's `welcome` gives it the new address, and the client package keeps it for that server. Whether iOS and Android resolve a `.local` name on their own hotspot is the open question.

## Storage

### The server folder and the data folder

| OS | Server folder (default) |
| --- | --- |
| macOS | `~/Library/Application Support/workspace-server` |
| Windows | `%APPDATA%\workspace-server` |
| Linux | `$XDG_DATA_HOME/workspace-server`, default `~/.local/share/workspace-server` |

`-dir <dir>` names another server folder (for tests and a second server). The commands make it absolute and clean first, so a relative or trailing-separator `-dir` still matches the default folder (the main server) and the paths the page compares. The data folder is `config.json`'s `dataDir`, or the server folder when it is empty.

- **The data is in `WSS-Data`,** a folder the server keeps in the data folder (`wssDir`, `appsRoot` in `datafolder.go`), so that a person finds it and its `README.txt` tells them to leave it alone. `dataDir` keeps the folder the user chose; the server adds `WSS-Data` itself, the default data folder (the server folder) included.
- **A `WSS-Data` folder chosen itself** is used as is, with no `WSS-Data/WSS-Data`: a folder named `WSS-Data` (in any case on Windows and macOS, whose file systems ignore it; exactly on Linux; paths compare the same way, `foldCase` in `datafolder.go`, since `filepath.Rel` ignores case on Windows only) that holds `apps` or `README.txt`, or nothing at all. The last case, beyond holding `apps` or `README.txt`, keeps one folder across a deletion: a reset that empties a chosen `WSS-Data` removes it, and the restart that recreates it must not then nest a second one in it; a missing `WSS-Data` chosen for `create` is the data's folder for the same reason. "Nothing" ignores the files the systems leave in a folder someone looked at (`.DS_Store`, `desktop.ini`, `Thumbs.db`). A `WSS-Data` holding other files gets a `WSS-Data` inside.
- **`README.txt`** (`readmeText` in `datafolder.go`) is written when `WSS-Data` is made, and again at every start (`ensureDataDir`) and after a move if it is missing; an existing one is never written over. Deleting data removes `README.txt` and `WSS-Data` only when nothing else is left in `WSS-Data` but the systems' own files (`.DS_Store`, `desktop.ini`, `Thumbs.db`), which go with it; a `WSS-Data` that is a symbolic link or a Windows junction loses only its `README.txt`, and the link stays (removing it would make the restart create a local folder in its place).
- **No old layout is read:** an `apps/` straight in the data folder (before 2026-11-13) is neither data nor moved; there were no users to migrate.
- **`/api/status`'s `dataDir`** is the `WSS-Data` folder, which the page shows; `PUT /api/config`'s `dataDir` and `GET /api/data-dir`'s `path` are the folder the user chose.

```
server folder:
identity.json          0600, created once, never changed (a reset deletes it)
config.json            what the user may change, dataDir included
apps.json              the apps this server serves
devices.json           0600, paired devices
upstream.json          0600, the link to an upstream server (a subordinate only); the bases are per app, in .wss/link.json
family.json            0600, a main's term, and whom it waits for after it took the role over
server.log             the log; never above 1 MiB
server.lock            one server per server folder

data folder:
WSS-Data/README.txt                       what the folder is, for a person (English)
WSS-Data/apps/<id>/<folder>/<doc>.<type>  the app's documents, plain files
WSS-Data/apps/<id>/.wss/index.json        the set: etags, sizes, mtimes, revision, tombstones
WSS-Data/apps/<id>/.wss/journal.json      only while a commit is being applied
WSS-Data/apps/<id>/.wss/staging/<tx>/     an open transaction: tx.json and the bodies
WSS-Data/apps/<id>/.wss/link.json         a subordinate's base with its main, and its link transactions in flight
```

### The files

- **`identity.json`** is `{serverId, certPem, icePwd, mdnsName}`.
  - The certificate is ECDSA P-256 in Pion's `Certificate.PEM()` form, valid for 20 years. Clients pin its SHA-256 fingerprint.
  - Losing the file means every device pairs again. Copying the server folder (and the data folder) to another machine keeps every pairing.
  - On load every field is checked as it was made: `serverId` 12 and `icePwd` 24 characters of `[A-Za-z0-9]`, `mdnsName` a lowercase `<uuid v4>.local`, and the certificate. A damaged file stops the server with its path and the field (never the password's value) and is left untouched, instead of failing later as an rtc or mDNS error.
- **`config.json`** is `{name, udpPort, httpPort, dataDir}`, with defaults host name, UDP 47801, HTTP 47800 and no `dataDir` (the server folder). The page has the round number, since a person types it; devices take theirs from the pairing code. The defaults apply only to a new `config.json`: an existing one keeps its ports, so paired devices keep working. `dataDir` must be absolute.
- **`apps.json`** is `{apps: [{id, name, quota}]}`.
  - `id` matches `^[a-z0-9-]{1,64}$`. It becomes a path segment and never changes.
  - `quota` is the most bytes the app's documents may take together: 50 MiB by default (range 1 MiB–1 TiB). Zero means the default. The fields of the store before the files plan, `history` and `maxBytes`, are gone; a file that still has them logs them as unknown keys, and the app gets the default quota.
  - The list decides which apps may pair. It is configuration, not security: the device token is the security.
- **`devices.json`** is `{devices: [{id, app, kind, name, tokenHash, pairedAt, lastSeen, server}], removed: [{id, at}]}`, one entry per device per app, and the devices removed (the newest 1,000; see "Switching the main"). An old binary ignores `removed` and drops it at its next write. A server-kind device (`kind: "server"`, a subordinate) has `app: ""` and is admitted to every app; `kind` is left out for a device of an app. `server` is what a server-kind device announced, `{serverId, name, password, fingerprint, port, hosts}`, absent until it has; an entry of an app with one, or one that breaks the announce rules, stops the server like any invalid entry.
  - Times are RFC 3339 UTC, and are read into UTC whatever offset the file holds.
  - `lastSeen` is written at most once a minute per device. A write that fails is retried, and logged, at most once a minute too.
  - On load, an entry with an empty or repeated id, an unknown `kind`, an invalid app id (a server-kind entry must have none) or a `tokenHash` that is not 64 lowercase hex digits stops the server with the file's path and the entry's index; the file is left untouched, like an invalid `apps.json` or `config.json`. Dropping such entries instead would lose them at the next write.
- **`upstream.json`** is `{serverId, name, password, fingerprint, port, hosts, device, token, linkedAt, devices: [{id, app, kind, name, tokenHash, pairedAt, server}], removed: [{id, at}], term, handover}`: the main, this server's device there and its token in plain text (hence 0600, like a browser's pairing record), the main's device registry as it last came (hashes only) with its removals, the family's term, and whether this server, a main that stepped down, still has its own registry to hand over. An invalid file stops the server with its path and is left untouched, like the other registry files.
- **`family.json`** is `{term, since, credential: {device, token}, handout: {serverId, device, token}, pending: [<serverId>]}` on a main (0600: it holds tokens): the term, and after it took the role over, when, its credential from its time as a subordinate, the credential it minted for its former main, and the servers it waits for to follow it; the last three go once every server follows. Missing means term 0. An invalid file stops the server, like the others; it is read only while `upstream.json` is missing.
- **The store** (`internal/store`, stage 1 of the files plan, 2026-11-13) keeps each app's documents as plain files in the app's folder, and its own files in `.wss/` beside them.
  - **A document** is the file `<folder>/<id>.<type>`, named by a key: folder segments `[a-z0-9_-]` (at most 10), id `[a-z0-9_-]{1,64}`, type `[a-z0-9]{1,8}`, at most 255 characters in all, no Windows device name (`con`, `prn`, `aux`, `nul`, `com0`–`com9`, `lpt0`–`lpt9`) as a segment or an id. The type is part of the identity. A file whose path is not a key (uppercase, a dot in the name, too deep, `.wss` itself) is not a document: the store does not show it, and never writes over it or deletes it. Symbolic links, junctions and other reparse points are not followed, and are left alone the same way (see "Commit" for how a change that would need their path is refused).
  - **Etag and revision:** a document's etag is the lowercase hex SHA-256 of its bytes; the set's revision is the SHA-256 of the lines `<key>\x00<etag>\n` sorted by key byte for byte, `null` for an empty set. Servers holding the same files have the same revision.
  - **The index** (`.wss/index.json`) holds each document's etag, size and mtime (Unix nanoseconds), the revision, the set's size, the time of its last change, and the tombstones of deleted documents (`{key, at}`, the newest 1,000). A document created again drops its tombstone. The index is a cache of the files: **reconciling** walks the app's folder (skipping `.wss`), hashes again only a file whose size or mtime differs, adds a new file with a valid name and turns a vanished one into a tombstone. It runs when the app is first loaded, on demand (`Reconcile`, which the hub calls at every `hello` for the app), and before every commit. A file or folder it cannot read (another program holds it without sharing, or its permissions refuse the server) is logged and left as the index holds it: a file not yet in the index waits for a later reconcile, and the documents in a folder it cannot read stay. One such file never stops the app; what the reconcile could not read is remembered until the next one, for the commit (below). A file hashed within 3 s of its mtime is marked racy and hashed again at the next reconcile, until it is older, so an edit within the same tick of a coarse clock (FAT's 2 s) that keeps the size is still seen. A later change kept to the same size and the same mtime goes unseen; a person editing the folder by hand is not the expected case (the README asks not to).
  - **Reads:** `List` and `Head` answer from the index; `Open(key, etag)` opens the document through `fsutil.OpenShared` under the app's lock, and the caller reads it without the lock, from any offset. A document that is not there with that etag is `ErrStale`; a file that differs from the index, or no longer opens as one (it became a folder), is reconciled first.
  - **Writes are transactions:** `Begin(device, tx)` (tx: 16–64 of `[A-Za-z0-9_-]`, chosen by the client) opens one, or returns to it with what it has staged; a device has one per app, and `Begin` with another id discards the old one. `Put(key, ifEtag, bytes, offset)` returns a sink that writes the body straight into `.wss/staging/<tx in hex>/` (hex, so that two ids differing only in case never share a folder where case is ignored), hashing as it goes; `offset > 0` resumes a body of the same size from at most what arrived (the kept part is hashed again from disk), and `offset == bytes` only changes `ifEtag`. The body is fsynced when it is complete (the files plan's stage 0: not per chunk, not in the commit), and before every save of its progress: `tx.json` records the device, the times and each change with its `received`, saved at most every 5 s while a body arrives (`persistInterval`), and when it stops, so a restart resumes from there (a crash or power loss loses at most about 5 s of a transfer; a dropped session loses nothing, since its sink syncs and records as it closes), and it never counts a byte a power loss could take back (else a resume would hash the zeros left in its place, and the commit would store them). A body file shorter than that counts only what it holds. The syncs run outside the app's lock and the sink's, so ending a transaction never waits for the disk, and a sink closes its file before it waits to record, so the end removes the transaction's folder with no handle left in it (the end removes `tx.json` first, so a folder that will not go is never loaded again as a transaction). A put that resumes from what a body still being written received syncs that body before counting it. A put that is refused (a bad offset, the quota) leaves a body still being written for the same key alone; one that is accepted ends it. `Delete(key, ifEtag)` stages a removal. `ifEtag` null means the document must not exist. `Abort` discards.
  - **Commit**, under the app's lock: every body complete (`ErrIncomplete`), no change touching what that reconcile could not read (`ErrUnreadable`: a file changed by hand there would pass the `ifEtag` check against the index's old etag, and its rename, blocked past the commit point, would later write over the edit; the transaction stays open, the hub answers `internal`, and the same commit, once the file can be read, goes through or conflicts), every `ifEtag` matching the index after a reconcile (a `*ConflictError` with the revision and the current etags; the transaction stays open), every path free on disk (below), the quota (`ErrQuota`); then `.wss/journal.json` is written, fsynced, and flushed again through a handle of its own (Windows has no folder sync, and the rename into place must survive a power loss before the device hears of the commit), which is the commit point; then the bodies are renamed into place with `fsutil.Rename`, deleted documents are removed with the folders that leaves empty (up to the app's folder), the index is written, and the staging folder and the journal are removed. Before the commit point nothing in the set has been touched.
    - **Paths are checked before the commit point,** so that no journal is written that the disk will not take: every folder above a document must be missing or a plain folder spelt exactly as the key spells it (not a file, a link, a junction or another reparse point, through which a commit would write or delete outside the app's folder; nor `Docs/` for `docs` where case is ignored, which would put the document where no key names it), and the document itself missing or a regular file the index holds (a folder there, a link, or a file whose name differs only in case where case is ignored, such as `Notes.json` for `notes.json`, belongs to someone else). Otherwise the commit is `ErrBadPath`, and the transaction stays open. A delete of a document the index does not hold touches nothing. Renames replace a read-only document (`FILE_RENAME_IGNORE_READONLY_ATTRIBUTE` on Windows), and removing the folders a delete leaves empty stops at a link and at the app's folder.
    - **After the commit point,** the next operation on the app, or the next start, applies the journal again from the top: every step can run twice; a body already moved is hashed again where it lies rather than trusted to the journal. A rename or a removal that another program's handle blocks (Windows errors 5, 32, 33) is tried again for half a second, then fails, leaving the journal for later, and every operation on the app fails meanwhile. So that nothing stops an app for good, a change that fails for any other reason, or stays blocked for a minute, is dropped: it is logged, the rest of the journal goes on, and the index is reconciled with what the disk holds (the commit then reports an error, and a replay goes on silently). So an error from `Commit` after the commit point may mean the commit is partly applied and the transaction gone: the protocol answers it as `internal`, and the client lists again, never retrying the same transaction (protocol.md, "Transactions"). A failure to write the index only marks it unsaved, since a reconcile rebuilds it. Only a journal that cannot be removed at all keeps the app stopped.
    - Each step's file operation is a package variable, so the tests fail each one in turn and check the recovery.
  - **The quota** counts the documents' sizes. `Put` refuses at once a body larger than the quota, and one with which the set would exceed it (a replaced document's old size and a deleted one's count as freed; the sum saturates instead of wrapping around); `Commit` checks again. A set already over its quota (the quota lowered, files added by hand) can always shrink: a transaction is refused only when the set after it exceeds the quota and is larger than now (decided in the review of 2026-11-13).
  - **Staging** lives 24 hours after its last activity: `Sweep` removes what is older, anything in `staging/` that is no open transaction, and the temporary files a crash left of the store's atomic writes in `.wss/`. It runs when the server starts (`Recover`, which also applies leftover journals and reconciles every app folder) and every hour after.
  - **Side files** (`ReadSide`, `WriteSide`, `RemoveSide`): small files of the server's own beside the index in `.wss/`, written atomically: on a subordinate the link's `link.json` and `diverged.json`, on a main the hub's `divergences.json` (see "Divergence"). They are not documents, and move with the app's folder.
  - **Every open of a document** (reconcile, hashing, `Open`, the link's push, copying the data folder) goes through `fsutil.OpenShared`: one plain `os.Open` handle would block a commit's rename on Windows (stage 0).
  - **No history:** the store keeps no previous versions (the files plan's decision 4; no migration). A `history/` folder from before is ignored, and the user deletes it by hand; a `data.json` at the app's root is the document `data.json` of the new store.

### Atomic writes and the lock

- **Atomic writes:** all registry files use `fsutil.WriteFileAtomic`, which writes a temporary file, fsyncs it, renames it and then fsyncs the parent directory, so the rename survives a power loss. The directory sync is best effort (some network and FUSE mounts refuse it): once the file is renamed, a failed sync must not make a caller undo the write. On Windows it is skipped: a directory handle opened for reading cannot be flushed, and NTFS journals renames itself. The data-folder move syncs the folders it copied into and the ones it renamed into, the same way.
- **The lock:** `server.lock` uses `flock(LOCK_EX|LOCK_NB)` on Unix and `LockFileEx(EXCLUSIVE|FAIL_IMMEDIATELY)` on Windows.
  - It is held for the whole run.
  - The file is never deleted while a server could hold it.

## The web page (`internal/web`)

- **Address:** `http://localhost:<httpPort>/`, listening on `127.0.0.1` only. The browser, the log and the commands show `localhost`, which reads better; the program's own requests to the page (the probe, `stop`, the wait before opening the browser) go to `127.0.0.1`, so they never depend on how `localhost` resolves. A browser tries `::1` for `localhost` first, is refused at once (the page listens on IPv4 only) and falls back to `127.0.0.1`.
- **Content:** the page built from `client-wss/` (see above), embedded from `ui/dist`:
  - `GET /` is its `index.html`, run once per request as an `html/template`: `{{.Boot}}` in `<script type="application/json" id="boot">` becomes `{nonce, maxBytes}`, the CSP nonce and the add form's default quota (`apps.DefaultQuota`), under the page's old name until stage 4 of the files plan reworks the form;
  - `GET /assets/{file}` serves Vite's hashed files by extension (`.js`, `.css`, `.svg`, `.png`, `.woff2`); anything else, `index.html` included, is 404 `notFound`;
  - without a build, `GET /` is 503 in plain text, saying how to build it.
- **Language and theme** are chosen on the page, not by the server: it no longer reads `Accept-Language`.
- **Russian control names,** which the todo app's instructions quote: «Подключить устройство», «Приложения», «Устройства», «Сервер». Polish: «Sparuj urządzenie», «Aplikacje», «Urządzenia», «Serwer». They live in `client-wss/src/i18n/locales/`.
- **API routes** (JSON, lowerCamelCase keys):
  - `GET /api/status`;
  - `PUT /api/config`;
  - `GET|POST /api/apps`;
  - `DELETE /api/apps/{id}`;
  - `GET /api/apps/{id}/document` (a download named with today's date);
  - `GET /api/devices`;
  - `DELETE /api/devices/{id}`;
  - `POST /api/pairing`;
  - `GET /api/pairing/refusals`;
  - `POST /api/server/stop` with `{"removeAutostart": bool}`: stop (202);
  - `DELETE /api/data`: reset (202);
  - `DELETE /api/server`: stop and remove the server from the machine (202);
  - `GET /api/data-dir?path=`: check a data folder;
  - `GET|POST|DELETE /api/upstream`: the link to an upstream server, linking, and unlinking; `POST /api/upstream/invite`: a pairing code of the main for a browser on this computer; `POST /api/upstream/promote {force}`: this server becomes the main (`{followed}`; 409 `notLinked`, `busy`, `notInStep`; 502 `upstream` with a reason); `POST /api/upstream/hand-over {serverId}`: a subordinate takes the role over (`{done}`; 404 `notFound`, 409 `notMain`, `notConnected`); `POST /api/upstream/resume {app}`: resume an app the write guard stopped (answers at once with the link's status, without waiting for the main; 400 `invalid` for an invalid app id, 409 `notLinked`, 404 `notFound` for an app this server does not serve, 409 `notStopped`, 502 `upstream` with the reason a halted link halted) (see "Server modes and the link"). `GET /api/upstream` also gives `term`, `waiting` (on a main that took the role over: `[{serverId, name}]`) and `promoting`.
- **`/api/status`** also carries `listening` (the UDP port is open, so devices can connect), `program` (the program file), `serverDir`, `dataDir`, `mainServer` (the default server folder), `lastError` (why the last reset or move failed, or why a restart needed retries), `role` (`main`, or `subordinate` while linked) and `term` (the family's).
- **The server controls answer first:** each handler that stops or restarts the server writes and flushes its 202, then asks the server; the web server's shutdown waits for requests in flight (5 s). The first request wins; a later one while the server stops is ignored. The destructive actions have their own routes and take no confirmation word: a wrong flag in a body cannot turn a stop into a deletion.
- **The guard** applies to every route, the page and assets included:
  - `Host` must be `127.0.0.1:<port>` or `localhost:<port>`, against DNS rebinding;
  - an `Origin`, if present, must be the page's own;
  - every non-GET request needs the header `X-Workspace-Server: 1`, and so does `GET /api/data-dir`: it writes a probe file and tells which paths exist, and another site can send a GET with an `<img>` and no Origin. A cross-site page cannot send a custom header without a CORS preflight, and the server never grants one. **Never add CORS headers.**
- **Response headers:**
  - `X-Frame-Options: DENY`;
  - CSP `default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'`, so no inline scripts and no form posts;
  - the page itself adds `style-src 'self' 'nonce-<n>'`, with a new nonce on every request: Vuetify writes its theme into a `<style>` element, and `theme.cspNonce` gives it that nonce. Nothing else inline gets in: no `unsafe-inline`, no `eval`, scripts only from the page's own files. Styles that Vue and Vuetify set go through the CSSOM (`element.style`), which the CSP does not govern;
  - `Referrer-Policy: same-origin`;
  - `Cache-Control: no-store`.
- **Deleting an app** removes its entry, its folder and its devices, and closes their sessions. The confirmation shows what goes, offers a download, and asks the user to type the id.
- **Managing the server:** the Server section has a data-folder field with its own Change button, and the buttons Stop the server, Delete data (reset) and Delete the server and all data, each confirmed in a modal dialog with a plain OK. After a restart the page polls `/api/status` every 500 ms for up to 30 s and reloads; after a stop it shows a "stopped" panel.
- **Error messages:** the page shows a localized error and appends the server's English `message` in parentheses.
- **Every error is JSON** `{error, message}`: a path with no route is 404 `notFound`, a route asked with another method 405 `invalid` with the mux's `Allow` header, an unknown asset 404 `notFound`. The handler asks the mux which pattern matches and passes only matched requests to it, so the handlers still get their path values.

## Commands, autostart and removal

| Command | What it does |
| --- | --- |
| none (opening the file) | **Interactive.** If the server folder's server answers (`probeServer`: `identity.json`'s `serverId` matches `/api/status` on `config.json`'s port), opens its page and exits 0. Otherwise runs the server in the console and opens the page once it serves. If the lock is held by a server the probe missed, retries the probe for 3 s, then exits 1. |
| `run [-dir] [-udp-port] [-http-port] [-background]` | The server in the foreground; no probe, no browser. For scripts and autostart. `-background` releases the Windows console first. |
| `start` | Stops a running server (`POST /api/server/stop {}`, waits up to 10 s; one that does not go is stopped through the OS, `autostart.Uninstall`, as `stop` does), registers autostart for the default server folder, writes the Windows Apps & features entry, opens the page, waiting up to a minute for it: the first start of a new unsigned file can be slow while the system checks it. |
| `stop [-dir]` | Asks the server to stop, then (main server) `autostart.Uninstall`, which also stops through the OS a server that did not answer; waits until the page is gone and the lock is free. Data, program file and the Apps entry stay. |
| `delete [-dir] [--yes] [--pause]` | Shows what goes (folders, counts, the program file), asks `[y/N]` (Enter is no; without a terminal and without `--yes`, exit 2), stops like `stop`, then removes the data (when the data folder is another one), the server folder, and, for the main server, the Apps entry and the program file. `--pause` waits for Enter (the Windows uninstaller's window). A signal, or the console window closing, ends the question and the pause at once with nothing deleted. A side server whose folder does not exist has nothing to delete; for the main server it lists only the program file and autostart. |

- **The main server** is the one on the default server folder. Autostart, the program file and the Apps entry belong to it; `-dir` with another folder touches none of them.
- **A development build** (version `dev`: built without the ldflag, as every `go build` and `go run` is) never uses the installed server's folders (`devguard.go`, D11). Protected are the default server folder and the data folder its `config.json` names, and every folder inside either; paths compare with `filepath.Rel` (case-insensitive on Windows) and `os.SameFile`. A folder checked is refused also when its `WSS-Data` is protected: the parent of an installed data folder that is a `WSS-Data` chosen itself would otherwise share it.
  - `run` and interactive mode refuse in `runOwned`, before the folder is created, locked or logged in, when the server folder or the data folder its own `config.json` names is protected.
  - The page's data-folder check (`checkDataDir`) gives `unavailable` for a protected target, so a move or an adopt cannot reach it.
  - `start` refuses before anything else, so the installed server keeps running and autostart stays.
  - `delete` refuses a protected server folder or data folder. `stop` works.
  - To run on the default folder on purpose, build with a version: `go build -ldflags "-X main.version=0.0.0-local" -o tmp/x.exe ./cmd/workspace-server`.
- **The CLI always sends `{}` to the stop route** and does its removals itself after the server is gone. A server asked to unregister autostart as it exits could otherwise remove the registration `start` has just written.

### Autostart (`internal/autostart`)

- **The registered path:** the running file, after `os.Executable` and `EvalSymlinks`. The command line names the server folder (`-dir`), so the service and a shell agree on it even when the service manager's environment (`XDG_DATA_HOME`) differs from the shell's.
- **`Install`** is idempotent; a failed one removes the plist or unit it wrote. **`Uninstall`** stops through the OS and removes; it reports whether anything was there. **`Unregister`** removes the registration **without stopping the caller** (the server that deletes itself may be the one the registration started): Windows `schtasks /Delete /F` without `/End`; macOS deletes the plist, then `launchctl bootout` last; Linux `disable` without `--now`, delete the unit, `daemon-reload`.
- **`OpenPage`** waits for the port (10 s for a running server, a minute after `start`), then starts the browser without waiting for it (`xdg-open` may run one in the foreground). A page that never answered is not opened, since the browser would only show an error; the command says the server is still starting and gives the address.
- **Other systems** (FreeBSD and the rest): `Install` is not supported; `Uninstall` and `Unregister` find nothing registered, so `stop` and `delete` work.

| OS | Registration |
| --- | --- |
| macOS | LaunchAgent `io.github.flighter7.workspace-server`, `~/Library/LaunchAgents/…plist`, `run -dir <folder>`. `KeepAlive` is `{SuccessfulExit: false}`: launchd restarts the server after a crash, not after a requested stop (exit 0). `start` runs `launchctl bootout` then `bootstrap gui/<uid>`. |
| Linux | `~/.config/systemd/user/workspace-server.service` (fixed path: the user manager's search path comes from its own environment, not the shell's), `ExecStart=<binary> run -dir <folder>`, `Restart=on-failure`. `start` runs `daemon-reload`, `enable --now`, `restart`. `%` is doubled everywhere and `$` in the folder; a program path holding `$` is refused, since systemd.service(5) does not promise to undo `$$` in the executable. |
| Windows | Task `workspace-server`, `run -background -dir "<folder>"`, registered with `schtasks /Create /XML <file> /F` (see below). |

**The Windows task** is built from XML, not `/SC ONLOGON`:

- **Why XML:**
  - `/SC ONLOGON` without a user is an any-user trigger, which a standard user may not create.
  - Tasks created by `schtasks` default to "AC power only", "stop on battery" and a 72 h limit.
- **What the XML contains:**
  - a `LogonTrigger` and a `Principal` for the current user only: `InteractiveToken`, `LeastPrivilege`;
  - `Exec <binary> run -background -dir <folder>`;
  - no battery conditions;
  - `ExecutionTimeLimit PT0S`;
  - `MultipleInstancesPolicy IgnoreNew`;
  - `Settings` children in Task Scheduler's export order.
- **The file** is written in UTF-16LE with a BOM.
- **Result:** verified on Windows 10 by a standard user (with `run` as the argument, before `-background` and `-dir`).

**The Windows console window:** the binary stays one console program (PE subsystem 3).

- **`run -background`** calls `FreeConsole()` before it prints anything (`console_windows.go`). The call fails harmlessly when there is no console, as in a server `start` launched. After it the process gets no console events: Ctrl+C and the window's close button are gone, and the page's Stop button and `workspace-server stop` are what stop it. Writes to stderr fail and are ignored; the log file keeps every line.
- **`start` starts the server itself** instead of running the task (`spawn_windows.go`): `CREATE_NO_WINDOW|CREATE_NEW_PROCESS_GROUP|CREATE_BREAKAWAY_FROM_JOB` (once more without the last when a job object forbids it, which shows as "access denied"), the null device for its streams, the **system folder** as its working folder (like a task's own start; a folder this command ran in, or the server folder, would otherwise stay undeletable while the server runs), and `Process.Release()`.
- **A brief flash of the console at logon** is possible: the task starts a console program, which then lets go of the window. A person watching a real logon on 2026-11-10 saw none, and polling at about 100 ms caught none either. That is one machine, once, so the guide keeps saying it may happen.
- **`taskExists`** tells "no such task" from a failing query by listing every task (`schtasks /Query /FO CSV /NH`) after `/Query /TN` fails: the two cases share exit status 1 and a localized message.

### Stopping and removing the server

After a requested stop, once the log is closed and the lock released (on Windows an open file blocks deleting its folder):

1. **Delete** only: delete the data (the server's app folders in a data folder other than the server folder; `apps/`, then `README.txt` with `WSS-Data`, then the folder itself, each only if nothing else is left in it, since the user may have chosen a folder holding other files).
2. **Delete** only: delete the server folder. `removeServerDir` refuses a folder holding none of `identity.json`, `config.json`, `apps.json`, `server.lock` (a mistyped `-dir`), and a folder another process has locked; it deletes everything but `server.lock` while holding the lock, then the lock file and the folder.
3. **Delete**, main server: remove the Windows Apps & features entry (`HKCU\…\Uninstall\workspace-server`; a fixed key, so at most one entry).
4. **Delete**, main server: delete the program file: `program.Remove` deletes one regular file, never a folder or a symlink. Unix unlinks it at once. Windows cannot delete a running program's file, so a hidden PowerShell helper retries `Remove-Item -LiteralPath` every 0.5 s for 10 s after the process exits.
5. **Delete** or **stop with `removeAutostart`**, main server: `autostart.Unregister()`, **last**, because on some systems it ends the calling process. Just before it the process gives SIGINT, SIGTERM and SIGHUP back their default action: `launchctl bootout` ends the agent it unloads with SIGTERM, and a caught SIGTERM would leave the process waiting for launchd's kill.

Each step runs even if an earlier one failed; failures go to stderr (the log may be gone). On Windows the helper is a hidden Windows PowerShell from the system folder (`CREATE_NO_WINDOW|CREATE_NEW_PROCESS_GROUP`, working folder the system folder, nothing waited for); the entry is written and deleted with `x/sys/windows/registry`, and its key is a parameter of the inner functions so that tests use a scratch key.

## Packaging, CI, release

- **Binaries:** one per platform, everything embedded, with no runtime:
  - `wss-mac-arm64`, `-mac-amd64` (Go's `darwin`);
  - `-windows-amd64.exe`;
  - `-linux-amd64`, `-linux-arm64`.

  The guide has the user rename the file to `workspace-server`; the program does not depend on its own name.
- **`scripts/build.mjs [version]`** builds a release locally and in CI alike: the web page (`pnpm install --frozen-lockfile`, `build`), then the five binaries with `CGO_ENABLED=0 -trimpath -ldflags "-s -w -X main.version=<version>"`, and `SHA256SUMS` in `sha256sum`'s format, into a clean `dist/`. The platform list lives there only. `dist/` holds nothing else; throwaway binaries, servers' data, e2e data and check reports go to `tmp/`.
- **No code signing:** the first run meets Gatekeeper and SmartScreen, and the firewall asks once (on macOS again after each update).
- **`.github/workflows/ci.yml`:** runs on `ubuntu-latest`: `gofmt`, `go vet ./...` natively and for Windows, macOS and FreeBSD, `go test -race ./...`. The cross-vets compile the other systems' code but run none of it.
- **`.github/workflows/release.yml`:** on a `v*` tag:
  - runs `ci.yml` first, and builds nothing if it fails;
  - runs `node scripts/build.mjs <tag>` in one job, which stops if the page did not build;
  - in a job of its own with write access, checks `SHA256SUMS` and publishes `dist/*` as a release, a pre-release when the tag has a `-` (`v1.0.0-rc.1`).

  Without the ldflag the version is `dev`.
- **The client package in CI:** `ci.yml`'s `client` job runs on `ubuntu-latest`:
  - pnpm, at the version in the package's `packageManager`;
  - `pnpm run check`: lint, Prettier, typecheck, unit and conformance tests, build;
  - then the e2e, which builds the server from the same checkout.

  `release.yml` runs it with the rest of `ci.yml`, so binaries are built only after the package passes.
- **The web page in CI:** `ci.yml`'s `page` job does the same for `client-wss/`: `pnpm run check` (lint, Prettier, unit tests, typecheck, build), then the e2e against a server built with the page embedded. The Go job never builds the page: `internal/web`'s tests serve a stand-in from `fstest.MapFS`, and read `client-wss/index.html` itself to check it is the template the server fills.
- **Publishing the package: by the user, by hand.** No workflow and no agent publishes it.
  1. Set `client/package.json`'s `version` to the server's tag, `1.0.0` for `v1.0.0`, `1.0.0-rc.1` for `v1.0.0-rc.1`.
  2. From the package's folder, `cd client`, run `pnpm publish --access public`; add `--tag next` for a pre-release. `prepublishOnly` runs `check` first. Not `pnpm --dir client publish` (nor `-C client`): pnpm 10.33.0 passes `--dir`'s value on to npm 11.8.0 with `publish` again, and npm refuses the extra arguments with `EUSAGE` (checked with `--dry-run`, 2026-11-13).
  3. npm asks for 2FA.

  The first release goes out as the release candidate `1.0.0-rc.1` (`v1.0.0-rc.1`), and `1.0.0` follows once the user has checked it by hand ([TODO.md](TODO.md)).

  A manual publish has no provenance attestation; that was accepted. The package ships `dist/` and `src/` (so source maps and declaration maps resolve). `prepack` copies the root `LICENSE` in, and `postpack` removes it again.
- **Docker:** an image for a NAS is a later step. It needs host networking for UDP and mDNS.

## Decisions and why

Each entry gives the decision, the reason, and the cost if it is wrong.

### Protocol and hub

- **Apps (R1):** `apps.Add` treats a zero `quota` and an empty name as the defaults, and only then validates the ranges.
  - Why: the page's "Add it?" posts only the id.
  - Cost: an API caller sending 0 gets the default, not a 400.
- **Device writes (R2):** the `lastSeen` throttle counts only `lastSeen` writes, failed ones included (2026-11-10), so a broken disk is not retried and logged on every `hello`.
  - Cost: one extra `devices.json` write after pairing.
- **Refused puts (R3):** a refusal of a `put` (`quota`, say) follows its body, which the server discards.
  - Why: every reply to a `put` follows its body.
  - Cost: the client learns of the refusal slightly later than it could.
- **Frame order (R4):** frames are strictly ordered through one non-blocking send queue.
  - Why: the framing rule, and a blocking push would stall broadcast on a slow peer.
  - Cost: a session that does not read fills its queue; at 1 MiB of text frames or 4 bodies waiting it is closed (F2). Repeated `get`s are not coalesced, since frames carry no request id and every `get` gets its own `doc`.
- **Pairing confirmation (2026-11-10):** a pairing is confirmed by the client's next frame; a session that ends first deletes the new device.
  - Why: a device entry whose token never reached the client is a dead row on the page, and nothing else tells the server whether `paired` arrived.
  - Cost: a client must send a frame right after `welcome` (protocol.md says so), and a client that lost its session at that moment pairs again. No `proto` bump: no client implemented the protocol yet.
- **The backend interface (R5):** `protocol.Backend.Hello`/`Pair` take the `*Session`.
  - Why: the hub must register, list and close sessions.
- **Rate limit (R6):** an IP is blocked at its **10th** failure, one fewer than the original spec's "more than 10".
- **Blocked IPs (R7, changed 2026-11-09):** a blocked IP gets `blocked` on both `hello` and `pair`, with nothing checked.
  - At first it got `badToken`/`badSecret`. But the todo client treats `badToken` as "pairing gone" and asks the user to pair again, and that attempt was refused too.
  - A distinct code lets a client keep the pairing and wait. A client that does not know the code treats it as a temporary error.
  - Cost: one more error code. A stranger probing the server learns that it rate-limits.
- **Unknown frame types (R12):** an unknown `t` is `protocol` and closes the session, in every state. `notAuthenticated` covers only the known frames.
- **`forget` (R13):** closes every session of the device, the asking one included.
  - Cost: frames still queued on that session are dropped.
- **The connect deadline (R14):** it closes a connection whose **data channel** has not opened within 20 s, not merely one that is not ICE-connected.
  - Why: otherwise a client that connects but never opens a channel holds a slot forever.
- **Other session rules:**
  - a second `hello`/`pair` after authentication is `protocol`;
  - a missing `ifEtag` reads as `null`;
  - the authentication timeout closes without an error frame.

### Files and server

- **Atomic writes (R11):** one shared `fsutil.WriteFileAtomic`. It replaced five copies.
- **The lock:** a lock error other than "held by another process" logs and starts without the lock.
  - Why: refusing would break `-dir` on volumes without locks.
  - Cost: two servers could share a folder there.
- **Commits on exotic volumes:** `fsutil.Rename` falls back to `os.Rename` where POSIX renames are missing (FAT, exFAT, many network shares, Windows before 1809): there a commit over a document that a `get` is still reading fails until the read ends, and the journal applies it later.
- **Address changes:** an address change rebuilds the whole listener; the reason is under Watchers.

### Web page

- **JSON keys** are lowerCamelCase. The page is the only client.
- **The download date** is the download day, not the last-write day: the backend returns no time.
- **The guard covers every route,** with XFO and CSP. Cost: no inline scripts.
- **`Referrer-Policy: same-origin`** rather than `no-referrer`.
  - The first reason given, that `no-referrer` makes Origin `null`, was overstated: it only applies to non-cors requests such as form posts.
  - It is kept as harmless.
- **The English server message** appears in parentheses after localized errors.

### Commands, autostart and removal (agreed with the user, 2026-11-09)

- **D1, the page's "Delete data" is a reset:** identity, app list, devices and every app's data go; `config.json` and the log stay; the server restarts in-process as if new.
  - Why: the user's choice; a server that forgets everything but its settings is the clear meaning of "delete data".
  - Cost: every device must pair again. Devices are not told: a new identity makes their next `hello` fail.
- **D2, no typed word:** the page uses plain OK buttons; the CLI asks `[y/N]`, Enter being no. The API takes no confirmation field; destructive actions have their own routes.
  - Cost: one click deletes, after one dialog.
- **D3, `start` restarts a running server,** so that a replaced file runs; opening the file and `run` never restart one.
- **D4, Windows `start` starts the server itself,** detached and without a window, not with `schtasks /Run`, which would flash the console. (Verified on Windows, 2026-11-10.)
- **D5, `install`/`uninstall` are gone,** without aliases: there has been no release.
- **D6, the page's Stop also removes autostart,** like `stop`.
- **D7, two folders:** a fixed server folder (settings, identity, registries, log, lock) and a data folder (`WSS-Data` in it since 2026-11-13, D12) the page can move, stored as `dataDir`. `-data` became `-dir` (the server folder). Autostart, the program file and the Apps entry belong to the default server folder only.
  - Cost: two servers with different server folders but one `dataDir` would share `WSS-Data`; nothing prevents it, since a lock in the data folder would add a file the user did not ask for.
- **D8, the Windows Apps & features entry** is one fixed key: written by every `start`, kept by `stop`, removed by `delete`.
- **D9, the log** is cut to 512 KiB (half the limit) when a line would take it past 1 MiB; one file.
- **D10, the data folder:** a missing folder whose parent exists is created silently; both folders holding data is refused; only the target holding data adopts it and lists its apps; deleting a data folder removes only the server's data in `WSS-Data`, and `WSS-Data` and the folder only if then empty; an unreachable data folder at start stops the server with a log line in the server folder.
- **Moving data copies, then switches, then deletes** (never a rename across the switch), so a crash leaves a consistent state. Cost: the data is copied, not renamed, even on one volume.
- **The CLI's stop request is always `{}`;** the CLI removes autostart itself after the server is gone (otherwise a dying server could unregister what `start` just registered). `stop` and `delete` end by waiting until the page is gone and the lock is free, since the OS-level stop is asynchronous.
- **`checkDataDir` writes a probe file** to test that a folder is writable. Cost: a GET touches the folder briefly.
- **`removeServerDir` proceeds without the lock** where the file system has none, as the server itself does.
- **The Windows task** is built from XML for the current user. It was verified on 2026-11-09.
- **The systemd unit** is at a fixed path, and `Uninstall` deletes it even on a `systemctl` failure.
- **A program path with `$`** is refused by `start` on Linux (2026-11-10) rather than escaped: whether systemd undoes `$$` in the executable cannot be checked without a Linux machine. Cost: such a user moves the program first.
  - Cost: a user manager with a custom `XDG_CONFIG_HOME` reports "unit does not exist".

### The client package (agreed with the user, 2026-11-10)

- **C1, one client for every app:** the transport and the sync engine moved out of the todo app into `client/`, published as `@flighter/wss-client`. An app passes its app id and a `SyncAdapter`, which merges its document. The engine decides when to sync and runs the protocol. The server merges nothing.
  - Why: every next app would otherwise copy the most fragile code: the signaling-free answer, flow control, pairing confirmation, retries, the write guard.
  - Cost: a change to the engine reaches an app only through a new package version.
- **C2, no Protobuf:** considered as a single source for the contract and turned down. The contract stays prose in `protocol.md`, pinned by the vectors in `testdata/conformance/`.
  - Why: every planned client is a TypeScript web app. Protobuf would generate only about 15 message types; the drift that hurts is in what it does not describe (the SDP answer, the pairing code, chunking, the error policy), and the vectors cover those too.
  - Cost: a client in another language (Kotlin, Swift) would write its own message types, checked against the same vectors.
- **C3, the wire did not change:** `proto` stays 1, and the frames are JSON as before. Nothing had been released, so there was no old client to keep.
- **C4, a contract change is one commit:** `protocol.md`, the vectors, the server and `client/` change together, and `proto` goes up for any change an old client cannot handle.
- **C5, publishing is by hand:** the user publishes to npm; no workflow and no agent does. An app tries a change against `link:../workspace-server/client` before it is published.
  - Cost: no provenance attestation on the published package.
- **C6, the package version is the server's tag** (`1.0.0` for `v1.0.0`, `1.0.0-rc.1` for `v1.0.0-rc.1`), so a package and the server it was tested against are easy to pair up.
- **C7, a pnpm package of its own, not a workspace:** the root stays a Go module, and Go ignores the folder. `client/.gitattributes` keeps LF, which Prettier needs, whatever `core.autocrlf` says.
- **C8, licence:** CC BY-NC-ND 4.0 for the whole repository, `LICENSE` at the root. `prepack` copies it into the package.
  - Cost: Creative Commons does not recommend its licences for software, and ND forbids others to distribute changed copies. The author's own apps are not affected.
- **C9, todo's devices did not pair again:** the package kept todo's pairing record shape, the data channel label `workspace` and every error code. todo passes its own key `todo.server` and app id `todo`.

### Server modes (2026-11-12)

- **L1, a link session beside the app sessions:** the app list, and everything else that concerns the link as a whole (`announce` and the registry, `invite`, `term`, `takeOver`), travels on a session that exists whatever apps the main has, none included. It also gives the server-kind pairing a session that needs no app.
  - Cost: one more of the main's 64 sessions per subordinate.
- **L2, a server-kind device has no app** in `devices.json` (`app: ""`, `kind: "server"`), rather than a made-up app id that could collide with a real one.
  - Cost: a binary from before this change refuses such a `devices.json`, so downgrading a main that has a subordinate needs the entry removed first.
- **L3, the app list is objects:** a later field needs no new frame (the quota came so, L42), and the name labels the app on the subordinate.
- **L4, `proto` stays 1:** an old client meets nothing new it cannot handle. `role` is one more `welcome` field, which clients ignore; `kind`, a `hello` without app and `apps` come only from servers, and `apps` frames go only to server-kind sessions.
- **L5, `upstream.json` lives in the server folder,** beside `identity.json` and `devices.json`, not in the data folder: it holds a token, and a data folder can be moved, adopted or shared. A reset deletes it. The bases, which were in it, moved to each app's `.wss/link.json` in stage 2 of the files plan (F5), so they travel with the documents.
- **L6, linking is a page API route** (`POST /api/upstream`), with no command: the page's Upstream server section uses exactly this route, and a command would have to reach the running server through the same API anyway.
- **L7, the pairing is confirmed only after `upstream.json` is saved,** by asking for the app list: a token the main keeps is never one this server lost.
- **L8, a refused upstream stops the link** instead of retrying: `notMain`, `serverChanged`, `badToken`, `protocol`, and `unknownApp` on the link session. Each means a wrong or changed configuration, not a passing failure.
- **L9, a `hello` without app** from an unknown device, or with a wrong token, gets `badToken`, so that a subordinate removed on the main learns that its pairing is gone. From a device of an app it gets `unknownApp`, as it always did.
- **L10, no merging on a server** (see "The sync rule"). The devices already hold their own data and merge on every `changed`, so a server that only moves whole documents is enough. Until stage 3 of the files plan the main's set won a divergence at once; since then a device chooses (L36).
  - Cost: the loss window, a change that only lived in the set not kept and whose device vanished before it merged again, which is then lost (no history since 2026-11-13).
- **L11, every new revision wakes the link** (`Deps.Changed`), the link's own commits included: the base and both revisions decide, so a commit the link made itself finds the two in step and costs nothing, and no hook needs to tell the link's writes from the devices'. (Until stage 2 of the files plan the hook ran in `Hub.Put` only, and `Hub.Replace`, the link's write, skipped it.) It runs after the hub's locks are released and only wakes a worker, so a device's write never waits for the main.
- **L12, nothing is queued for the main:** a write made while the main is out of reach is pushed by the next `welcome`, as whatever the subordinate's set is then. Content hashes make every step idempotent: a crash between a write and saving the base is settled by the next decision, which finds the two etags equal.
- **L13, the write guard counts one app's pushes and stops that app, both ways, until the server starts again** (see "The write guard"), like the client package's engine, which stops writing until the app opens again.
  - Cost: a subordinate whose devices write one app more than 10 times a minute between them (several busy devices on the laptop, which their own guards make unlikely) stops syncing that app with the main until the user resumes it on the page (L35) or the server restarts. The page's Upstream server section lists it.

### Handing out (2026-11-12)

- **L14, `announce` is answered by the device registry,** and the registry is pushed only to link sessions that announced. One round trip on every link session, and an old subordinate, which never announces, never gets a frame it does not know.
- **L15, a main always sends `subordinates`, `[]` included:** a device drops a subordinate the main no longer lists, and a subordinate knows that its main takes `announce`. A main from before would close the link session over an unknown frame, which halts the link (`protocol`).
- **L16, the subordinate gets token hashes, never tokens,** and checks a token by its SHA-256, which a 256-bit random token needs no salt for. The replica lives in `upstream.json`, not `devices.json`: it goes with the link (a reset forgets both), is never listed on the page, and an old binary reading `devices.json` meets nothing new but `server`, which it ignores and drops at its next write.
- **L17, a removed device is admitted by a subordinate until its next link:** a trade the user accepted for a single user. A subordinate the main removed clears the replica: the main no longer vouches for those devices.
- **L18, `pair` on a subordinate gets a new code, `notMain`,** checked before the secret: nothing to consume, and no failure counted, since the device did nothing wrong. An old client reads it as `internal`. No `proto` bump (codes may be added).
- **L19, the device prefers the main by its `role`,** then the server it paired with: the paired main answering is taken at once; any other answer waits until 2 s after the start (`MAIN_GRACE_MS`), or until every attempt ended. A dead server holds an attempt until it times out (8 s in the browser), which is why nothing waits for every attempt. Only the paired server's refusals stop sync: a subordinate's `badToken` can mean a device it has not heard of yet. `serverChanged` is a `welcome` from outside the known set. `lastEtag` and `lastRevision` stay shared: both are content hashes.
- **L20, `forget` through a subordinate stays there:** relaying it would need a new frame for a rare case; the main's page removes the device.

### The page and invite (2026-11-12)

- **L21, `invite` asks the main for its ordinary code,** over the link session, rather than letting a subordinate pair devices itself: devices pair only with the main, and the code the browser gets is exactly what the main's page would show, so nothing new is trusted. The subordinate is a server-kind device already admitted to every app.
  - Cost: the code revokes the one on the main's page, as any new code does.
- **L22, `invite` names no app:** a pairing code never does; the app that pastes it names itself in `pair`. A field would only restate what the browser's app already says, and the main's `unknownApp` offer covers an app it does not serve.
- **L23, the subordinate's own `POST /api/pairing` is refused** (409 `notMain`) rather than relayed to the main: a code of the main is shown only where the page says that it is the main's, under "Pair this computer's browser".
- **L24, a main with subordinates does not link** (409 `hasSubordinates`), instead of linking and leaving its subordinates halted: re-parenting a family is a switch of the main (L28–L34).
- **L25, unlinking tells the main when it can,** over a link session of its own after the link has stopped, and goes ahead when it cannot; the page says which. The main's devices are no longer admitted: the main no longer vouches for them here.
- **L26, the Upstream server section lives in Server,** not in the header's links: it is a setting of this server, and a fourth link would crowd a phone's header for a section a main rarely uses.
- **L27, the CSP stays as it is for the camera:** qr-scanner's worker is built as a file of the page's own instead of allowing `worker-src blob:`, and its overlays, which need `style` attributes, stay off.

### Switching the main (2026-11-12)

- **L28, a term per family, compared with the server id:** the user's flow ("the subordinate becomes the main, tells the others, the old main steps down") made to work offline: the term orders mains with no clock, and the server id breaks a tie the same way on every server. A term goes up only when a subordinate becomes the main; a server that steps down or follows takes the term it is told.
  - Cost: the order is arbitrary between two servers made the main apart at the same term.
- **L29, every server reaches the others with the credential it already has:** the registry now carries server-kind devices (hashes and connection data), so every server of a family admits every other to a link session, and a promoted subordinate keeps its credential. The one server without any, a main, gets one minted by the new main when it is reached. Considered and set aside: a family-wide shared secret (one more secret, and any server could impersonate the main), the server's certificate as a credential (needs the DTLS layer to expose the peer, a deeper change), and the old main dialling the new one (it has no credential to dial with).
  - Cost: a server removed from a family on one side during a split cannot be told by the other (its credential is refused), and two mains stay mains until the user removes or links one by hand.
- **L30, the new main tells; a lower server follows by itself when it meets a higher main:** only a promoted main (the one with a credential) dials, so a family that never switched dials nobody; a subordinate whose main stepped down looks for the new main itself, so one that was away when its main stepped down still finds it.
  - Cost: a promoted main whose subordinates are often away (a laptop asleep) dials them up to every 5 minutes until each has followed it once.
- **L31, the registry merges by union with tombstones, with no clock:** device ids are random and never reused, so "removed" is final and "paired on either side" is kept; nothing depends on the servers' clocks. A main hands its own registry over in its first `announce` after it steps down, and forgets it only once the new main answers, so a device is never admitted nowhere.
  - Cost: only the newest 1,000 removals are kept; an older one could come back with a registry from a server that held the device all that time.
- **L32, a planned switch waits for the documents to be the same:** otherwise the old main, now a subordinate with no base, would meet a divergence at its first sync and have its own set replaced for nothing. A forced switch cannot wait, and the sync rule settles it the same way it settles any first link.
- **L33, the device follows the main:** a fallback answering as the main at a term at least the paired server's becomes the paired server (the record shape unchanged, one optional field added, `term`). Otherwise a device paired with a PC that is gone for good, or later unlinked, would keep it as the server whose refusals stop sync. `proto` stays 1: `term` is one more `welcome` field, and every new frame is between servers.
- **L34, no plain toggle:** the controls are "Make this server the main" on a subordinate and "Make it the main" on a main's linked server; a main never stops being one without a successor.

### Resuming a stopped app (the user's choice, 2026-11-13)

- **L35, a "Resume" button beside each app the write guard stopped,** on the subordinate's page, chosen by the user over a restart only and over resuming by itself after a pause. It clears the app's stop and its pushes in the guard's minute (a fresh 10), and syncs the app with the main at once by the sync rule, over a new session (once the main is reachable, when it is not). The guard (L13) stays as it is, and so does a restart, which still resumes every stopped app. No question before it: every write is conditional, and a divergence it meets settles as any other (see "The loss window").
  - Why not resume by itself: a pause and a retry would hide a real loop, which would then go on, stopping and resuming, and quietly replace both servers' documents for ever. A person who presses Resume has seen that the app stopped.
  - Cost: a loop that goes on stops the app again, after at most 10 more writes per press.

### Development builds (the user's choice, 2026-11-13)

- **D11, a development build keeps off the installed server's folders** (see "Commands, autostart and removal"). The user runs the installed server and develops it on the same machine. The lock keeps a second server off a folder only while the first one runs; with it stopped, a `go run` without `-dir`, a `start` or `delete` from a development build, or a move or adopt on a development server's page reached the real data.
  - Why the version and not a flag: a forgotten flag is the very mistake to catch, while a release build needs nothing; the e2e harnesses always pass `-dir` under `tmp/`, so they are unaffected.
  - Why not the alternatives discussed: a read-only data folder stops the installed server's own writes, so sync stops while it is on, and the folder's read-only attribute does not cover the files in it on Windows; a write freeze on the page does not bind another process; snapshots were left for later.
  - Cost: a check of `start`, autostart or `delete` on the real default folder needs a build with a version. Two release builds can still share a data folder (D7).

### The data's folder (the user's choice, 2026-11-13)

- **D12, `WSS-Data`:** the data sits in a `WSS-Data` folder with a `README.txt` that tells a person not to touch it, so it is easy to find and hard to mistake for something else. `dataDir` keeps the user's choice and the server adds `WSS-Data`, everywhere, the default included; a `WSS-Data` chosen itself is used as is. Cost: the verdicts depend on what the chosen folder holds (whether it is taken for a `WSS-Data`), not on its path alone.

### The files plan, stage 2 (2026-11-13)

The protocol, the hub, the link and both clients moved from one JSON document per app to the store's set of files (the user's decisions and the plan's are in the plan's working file; these are what the code settled).

- **F1, bodies are streamed, never held:** a `put` body is written to its staging file in the message handler, synchronously, with no `bufio` and no writer queue (stage 0: back pressure works, no speed lost on an SSD, about 20% on an HDD for a 1 GiB body), and a `doc` body is read from its file as the channel takes it.
  - Why: the goal was a server whose memory does not grow with a document. Measured: about 31 MiB at peak for the whole process with 256 MiB and 1 GiB bodies, against 5.1 GB for 1 GiB before (see "What has been verified, where").
  - Cost: large bodies on an HDD are slower than the channel (see TODO.md).
- **F2, the send queue holds 1 MiB of text and 4 bodies,** replacing the app's limit plus 1 MiB: a waiting body is an open file, not its bytes, so the limit counts files.
- **F3, a put's progress is recorded by time, at most every 5 s,** not every 8 MiB (the conductor's decision, 2026-11-13): each record syncs the body and writes `tx.json`, and 32 records per 256 MiB halved the put's speed (20–24 MiB/s against the channel's ~48).
  - Cost: a crash or power loss loses at most about 5 s of a transfer; a dropped session loses nothing.
- **F4, a commit error after the commit point is `internal`, and the client lists again:** the store may have applied part of it and ended the transaction (stage 1), so retrying the same transaction could not be right; `begin` with the same id tells the client whether the failure came before the commit point.
- **F5, the base of a link is per app, in `.wss/link.json`, with the main's server id:** a set can be large, so it left `upstream.json`; it travels with the app's folder; a base agreed with another main is no base, so following or becoming a main never compares with what was agreed long before. Promoting and unlinking forget every base.
- **F6, a divergence was settled by the main's set, the whole of it,** until stage 3 (L36): the plan's decision 5 already settles a choice for the whole set, so the old rule was that choice made at once for the main. The frames of the choice (`diverged`, `resolve`, `resolved`) went into the contract then, so that clients could be built against the final one.
- **F7, the client package's engine keeps its adapter for one document,** syncing `data.json` through the new frames, until stage 5 brings one for several: todo builds and syncs against it unchanged (checked through `link:` before it moves, stage 6). The pairing record keeps `lastEtag` (the etag of `data.json`) and gains an optional `lastRevision`, so records saved before still read, and a sync that finds the set at `lastRevision` reads nothing. The package's version stays `1.0.0-rc.1` until stage 5 makes it `1.0.0-rc.2`.
- **F8, the engine reads again rather than writes again:** a write that fails with `noTransaction` or `internal` goes round the conflict loop (list, read, merge, write), as a conflict does; a conflict aborts its transaction rather than leave it to the next `begin`.
- **F9, `proto` stays 1:** nothing had been released (the user's decision of 2026-11-13: no compatibility, no migration); the server, the Go client, the vectors and the client package changed in one commit (C4).

### The files plan, stage 3: divergence and the family's quota (the user's choices, 2026-11-13)

- **L36, a divergence waits for a device's choice** (the user's choice, over the main's set winning at once): the subordinate keeps its own set, the app stops syncing with the main both ways, and the devices choose to keep the main's set or the subordinate's, the whole of it (the plan's decision 5). Nothing is merged on a server.
  - Why: the main's set winning at once lost a change made only on the subordinate whenever its device vanished before it merged again; a person who sees both servers named chooses knowingly.
  - Cost: until a device chooses, the two servers hold different sets of the app, and a device that syncs with both (a phone falling back to the laptop) sees whichever it reaches. A device that never answers keeps the app paused; the page lists it.
- **L37, every device of the app is asked, on either server, and the first answer wins:** the main lists every subordinate's divergence, the subordinate its own, and a choice made on either counts.
  - Why: the user's choice; the device at hand is as good a judge as any, and asking only the subordinate's devices would leave a family whose laptop is rarely used waiting.
  - Cost: two people may answer differently; the later is told `resolved`, as answered, and sees the first one's outcome.
- **L38, the subordinate decides which choice is first:** a choice on the main is pushed to the subordinate (`resolve` on its link session), at once or when they next meet, and it counts when it gets there; one made on the subordinate meanwhile wins. The two servers never need to agree on a clock or meet to take a choice: the subordinate holds the copy that waits, and its lock orders the choices.
  - Cost: a device that chose on the main while the subordinate was away is answered `resolved` at once, and may later see the other choice's outcome.
- **L39, a choice is carried out as an ordinary transaction,** one in the chosen direction, recorded in `link.json` and resumable like any pull or push, and it counts against the write guard when it pushes. No new kind of write, and nothing a restart can leave half done that the table cannot settle.
- **L40, the link-session frames are small:** `diverged {app, since}` from the subordinate, unanswered, sent for every app at every link session and at every change, and `resolve {app, keep, since}` pushed by the main; `since` names the divergence, so a choice for an old one is never taken for a new one. The main keeps its subordinates' divergences in each app's `.wss/divergences.json`, a choice included, so a restart of the main forgets neither. `proto` stays 1: only servers send or receive these.
  - Cost: one frame per app at every link session.
- **L41, a planned switch is refused while a divergence waits anywhere in the family** (409 `diverged`: the subordinate's own, another subordinate's as the main's sessions last told it, and on the main's hand-over every one it keeps, a choice not yet taken included; the conductor's decision, 2026-11-14); a forced switch and a first link go through the same divergence as any other: a forced switch forgets the divergence from the main it leaves, and the old main, stepping down, meets whatever differs at its first link to the new one.
- **L42, the quota is the main's:** `apps` carries each app's quota, and a subordinate writes it into its own `apps.json`, so both servers refuse the same sets and a pull never fails on a smaller quota here. Editing the quota on a subordinate's page is stage 4's (409 `followsMain`); a quota out of the subordinate's range is left as it was, and logged.

### Process

- **Development (R10):** the Go rewrite was developed directly on `main`, one reviewed task at a time. Every commit carries the orchestrator's attribution line.

## Limits

- IPv4 and the LAN only. No accounts, no merging, no history of documents.
- No code signing.
- On Windows, a logon-started server may flash a console window for an instant before it lets go of it (see "Commands, autostart and removal"; none was seen at a real logon on 2026-11-10). A process without a console gets no console events, so at logoff Windows ends it hard. That is safe: writes are atomic, and the OS releases the lock.
- A server started with `-http-port` that is not in `config.json` is not found by `probeServer`; opening the file then runs into the lock and exits 1, and `stop` falls back to the OS-level stop.
- Two servers whose `dataDir` is the same folder share `WSS-Data` unprotected.
- Deleting the program file by hand on Windows leaves its Apps & features entry; Windows does not clean it up.
- The mDNS responder:
  - guesses the arrival interface on Windows, by subnet;
  - suppresses only copies of a query on one interface within 250 ms, not the duplicate answers of RFC 6762 §7.4 that other responders sent;
  - has not been tried on a Windows machine with several adapters.
- `.local` resolution costs about 1 s on Windows.
- A subordinate holds N + 1 of the main's 64 sessions for N apps.
- When the main removes a subordinate, each of the subordinate's sessions fails once on reconnecting. With 10 apps or more that is the 10th failure within a minute, and the main blocks the subordinate's address for 10 minutes, the browser on that machine included.
- A device removed on the main is still admitted by a subordinate whose link is down, until its next link; `forget` sent to a subordinate does not reach the main.
- A code a subordinate's page gets for a browser (`invite`) revokes the code the main's page shows (which that page keeps showing until it expires), and needs the main to be reachable.
- A subordinate unlinked while its main is out of reach stays listed on the main until it is removed on the main's page.
- An app the write guard stopped syncs with the main again only when the user resumes it on the page, or when the server restarts; nothing resumes it by itself (L35). On a halted link it resumes only with the restart.
- The device registry travels in one text frame, which must fit one data-channel message (256 KiB): about 700 devices with the full list of 1,000 removals.
- A divergence between a subordinate and its main waits for a device's choice; until one comes, the app does not sync between the two servers, and a device that reaches both may see either set. The choice replaces the other set, whole: a change that only lived there, whose writing device vanished before it merged again, is lost (see "The sync rule"). A divergence does not end by itself when both servers come to hold the same set while it waits: a device still chooses, and nothing is then written.
- The write guard stops an app on a subordinate after more than 10 pushes to the main within a minute, until it is resumed on the page or the server starts again.
- Switching the main: a planned switch needs the documents in step within 10 s, and a main whose devices keep writing may not get there (a forced switch is then the way). A server removed on one side during a split is not told by the other; two mains then stay mains until one is unlinked or linked by hand. A promoted main dials the servers it waits for up to every 5 minutes until each follows it once; one gone for good is removed under Devices. A device paired with a main and not opened before that main left the family (unlinked, or reset) does not learn the new main, and pairs again.
- Away from home, a browser on the laptop with no network at all, and a phone on a hotspot or on Wi-Fi that isolates clients, may not reach the laptop's server (see "Away from home: the network"); neither was checked yet.
- Small known gaps and their fixes are listed in [TODO.md](TODO.md).

## What has been verified, where

| What | Where | Result |
| --- | --- | --- |
| The spike: iPhone Safari (tab and Home Screen), iPhone Chrome, Mac Chrome | 2026-11-07 | pass |
| mDNS responder: `dns-sd -G v4` gives A with TTL 120; `-G v6` gives "No Such Record"; the Mac's own `.local` still resolves after Close | macOS, 2026-11-08 | pass |
| Server smoke test: page, pairing code, `todo` app, `dns-sd`, SIGINT exit 0 | macOS, 2026-11-08 | pass |
| mDNS responder: coexistence with Dnscache, A answers (multicast and legacy unicast), `ping -4` 30–54 ms, the Mac resolves the Windows name | Windows 10 22H2, `c925c5e` | 7 of 8 pass. AAAA takes about 1.04 s: the Windows client's floor, see above. |
| Vet and tests; server; lock (second run exits 1 in 24 ms, log untouched); Ctrl+C and window close are graceful; restart keeps `serverId`; `install` from XML without admin; logon start; `uninstall` | Windows 10 22H2, standard user, `a99ba9e` | 7 of 7 pass |
| `start`, `stop`, `delete`, the page's server controls, the data folder: interactive mode and its already-running detection (also a second double click in Finder), SIGHUP and closing the Terminal window, data-folder create/move/back/conflict/adopt, reset, `delete -dir` of a side server, the log cut from 3 MiB to 512 KiB; then with the LaunchAgent on the real default folder: `start` (plist `run -dir`, `KeepAlive` dict), the page's Stop (exit in 0.4 s, launchd does not restart it in 30 s, plist gone), `stop`, `delete` (Enter keeps all, `y` removes process, agent, folder and the program file), the page's Delete the server, `delete -dir` leaving the main server alone; the page in a browser (dialogs, themes, narrow window) | macOS (Darwin 25.6, arm64), 2026-11-09/10, builds 7ffbe5e and 63416f0 | pass. Once, the very first `start` of a new binary did not answer within 10 s (not reproduced; likely the system's first check of an unsigned file). |
| `go test ./...` (no `-race`: no cgo); a console build (PE subsystem 3); a launch from a `.lnk` through Explorer, the second launch (`already running; opened …`, exit 0) and closing the window (graceful, same `serverId`); `start` (task XML `run -background -dir "…"`, a detached server with no window and no `conhost.exe` that outlives its parent shell, the Apps entry's values, a second `start` restarts with the same `serverId`); the page's Stop for a server the task started, with autostart removed (gone in 0.14 s; data, entry and file kept); `stop` twice and after a hard kill; `delete --yes --pause` from a copy in `a b'c $x` (all removed in 0.2 s, the file gone 0.7 s after the exit, a neighbour kept); a side server's `stop -dir`/`delete -dir` leaving the file, task and entry alone; `DELETE /api/server` for the main and for a side server with its own data folder; the data folder (`create`, `move`, back, `adopt`, `conflict`, every `unavailable`) and the reset; `delete`'s `[y/N]` on a real console; the log cut from 3 MiB to 512 KiB; `schtasks /Delete` leaves a running instance running | Windows 10 22H2 (19045), standard user, build 34c4ff0, 2026-11-10 | pass. |
| With a person at the screen: a real `start` of a fresh copy, the entry listed in Settings → Apps, the Windows Firewall dialog for the new path (Allow, Private); signing out and in (the logon task started the server, no console window flashed); Uninstall from Settings → Apps (`delete --pause` asks `[y/N]`, `y` removes the task, the entry, the process, the program file and the data folder) | Windows 10 22H2 (19045), standard user, build of f92336f (code as 34c4ff0), 2026-11-10 | pass, as the user reported; the machine was checked clean afterwards. Not written down: Windows' own confirmation text, whether the window waited for Enter, and whether a device could connect after Allow. |
| Anything on a real machine | Linux | not tested, and not planned: there is no Linux machine to check it on. The code is built, vetted and unit-tested in CI only. |
| The client package's e2e (two Chromium devices pair, merge and get a `changed` push; `badSecret`; `unknownApp`) and its unit and conformance tests; the Go conformance tests | Windows 10 22H2 (19045), 2026-11-10 | pass |
| Mac Safari, Android | | never tried |
| Two servers linked in one process over real WebRTC on `127.0.0.1` (Go tests: pairing as a server, the app list, the pull, reconnects; then push, divergence, a conflict during a push, the write guard, no ping-pong); the mDNS resolver against the server's own responder on this machine's LAN | Windows 10 22H2 (19045), 2026-11-12 | pass. A subordinate and a main on two machines: not tried. |
| Handing out: `announce`, the subordinates in `welcome`, the registry replicated and removals, the replica admitted with the main stopped and after the subordinate's restart, `pair` refused (Go, real WebRTC on `127.0.0.1`); Chromium devices paired with a main falling back to its subordinate when the main stops, and both servers converging after both moved (the client package's e2e and the todo app's, two server processes on this machine) | Windows 10 22H2 (19045), 2026-11-12 | pass. Two machines, phones, Safari: not tried. |
| Switching the main: a planned switch (documents in step first, the old main stepping down and linking, another subordinate relinking, a device paired with the old main admitted everywhere), `takeOver`, a forced switch with the old main away then meeting (it steps down, its own write kept in history), two subordinates made the main apart at the same term (the higher id wins), the registry union with tombstones across a split, the term across restarts (Go, real WebRTC on `127.0.0.1`); a Chromium device paired with the first main following a planned switch and then a forced one with that main gone (the client package's e2e, three server processes); the page's controls and the forced confirmation (Playwright, two server processes); screenshots light and dark at 390 and 1280 px | Windows 10 22H2 (19045), 2026-11-12 | pass. Two machines, phones, Safari: not tried. |
| The page's part: `invite` (a code that pairs a device with the main, refused to any other session), unlinking with the main reachable and away (Go, real WebRTC on `127.0.0.1`, and two whole servers through the page's API); the page in Chromium against two server processes: linking by a pasted code and by a QR code read through Chromium's fake camera (qr-scanner's worker loaded from `/assets/`, no CSP violation), the main's Devices marking the server, "Pair this computer's browser", unlinking, the main's removal halting the link; screenshots light and dark at 390 and 1280 px | Windows 10 22H2 (19045), 2026-11-12 | pass. A real camera, Safari, two machines: not tried. |
| Resuming an app the write guard stopped (L35): at once with a fresh count, while the main is away, the refusals, another app untouched, a restart still resuming (Go, real WebRTC on `127.0.0.1`); the page's "Resume todo" on two server processes, a Go device tripping the real guard, the app synced at once and stopped again by a second loop (Playwright, also `--repeat-each=3`); screenshots light and dark at 390 and 1280 px, en, ru and pl | Windows 10 22H2 (19045), 2026-11-13 | pass. Two machines: not tried. |
| Replacing a file another handle is reading (the files plan, stage 0): `os.Open` shares no delete, so a rename over it or its removal fails (5 or 32) while it is open; a reader opened with `FILE_SHARE_DELETE` (`fsutil.OpenShared`) and a POSIX rename (`fsutil.Rename`, `FileRenameInfoEx`) let a rename replace it, the reader finishing the old content and a fresh open seeing the new (8 readers × 200 commits, 4 runs); `os.Remove` under such a reader frees the name at once. Kept as `internal/fsutil` tests | Windows 10 22H2 (19045), NTFS, 2026-11-13 | pass. FAT, exFAT, SMB shares and Windows before 1809, where `fsutil.Rename` falls back to `os.Rename`: not tried. |
| The files plan's stage 3: a divergence on the same key and at a first link, the app paused alone, the choice on either server (forwarded, waiting for the subordinate, the first winning, two at once), restarts of either server while waiting and while the choice is carried out, the main's quota taken, a planned switch refused (Go, real WebRTC on `127.0.0.1`, and two whole servers through the page's API) | Windows 10 22H2 (19045), 2026-11-14 | pass. Two machines, phones, a device's own dialog (stages 5 and 6): not tried. |
| The files plan's stage 2, memory and speed (`tmp/reports/files-stage2/memory.md`): a `put` and a `get` of 256 MiB and of 1 GiB through a real server over Pion on `127.0.0.1`, the repository on an HDD (D:) and the server's data on an SSD (C:) in turn | Windows 10 22H2 (19045), Ryzen 5 3600, Go 1.27.3, 2026-11-13 | pass. The server's peak working set ≈ 31 MiB for 256 MiB to 1 GiB bodies (5.1 GB for 1 GiB before stage 2); `get` 47–49 MiB/s (the channel's rate); `put` 40–48 MiB/s on the SSD and 26–28 MiB/s on the HDD with the progress recorded every 5 s (F3; 20–24 with a record every 8 MiB). |
| The contract of stage 2 end to end: the client package's e2e against servers built from this checkout (two devices merging through `data.json`; a main and a subordinate; a planned and a forced switch; a 3 MiB document written from a `Blob`, listed, read in pieces and from an offset, `stale`, a conflict and a delete, through `ServerClient`); the page's e2e; the Go link tests on real WebRTC (pulls, pushes, deletes both ways, first links, bases of another main, big documents, transactions resumed after a drop) | Windows 10 22H2 (19045), 2026-11-13 | pass. The todo app on this server: stage 6. |
| The local hub anywhere but on one machine: a subordinate and its main on two machines (the laptop linked to the PC by a pasted code and by a real camera; a planned switch from either page; a forced one with the PC off, and the PC stepping down by itself when it is back; removing a PC gone for good); the Mac as the laptop (no network at all, a foreign network, sleep and wake, the PC restarting); "Pair this computer's browser" and the page in Safari; iPhone and Android syncing through the laptop's server, on a hotspot too, and following a switch; the camera's `BarcodeDetector` path (Chrome on macOS); the todo app's screen reader run of its new Sync page lines | | **not tried yet.** Every hub result above comes from one Windows machine, with every server on `127.0.0.1`. The checks are written up for the user, and listed in [TODO.md](TODO.md). |

## Testing

### Commands

Run all of these before committing:

```
gofmt -l .                       # must print nothing
go vet ./...
GOOS=windows go vet ./...
GOOS=linux go vet ./...
GOOS=darwin go vet ./...
GOOS=freebsd go build ./...      # the "not supported" autostart path
GOOS=freebsd go vet ./...        # and its test, which no other vet compiles
go test -race ./...
```

After a change to `client/`, the contract or the vectors, also run:

```
pnpm --dir client install        # once
pnpm --dir client run check      # lint, Prettier, typecheck, tests, build
pnpm --dir client run test:e2e   # builds the server with Go, two Chromium devices sync through it
```

### What the tests cover

- The pairing code, secrets and refusals.
- The identity, config, apps and devices files.
- The store: keys at its boundary; reconciling with hand edits (changed, added, removed, invalid names and `.wss` left alone, unchanged files not hashed again, symbolic links skipped where the system has them); the revision (its canonical form, the same on two stores, `null` when empty); tombstones (the cap of 1,000, dropped on re-creation); `Open` and `ErrStale`; transactions (resuming at an offset, an `ifEtag` change alone, one per device, another device's refused, abort, the 24-hour sweep, a restart in the middle of a put and after a complete one); a conflict that leaves the transaction open; the quota at `put` and at `commit`; a failure at every step of a commit and the recovery, by a restart and by the next operation, and a journal applied three times; empty folders removed; a commit replacing and deleting documents under a reader; on Windows a commit that waits for a brief plain handle, and one blocked until the next operation; side files; and what the review of 2026-11-13 found: a link or junction in the app's folder neither followed nor removed, a folder whose name differs in case refused, obstacles on a path (a folder at a document, a file at a folder) and a name differing only in case refused before the commit point, read-only documents replaced and deleted, a change that cannot be applied dropped (at once, or after a minute when busy) with the app going on, sizes near `math.MaxInt64`, an over-quota set shrinking but not growing, a body synced before each save of its progress, ending a transaction without waiting for a sync or failing on a closing body, a resume from a running body synced first, a refused put leaving a running body alone, transaction ids differing in case, a racy mtime, a replay hashing a body already moved, the journal flushed, a document turned into a folder, and stray temporary files swept.
- The protocol session over a fake channel that keeps frames sent after a close apart, so "error frame, then close" is pinned; the queue's limits (text and bodies); pairing confirmation; the sender goroutine exiting after every kind of close; the documents' frames: list pages and their cursors, `stale` across pages, `get` from an offset and at the end (no body), a body that cannot be read closing the session, a `put` streamed into the sink, resumed, refused with its body discarded, a sink that fails, a sink closed when the session ends under it, `begin`, `delete`, `commit`, `abort`, `resolve` and their refusals, the document frames before `hello` and on a link session, and the `changed`, `diverged` and `resolved` pushes.
- The hub: authentication, broadcast, rate limit, removal, and its failure branches (a lost secret race, unwritable `devices.json`, a failing welcome, internal errors), with a hook inside the welcome to pin the orders a race can take.
- mDNS packet handling, as a pure function, plus tests on the real port 5353.
- The web guard and handlers.
- The autostart file builders (the task XML's element order included), `runCommand`'s error wrapping through a helper process, and the order of the `Unregister`/`Uninstall` steps.
- The program entry's values and the PowerShell helper's quoting; deleting the program file on Unix.
- On Windows only: the detached start (no console window, the system folder as working folder), the entry written to and deleted from a scratch registry key, and the helper deleting a file whose name holds spaces, quotes, `$` and a backtick, and one that is open for a while. The task list parser is pure and runs everywhere.
- `runServer`: start, restart, the lock, the address change; stop, reset, data move and adopt, deletion and its order, main and side servers.
- The data folders: every verdict, the copy, reset and the safe deletions; `WSS-Data` (where it is, a `WSS-Data` chosen itself, `README.txt` written, restored and never written over, and removed only with nothing else left; the name in another case, the systems' own files, a linked `WSS-Data` kept, overlapping folders refused, a failed move adding nothing).
- The log cap, under concurrent writers.
- `internal/fsutil`'s shared reads and renames: a rename and an atomic write over a file a reader holds through `OpenShared`, the reader getting the old content and a fresh read the new; a removal under such a reader and a new file at the name at once; a plain `os.Open` still blocking the rename (Windows only); paths past `MAX_PATH`; which errors fall back to `os.Rename`.
- Document keys (`internal/store`): the charset, case, dots, Windows device names as a folder or an id, empty and edge segments, the limits on depth, id, type and the whole key, and parsing a path back into a key.
- The commands with fakes: interactive, `run`, `start`, `stop`, `delete`.
- A development build (D11): the protected folders (a folder inside one, the moved data folder, a path differing only in case on Windows), `runServer` refusing the default folder with nothing created in it and a side folder naming the installed data, `checkDataDir` refusing both as a target, `start` refusing with nothing called, `delete` refusing the installed server directly and through a side folder, a side server deleted and `stop` working.
- The control client against a fake server.
- An integration test: the real UDP listener on loopback with a Pion client using the client-built answer. It pairs, writes and reads, and receives `changed` on a second client. It also checks that a wrong password is ignored.
- The listener's admission (password, session cap, recently ended ufrags) by feeding binding requests straight to its packet handler, and the teardown's drain with a stand-in peer connection.
- The contract vectors (`testdata/conformance/`), played against the `Session`, `EncodeCode`, the reference answer builder and the Go client, and by the client package against its client.
- Server-kind devices in the hub: pairing, admission to every app and to link sessions, the app list and its pushes, the link's transactions (`LinkTx`), removal across apps; `Deps.Changed` after every new revision (a device's commit, the link's, a reconcile), outside the hub's locks; `announce` and its refusals, the subordinates in a main's `welcome` (none in a subordinate's), the registry pushed after every change and only to link sessions that announced, `pair` refused on a subordinate with nothing used or counted, the replica admitted by hash to its own app and its dropped devices' sessions closed.
- The mDNS resolver: query and answer as pure functions, and a lookup of the server's own responder on the real port 5353 (skipped where it cannot run).
- The link, on real servers in one process linked over real WebRTC on `127.0.0.1`: the rule as pure functions (`decide`, `compare`); every row of the sync table (the same documents on both sides before a first link; only U moved; only L moved, pushed at once after each device's write; a first link where only L has documents; both moved while the link was down; a first link with different documents on both sides; the devices' merge written back and pushed; both sides changing different documents; deletes both ways), first links, the base kept across a restart and a base of another main ignored, big documents streamed, a pull and a push cut off and resumed, `inStep`, a conflict during a push (U's document changed behind its hub, so L pushes against a stale etag, and the two diverged), writes made on L while U was down pushed once it is back (the last only), the write guard stopping one app while another goes on, the guard's sliding minute with a fake clock, resuming an app the guard stopped (the held-back document pushed at once over a new session, a fresh count of 10 after which the guard stops it again, another app syncing all along with its count untouched, a restart still resuming), resuming while U is away (synced once it is back), the refusals to resume (not linked, not stopped, not served here, a halted link, which changes nothing), and no write ping-pong (devices on both sides, the documents and the link's log stay still for 1.5 s after convergence); the app list (added and removed on the main), a subordinate refused as an upstream (`notMain`), a subordinate removed on the main (and its replica cleared), reconnects after the main's listener restarts, after a restart from `upstream.json` and after a network change; handing out: the main's `welcome` listing what the subordinate announced, re-announcing after a change, the registry replicated and a removal refused at once while linked, the replica admitted with the main stopped and after the subordinate's restart, a removal while away admitted until the next link; every device "on the subordinate" pairs with the main and is admitted on its word. Divergence (the files plan's stage 3): the same key changed on both sides and a first link with two different sets, each recorded in `diverged.json`, told to the devices of both servers and in their `welcome`s, with nothing moving for that app while another app syncs on; a choice on the subordinate keeping the main's set (documents only the subordinate held deleted, the next write on the main pulled again) and keeping its own (documents only the main held deleted, the push counted by the write guard); a choice on the main reaching the subordinate at once, and one made while the subordinate was away, across a restart of the main, carried out when it is back; the first choice winning over a later one on the other server, and two choices at once leaving both servers with one of the two sets; a restart of either server while the app waits, and of the subordinate while the choice is carried out (the pull resumed); the main's quota taken and followed; a planned switch refused while diverged, a forced one forgetting the divergence; in the hub, the frames on both roles (a subordinate's divergence told, listed, chosen for, forwarded at once and on its next link session, dropped when it says none waits or a new one begins, refused from an app session or a family server, ignored from a server that never announced, forgotten when the server is removed; a subordinate's own divergence listed and handed to its link, a later choice answered alone); the vectors of the link-session frames. In `cmd/workspace-server`, two whole servers linked through `POST /api/upstream`: the subordinate announced (as its status says) in the main's `welcome`, `pair` refused on it, a device paired with the main admitted there and its write pushed to the main, then reset; and two whose sets differ at their first link, the main's quota taken, the divergence on the page's status, a planned switch refused with 409 `diverged`, and a device of the main keeping the subordinate's set.
- The page's part of the link: `invite` in the hub (a code for a link session only; `protocol` and closed for a device's or an app session; `notAuthenticated` before hello; `notMain` on a subordinate with the session kept; `internal` on a failure) and its vectors in `frames.json`; on real servers, a code got with `invite` pairing a device with the main, which then lists the subordinate and is admitted there, and `unreachable` with the main away; unlinking (the main told and forgetting the server, `upstream.json` gone, the replica cleared and its sessions closed, the data kept, no more sync, linking again) and unlinking with the main away (not told; a server the main had removed counts as forgotten). In `cmd/workspace-server`, two whole servers through the page's API: the subordinate's own code refused, the main's `/api/devices` marking the server, the main refusing to link while it has one, `invite`, unlinking. The page: Vitest for the Upstream section's states and actions (a stand-in camera included; Resume named for its app, at once and while the main is away, a refusal shown with the list reloaded, no button on a halted link) and for Devices on a main and on a subordinate; Playwright on two server processes (see "What has been verified, where"), Resume included, with the e2e device tripping the real guard. In `internal/web`, `POST /api/upstream/resume`'s answer and refusals.
- Switching the main: the registry's merge (union, tombstones, its own entry kept, itself and a second credential for one server left out), the cap of removals, minting and re-keying a credential, `Clear`, a family server admitted to link sessions only; in the hub, the term in `welcome`, `term` handed to `Follow` and refused elsewhere, a family session allowed nothing but `term`, a registry handed over in `announce` (and refused on a subordinate), `TakeOver` and `Linked`, `MintServer`; on real servers in one process over real WebRTC: a planned switch, `takeOver`, a forced switch then the two mains meeting, the same term settled by the server id, the registry union with tombstones across a split, the term surviving restarts, a damaged `family.json` refused. The page: Vitest for the planned and forced questions, `notInStep`, looking for the new main, the servers waited for, and handing the role over; Playwright on two server processes for the same through the API.
- The client package:
  - its unit tests: framing (offsets, bodies streamed to a receiver, a `Blob` sent a slice at a time), the pairing code, the protocol client (every frame, `listAll` starting over when the set moves, `onPiece` and its failure), the pairing store (`lastRevision`), the answer;
  - the engine against a small union-merge adapter and a `FakeServer` that holds a set: nothing read at the same revision, only other documents moving, a document changing between the listing and the read, a commit failing before and after its commit point, `quota`, conflicts;
  - the page lifecycle with stand-in event targets;
  - fallback servers: learning them from the main, preferring the main within the grace period, falling back, a subordinate's refusals not halting, `serverChanged` outside the set, records with and without the field; following the main after a planned switch and after a forced one, not following a lower term, the higher term of two mains, the term kept and read;
  - its e2e: the built package in Chromium, against a server built from this checkout. Two devices pair, merge and receive each other's edit through a `changed` push; a used code gets `badSecret`; another app id gets `unknownApp`; `ServerClient` writes a 3 MiB document from a `Blob`, lists it, reads it in pieces and from an offset, meets `stale` and a conflict, and deletes it. Against a main and a linked subordinate (two processes): devices paired with the main learn the subordinate, sync through it while the main is stopped, and every copy converges after both servers moved while apart, once a device (through `ServerClient`, since the engine does not ask yet) keeps the main's copy where they diverged. Against three (a desk, a laptop and a tablet): a device paired with the desk follows the laptop after a planned switch, and the tablet after a forced one with the laptop gone, without pairing again, its edits reaching every server left.

### Traps when checking on Windows

All of these were measured on 2026-11-09.

- **Ctrl+C:** a console process started with `Start-Process` from an agent's PowerShell inherits `CONSOLE_IGNORE_CTRL_C` (`ConsoleFlags = 0x1`), so keyboard Ctrl+C does nothing. Instead, start the server from a shortcut opened by Explorer:
  1. create the `.lnk` with `WScript.Shell` `CreateShortcut`;
  2. run `explorer.exe <file>.lnk`.
- **Name resolution:** `Resolve-DnsName name.local` goes through LLMNR/NetBIOS, not mDNS. Use getaddrinfo (`ping -4 name.local`) and a trailing-dot name.
- **The race detector:** `-race` needs cgo on Windows. Run tests without it there; Linux CI runs `-race`.
- **Go:** Go 1.27.3 is installed and on `PATH` on the user's Windows machine (checked 2026-11-10; earlier it was not).
- **Line endings:** `core.autocrlf` is on and the root has no `.gitattributes`, so Go files check out with CRLF and `gofmt -l .` lists them. Check what is committed instead: `git show ":<file>" | gofmt -l`. `client/` keeps LF through its own `.gitattributes`, which Prettier needs.
- **The firewall:** a new binary path triggers the Windows Defender Firewall dialog once. Allow **Private**.
- **File sizes:** `Get-ChildItem` may show 0 bytes for a file another process holds open. Compare contents, or use `Get-Item`.
- **Task settings:**
  - `schtasks /Query /XML` prints the task without defaults, such as `RunLevel`. Use `Get-ScheduledTask` for the principal and settings.
  - `Last Result: 267009` (0x41301) means "task is running".
- **Locked files:**
  - a running `.exe` cannot delete itself (it can be renamed);
  - a file another handle holds open cannot be deleted or renamed over.
- **Hard stops:** a process without a console or window refuses `taskkill` without `/F`.
- **`schtasks /Delete` on a running instance** does not end it: the process kept running and answering 5 s later. Windows does not need `Unregister` to run last; it does so for the systems where unregistering ends the caller.
- **`schtasks /Query /TN`** exits 1 both for a missing task and for a failing query, with a localized message; list every task to tell them apart.
- **Reading a live log:** `File.ReadAllText` fails while the server holds `server.log` open; open it with `FileShare.ReadWrite`.
- **Tests that assume Unix** (`chmod 0` to make a file unreadable, deleting a folder under an open file, `/data` as an absolute path) fail on Windows. Hold the file open without sharing, close it first, use `os.TempDir()`.
- **`gofmt -l`** lists every file in a checkout with `core.autocrlf=true` (CRLF on disk, LF in the index). Compare each file's LF form with `gofmt`'s output instead.
- **A console window closed under a waiting `delete`:** before 2026-11-10 the process did not stop by itself (alive 2 s after the close, gone at the next look, tens of seconds later), so Windows ended it; nothing was deleted. The close event (SIGTERM in Go) now ends the read at once: on 2026-11-10 a `delete -dir` waiting at its question in its own console window exited 8 ms after the window got `WM_CLOSE`, with exit 1 and nothing deleted.
- **A `NUL` stdin:** Go reports the null device as a character device, so a mode check alone takes `< NUL` for a terminal. `stdinIsTerminal` asks for the console mode instead (`GetConsoleMode`); `delete` with `< NUL` and without `--yes` exits 2 (checked on Windows 10, 2026-11-10).
- **Ports given only as `-http-port`/`-udp-port`** are not found by `probeServer`; write them into the throwaway folder's `config.json`.
- **The console belongs to the binary:** it is a console-subsystem program (PE subsystem 3) with its own `conhost.exe`. Task Scheduler runs the logon task as `Interactive only`, so that window shows on the desktop.
- **A GUI-subsystem build** (`-H=windowsgui`, considered and set aside): no window and no `conhost.exe`, works fully, but `taskkill` without `/F` refuses it and only `Stop-Process` ends it (no `stopping` line; the OS releases the lock and the port). `version` and flag errors would print nothing in a terminal. It is the fallback if the background mode's console flash at logon turns out to be objectionable.
- **`<Hidden>true</Hidden>`** in the task XML hides the task in Task Scheduler's UI, not the program's window.
