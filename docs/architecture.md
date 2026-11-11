# Architecture

How `workspace-server` works, why it was built this way, what limits it has, and what has been verified where.

- **Audience:** developers and agents.
- **Related documents:**
  - the user guide: [guide.md](guide.md);
  - the wire contract with clients: [protocol.md](protocol.md);
  - open work: [TODO.md](TODO.md).
- **History:** this file condenses the original design (2026-11-07), the Go rewrite plan, its reviews, the Windows checks and the start/stop/delete plan (2026-11-09). The full texts are in git: the spec and plan up to `ad18bef`; the Windows reports in `c925c5e` (mDNS) and `a99ba9e` (server, lock, autostart); the start/stop/delete plan and its check lists in `af855e0`.

## What it is and what it is not

- **What it does:** it keeps **one JSON document per app** in a folder on disk, and it is the meeting point through which that app's devices sync.
- **What it knows:** nothing about any app's data. It stores bytes, checks only that they are JSON, and tells versions apart by their SHA-256.
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
  - several documents per app;
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
| `internal/devices` | `devices.json`: paired devices and their token hashes. |
| `internal/store` | Documents, etags and history. |
| `internal/fsutil` | `WriteFileAtomic`, shared by every file writer. |
| `internal/pairing` | The pairing code, secrets and refused pairings, in memory. |
| `internal/protocol` | One session over a data channel: framing, the message state machine, the ordered send queue. |
| `internal/hub` | The protocol's backend: authentication, rate limits, broadcast of `changed`, closing sessions on removal. |
| `internal/rtc` | The UDP listener and the signaling-free accept path. It creates a peer connection per unknown ufrag. |
| `internal/mdns` | The minimal mDNS responder for the `<uuid>.local` name. |
| `internal/netaddr` | LAN IPv4 addresses and interfaces. |
| `internal/web` | The localhost page: serves the page built from `client-wss/` (embedded from `ui/dist`), the API, the guard. |
| `internal/autostart` | LaunchAgent, `systemd --user` unit, Windows logon task; opening the page. |
| `internal/program` | The program file itself: the Windows Apps & features entry, and deleting the file. |
| `internal/conformance` | Reads the contract vectors in `testdata/conformance/`, for tests. |

### The web page (`client-wss/`)

The page `internal/web` serves: Vue 3, Vuetify 4 and vue-i18n, built with Vite. It is a private pnpm package of its own, beside `client/` and not a workspace with it, and it does not depend on the client package: it speaks only the page's JSON API. Its conventions follow the todo app: tabs, single quotes, width 100; SFC blocks in the order style, template, script; `<style lang="scss" module>`, `<script setup lang="ts">`, the `@/` alias.

- **The build goes to `internal/web/ui/dist/`, which git never holds.** `pnpm --dir client-wss build` writes it; `go build` embeds it. A binary built without it still compiles (`ui/.gitkeep` keeps `//go:embed all:ui` valid) and answers `/` with 503 and how to build the page; the API works. The release builds the page once and refuses to build a binary without it.
- **The strings** are in `src/i18n/locales/{en,ru,pl}.json`, precompiled by `@intlify/unplugin-vue-i18n` (the runtime compiler needs `eval`, which the CSP forbids). The type checker holds every `t()` key to the English catalog; the unit tests hold the other languages to the same keys and placeholders, and fail on a key nothing names.
- **Language and theme are the viewer's:** the language comes from the choice saved in this browser, else `navigator.languages`, else English; the theme is system, light or dark, `system` following the OS live. Two buttons at the right of the header open their pickers, as in the todo app. The choice lives in `localStorage['workspace-server.preferences']`, every access guarded. The origin includes the port, so a new HTTP port, or `localhost` instead of `127.0.0.1`, starts from the defaults again; accepted for a page on loopback.
- **The sections are links, not tabs:** a `<nav>` of `VBtn` links to `#server`, `#apps`, `#devices`, `#pair`, the current one with `aria-current="page"`. `VTabs` would give each link `role="tab"`, `aria-selected` and `tabindex="-1"`, which are wrong for navigation.
- **Nothing is parsed as HTML:** names, device names and refused ids are text in `<bdi>`, and `vue/no-v-html` is an error.
- **Dialogs** focus their safe button (Cancel) or their input on open and give the focus back on close; when the element that had it is gone with its row (a deleted app, a removed device), the section's `h1` gets it, never the body.
- **Development:** `pnpm --dir client-wss dev` proxies `/api` to `WSS_TARGET` (default `http://127.0.0.1:47811`, a throwaway server). The guard wants its own `Host` and `Origin`, so the proxy rewrites both; the server still sends no CORS headers. The boot block is then unfilled, and the page falls back to the defaults in `src/boot.ts`, which mirror `internal/apps`.
- **Tests:** Vitest for the logic and components (happy-dom), Playwright e2e against a real server built with the page embedded, which also checks that no CSP violation happens on any section, dialog or theme.

### The client package (`client/`)

`@flighter7/workspace-client` is the client a TypeScript web app syncs through. It is a pnpm package of its own, not a workspace: the root stays a Go module, and Go ignores the folder. Its README is the documentation for app developers.

| Module | Job |
| --- | --- |
| `PairingCode.ts` | Parses a `wsrv1.` code into what the device keeps. |
| `serverAnswer.ts` | Builds the server's SDP answer from the pairing, with no signaling. |
| `openServerChannel.ts` | `RTCPeerConnection` with no ICE servers, one data channel labelled `workspace`; a closed channel closes the peer connection. |
| `ChannelFraming.ts` | Text frames, bodies in 16 KiB pieces, flow control at 1 MiB / 256 KiB. |
| `ServerClient.ts` | The messages, one request at a time; `changed` pushes; an unknown error code reads as `internal`. |
| `PairingStore.ts` | The pairing record in Web Storage under the app's key; every access guarded. |
| `SyncEngine.ts` | When to connect, sync and write; retries, halts, the write guard, the conflict loop. The app's `SyncAdapter` merges its document; its `DocumentError` halts sync. |
| `startInPage.ts` | Starts the engine and follows the page's visibility, the back-forward cache and the network. |
| `WorkspaceError.ts` | The error codes, by kind: server, pairing code, transport, engine. |
| `testing/` | `FakeServer` and `FakeChannel`, exported as `@flighter7/workspace-client/testing` for apps' tests. |

**The contract as data:** `testdata/conformance/` holds the contract as vectors. The client package's `conformance.test.ts` plays them against `ServerClient`, `parsePairingCode` and `buildServerAnswer`. The Go tests play the same files against the `Session` (`internal/protocol`), `EncodeCode` (`internal/pairing`) and the reference answer builder (`internal/rtc`). A change on one side that the other does not follow fails a test. A change to the contract changes `docs/protocol.md`, the vectors and both sides in one commit.

| File | Holds |
| --- | --- |
| `constants.json` | `proto`, the code prefix, the chunk size, the flow-control marks |
| `error-codes.json` | Every code the server sends |
| `pairing-codes.json` | Valid codes with their fields and what a client keeps; invalid ones with the client's error code |
| `answers.json` | Offer, pairing data, ufrag and time, to the exact SDP answer |
| `frames.json` | Every request and answer as frames, with the backend state that causes it; frames each side tolerates |
| `chunks.json` | Body sizes to the binary message sizes |

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
   - the **data folder**: `dataDir` from `config.json`, or the server folder. A missing data folder is created when its parent exists; otherwise the server does not start (`cannot start: the data folder … is not available` in the log). Creating it under a missing parent would show an empty server where the user's data used to be, on an unmounted disk;
   - `apps.json` (a first start writes the `todo` entry), plus the apps found in an adopted data folder;
   - `devices.json`.
5. Apply the flag overrides, then validate the result. The flags can set equal ports, for example. On an in-process restart the ports in effect at the end of the previous run take the flags' place, so a port changed on the page survives a reset or a move.
6. Start the UDP listener, then the web page on `127.0.0.1:<httpPort>`, then the mDNS responder.
   - The responder may fail; the server then works by IP only, and the page shows the responder as down.
   - Interactive mode's `OnReady` (open the browser) runs once, after the first start.
7. Run the watchers until a signal arrives (Ctrl+C, SIGTERM, SIGHUP, a closed console window on Windows) or the page asks the server to stop, reset or move its data.
8. Stop everything and log `stopping` and `stopped`. Then, with the lock and the log still held:
   - **reset:** delete `identity.json`, `apps.json`, `devices.json` and the data folder's `apps/`; keep `config.json`, `server.log` and the lock; go to step 4;
   - **move:** copy the data to the new folder, save `config.json`, delete the old copy (see "Changing the data folder"); go to step 4;
   - **stop or delete:** close the log, release the lock, then perform the removals (see "Stopping and removing the server").

A reset or move that fails is logged and shown by the page (`/api/status` `lastError`); the server comes back on the old state.

A restart that cannot start (a port taken in the gap, say) is retried every 2 s for a minute, keeping the lock and the log; once it starts, the page shows why the first attempt failed. Only then does the process end with the error, where only autostart would bring it back (launchd and systemd restart on failure; the Windows task does not). The first start is not retried: its error goes to the terminal or the log at once.

### Watchers

- **`apps.json`:** checked every 2 s; a hand edit takes effect.
  - An invalid file keeps the last good list; each distinct failure is logged once.
  - An app dropped from the file has its sessions closed: every app with sessions that is not listed, not only those listed before.
  - A key the server does not read (a misspelt `histroy`, say) is logged with the file's path and ignored, not refused, so that a file from a later version still loads. Keys match without regard to case, as `encoding/json` does.
  - **A page edit and a hand edit:** Add and Remove first take in a hand edit the watcher has not read yet, and apply their change to the file as it is now; the next check reports the list as changed. An invalid hand edit refuses an Add (400) and is replaced by a Remove, whose app's data may already be gone. The registry remembers its own writes by the new file's stamp, taken before the rename, so an edit right after a write is never mistaken for it.
- **LAN addresses:** checked every 30 s. On a change, the listener and the responder are rebuilt, so every live session ends and clients reconnect.
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

The page asks `GET /api/data-dir?path=P` first; the server answers a verdict and the apps' data on each side.

- **The server's app folders** are the folders under `apps/` with a valid app id that hold `data.json` or `history/`. `apps` is a common folder name and the user may pick a folder that already has one, so nothing else under `apps/` is data, and nothing else is ever moved or deleted: a reset, a move and `delete` remove only the server's app folders (and empty ones with a valid id), and `apps/` itself only when it is then empty.
- **"Has data"** means a regular file in one of the server's app folders (a document or a history version).

| Verdict | When | `PUT /api/config {"dataDir": P, "verdict": V}` |
| --- | --- | --- |
| `same` | the current folder | 204, nothing |
| `create` | missing, its parent exists | create it, move the data if any |
| `move` | exists without data; the current one has data | move the data |
| `switch` | neither has data | switch only |
| `adopt` | only P has data | switch, and add every `apps/<id>` with a valid id to the app list |
| `conflict` | both have data | 409 `dataExists` |
| `unavailable` | relative, not a folder, not writable, inside the current `apps/`, or its parent is missing | 400 `invalid` |

- **`PUT` carries the verdict the page showed** (`verdict`, required with `dataDir` and allowed only with it) and checks the folders again, since they may have changed: `conflict` and `unavailable` are refused as in the table, and any other verdict that differs from the page's is 409 `changed`, so the user never gets a move they were not shown. Otherwise it answers 202; the handler flushes the answer and only then restarts the server.
- **One change at a time:** a `PUT` while a change waits for its restart, or while the server stops, is 409 `busy`.
- **The move checks a third time,** once the server has stopped: a verdict that changed since the `PUT` (data appeared in the current folder for a `switch`, say, which would stay behind unseen) changes nothing and goes to `lastError`.
- **The move** is copy, then switch, then delete: copy the server's app folders to `P/apps.partial` (each file fsynced), put them in place (rename `apps.partial` to `apps`, or each app into an existing `P/apps`), save `config.json`, delete the old app folders, and then the old folder itself if it is empty and is not the server folder. A crash before the save leaves the old folder current; after it, the new one current and a stale copy behind. A failed copy removes `apps.partial` and keeps the old folder.
- A data folder equal to the server folder is saved as an empty `dataDir`.
- The data folder holds only `apps/`; the log stays in the server folder, so it can say why an unreachable data folder stopped the server.

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
  - The queue holds at most the app's `maxBytes` plus 1 MiB (1 MiB before authentication). An item that would take it past that closes the session, without an error frame, which could not go ahead of the queue anyway. An item that finds the queue empty is always taken, so a document stored before the app's limit was lowered can still be read. The close runs on its own goroutine: `PushChanged` queues under the hub's app lock.
- **Pairing confirmation:** a successful `pair` leaves the session unconfirmed until the client sends any further frame. A session that ends unconfirmed (closed during `Pair`, `paired` dropped from the queue or failing to send, or the client gone before it answered) calls the backend's `Forget` for the new device. This is the only way the server learns that the token arrived: a frame handed to SCTP may still be lost.
- **Flow control:** pause above 1 MiB buffered, resume at 256 KiB.
- **Receiving bodies:** they are reassembled from binary messages of up to 16 KiB. An oversized `put` is discarded as it arrives and never buffered.

### Hub (`internal/hub`)

- **Registering a session:** on a successful `hello`/`pair`, the session is registered under its app **before** the welcome's etag is read (register, then read).
- **Holding `changed`:** a `changed` that arrives before the welcome is held and sent right after it, unless it equals the welcome's etag. Together these mean no missed change and no change before `welcome`.
- **Locking:** a per-app lock covers admission, put plus push, and removal.
- **Rate limit:** an IP is blocked at its 10th failure within a minute, for 10 minutes. While it is blocked, `hello` and `pair` get `blocked` without any check.
- **The session cap** (64) lives in the listener.
- **Removals:**
  - removing a device or an app closes its sessions;
  - `forget` closes every session of the device;
  - a `put` from a device removed meanwhile gets `badToken` and is not stored.
- **Internal errors** reach devices as a generic message; the detail goes to the log.

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

## Storage

### The server folder and the data folder

| OS | Server folder (default) |
| --- | --- |
| macOS | `~/Library/Application Support/workspace-server` |
| Windows | `%APPDATA%\workspace-server` |
| Linux | `$XDG_DATA_HOME/workspace-server`, default `~/.local/share/workspace-server` |

`-dir <dir>` names another server folder (for tests and a second server). The commands make it absolute and clean first, so a relative or trailing-separator `-dir` still matches the default folder (the main server) and the paths the page compares. The data folder is `config.json`'s `dataDir`, or the server folder when it is empty.

```
server folder:
identity.json          0600, created once, never changed (a reset deletes it)
config.json            what the user may change, dataDir included
apps.json              the apps this server serves
devices.json           0600, paired devices
server.log             the log; never above 1 MiB
server.lock            one server per server folder

data folder:
apps/<id>/data.json    the app's current document
apps/<id>/history/     previous documents
```

### The files

- **`identity.json`** is `{serverId, certPem, icePwd, mdnsName}`.
  - The certificate is ECDSA P-256 in Pion's `Certificate.PEM()` form, valid for 20 years. Clients pin its SHA-256 fingerprint.
  - Losing the file means every device pairs again. Copying the server folder (and the data folder) to another machine keeps every pairing.
  - On load every field is checked as it was made: `serverId` 12 and `icePwd` 24 characters of `[A-Za-z0-9]`, `mdnsName` a lowercase `<uuid v4>.local`, and the certificate. A damaged file stops the server with its path and the field (never the password's value) and is left untouched, instead of failing later as an rtc or mDNS error.
- **`config.json`** is `{name, udpPort, httpPort, dataDir}`, with defaults host name, 47800, 47801 and no `dataDir` (the server folder). `dataDir` must be absolute.
- **`apps.json`** is `{apps: [{id, name, history, maxBytes}]}`.
  - `id` matches `^[a-z0-9-]{1,64}$`. It becomes a path segment and never changes.
  - `history` defaults to 10 (range 1–100).
  - `maxBytes` defaults to 64 MiB (range 1 KiB–1 GiB). Zero values mean the default.
  - The list decides which apps may pair. It is configuration, not security: the device token is the security.
- **`devices.json`** is `{devices: [{id, app, name, tokenHash, pairedAt, lastSeen}]}`, one entry per device per app.
  - Times are RFC 3339 UTC, and are read into UTC whatever offset the file holds.
  - `lastSeen` is written at most once a minute per device. A write that fails is retried, and logged, at most once a minute too.
  - On load, an entry with an empty or repeated id, an invalid app id or a `tokenHash` that is not 64 lowercase hex digits stops the server with the file's path and the entry's index; the file is left untouched, like an invalid `apps.json` or `config.json`. Dropping such entries instead would lose them at the next write.
- **Documents** are exactly the bytes the last writer sent; the etag is their lowercase hex SHA-256.
  - A write goes to a temporary file in the same directory. It is fsynced and renamed over the target, under a per-app lock covering the compare and the write.
  - Before a write replaces a document, the old one becomes `history/<UTC yyyymmddThhmmss.mmmZ>-<first 8 hex of its etag>.json`, and the oldest beyond `history` are deleted.
    - History is ordered by the time in the name, not by the name. Names from before 2026-11-10 have no milliseconds (`…ssZ`) and sort by their time among the new ones.
    - A new name is a millisecond after the newest entry when the clock is not past it (several writes within a millisecond, a coarse clock, a clock stepped back), so the names keep the order of the writes and pruning keeps the newest.
    - No entry is made when the newest one already holds the replaced document (same etag prefix and same bytes): a rewrite of the same bytes, or a crash between the history link and the rename, which left the current document in history.
  - History uses `os.Link`. Where hard links fail (exFAT/FAT, SMB/CIFS), the old bytes are written as a copy instead.

### Atomic writes and the lock

- **Atomic writes:** all registry files use `fsutil.WriteFileAtomic`, which writes a temporary file, fsyncs it, renames it and then fsyncs the parent directory, so the rename survives a power loss. The directory sync is best effort (some network and FUSE mounts refuse it): once the file is renamed, a failed sync must not make a caller undo the write. On Windows it is skipped: a directory handle opened for reading cannot be flushed, and NTFS journals renames itself. The data-folder move syncs the folders it copied into and the ones it renamed into, the same way.
- **The lock:** `server.lock` uses `flock(LOCK_EX|LOCK_NB)` on Unix and `LockFileEx(EXCLUSIVE|FAIL_IMMEDIATELY)` on Windows.
  - It is held for the whole run.
  - The file is never deleted while a server could hold it.

## The web page (`internal/web`)

- **Address:** `http://127.0.0.1:<httpPort>/`, listening on `127.0.0.1` only.
- **Content:** the page built from `client-wss/` (see above), embedded from `ui/dist`:
  - `GET /` is its `index.html`, run once per request as an `html/template`: `{{.Boot}}` in `<script type="application/json" id="boot">` becomes `{nonce, history, maxBytes}`, the CSP nonce and the add form's defaults (`apps.DefaultHistory`, `apps.DefaultMaxBytes`);
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
  - `GET /api/data-dir?path=`: check a data folder.
- **`/api/status`** also carries `listening` (the UDP port is open, so devices can connect), `program` (the program file), `serverDir`, `dataDir`, `mainServer` (the default server folder) and `lastError` (why the last reset or move failed, or why a restart needed retries).
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

1. **Delete** only: delete the data (the server's app folders in a data folder other than the server folder; `apps/` and the folder itself only if then empty, since the user may have chosen a folder holding other files).
2. **Delete** only: delete the server folder. `removeServerDir` refuses a folder holding none of `identity.json`, `config.json`, `apps.json`, `server.lock` (a mistyped `-dir`), and a folder another process has locked; it deletes everything but `server.lock` while holding the lock, then the lock file and the folder.
3. **Delete**, main server: remove the Windows Apps & features entry (`HKCU\…\Uninstall\workspace-server`; a fixed key, so at most one entry).
4. **Delete**, main server: delete the program file: `program.Remove` deletes one regular file, never a folder or a symlink. Unix unlinks it at once. Windows cannot delete a running program's file, so a hidden PowerShell helper retries `Remove-Item -LiteralPath` every 0.5 s for 10 s after the process exits.
5. **Delete** or **stop with `removeAutostart`**, main server: `autostart.Unregister()`, **last**, because on some systems it ends the calling process. Just before it the process gives SIGINT, SIGTERM and SIGHUP back their default action: `launchctl bootout` ends the agent it unloads with SIGTERM, and a caught SIGTERM would leave the process waiting for launchd's kill.

Each step runs even if an earlier one failed; failures go to stderr (the log may be gone). On Windows the helper is a hidden Windows PowerShell from the system folder (`CREATE_NO_WINDOW|CREATE_NEW_PROCESS_GROUP`, working folder the system folder, nothing waited for); the entry is written and deleted with `x/sys/windows/registry`, and its key is a parameter of the inner functions so that tests use a scratch key.

## Packaging, CI, release

- **Binaries:** one per platform, everything embedded, with no runtime:
  - `wss-darwin-arm64`, `-darwin-amd64`;
  - `-windows-amd64.exe`;
  - `-linux-amd64`, `-linux-arm64`.

  The guide has the user rename the file to `workspace-server`; the program does not depend on its own name.
- **`scripts/build.mjs [version]`** builds a release locally and in CI alike: the web page (`pnpm install --frozen-lockfile`, `build`), then the five binaries with `CGO_ENABLED=0 -trimpath -ldflags "-s -w -X main.version=<version>"`, and `SHA256SUMS` in `sha256sum`'s format, into a clean `dist/`. The platform list lives there only. `dist/` holds nothing else; throwaway binaries, servers' data, e2e data and check reports go to `tmp/`.
- **No code signing:** the first run meets Gatekeeper and SmartScreen, and the firewall asks once (on macOS again after each update).
- **`.github/workflows/ci.yml`:** runs on `ubuntu-latest`: `gofmt`, `go vet ./...` natively and for Windows, macOS and FreeBSD, `go test -race ./...`. The cross-vets compile the other systems' code but run none of it.
- **`.github/workflows/release.yml`:** on a `v*` tag:
  - runs `ci.yml` first, and builds nothing if it fails;
  - runs `node scripts/build.mjs <tag>` in one job, which stops if the page did not build;
  - in a job of its own with write access, checks `SHA256SUMS` and publishes `dist/*` as a release, a pre-release when the tag has a `-` (`v1.0.0-rc1`).

  Without the ldflag the version is `dev`.
- **The client package in CI:** `ci.yml`'s `client` job runs on `ubuntu-latest`:
  - pnpm, at the version in the package's `packageManager`;
  - `pnpm run check`: lint, Prettier, typecheck, unit and conformance tests, build;
  - then the e2e, which builds the server from the same checkout.

  `release.yml` runs it with the rest of `ci.yml`, so binaries are built only after the package passes.
- **The web page in CI:** `ci.yml`'s `page` job does the same for `client-wss/`: `pnpm run check` (lint, Prettier, unit tests, typecheck, build), then the e2e against a server built with the page embedded. The Go job never builds the page: `internal/web`'s tests serve a stand-in from `fstest.MapFS`, and read `client-wss/index.html` itself to check it is the template the server fills.
- **Publishing the package: by the user, by hand.** No workflow and no agent publishes it.
  1. Set `client/package.json`'s `version` to the server's tag, `0.1.0` for `v0.1.0`.
  2. Run `pnpm --dir client publish --access public`; add `--tag next` for a pre-release. `prepublishOnly` runs `check` first.
  3. npm asks for 2FA.

  A manual publish has no provenance attestation; that was accepted. The package ships `dist/` and `src/` (so source maps and declaration maps resolve). `prepack` copies the root `LICENSE` in, and `postpack` removes it again.
- **Docker:** an image for a NAS is a later step. It needs host networking for UDP and mDNS.

## Decisions and why

Each entry gives the decision, the reason, and the cost if it is wrong.

### Protocol and hub

- **Apps (R1):** `apps.Add` treats zero `history`/`maxBytes` and an empty name as the defaults, and only then validates the ranges.
  - Why: the page's "Add it?" posts only the id.
  - Cost: an API caller sending 0 gets the default, not a 400.
- **Device writes (R2):** the `lastSeen` throttle counts only `lastSeen` writes, failed ones included (2026-11-10), so a broken disk is not retried and logged on every `hello`.
  - Cost: one extra `devices.json` write after pairing.
- **Oversized puts (R3):** a `tooLarge` reply follows the discarded body.
  - Why: every reply to a `put` follows its body.
  - Cost: the client learns of the refusal slightly later than it could.
- **Frame order (R4):** frames are strictly ordered through one non-blocking send queue.
  - Why: the framing rule, and a blocking push would stall broadcast on a slow peer.
  - Cost: a session that does not read fills its queue; at the app's `maxBytes` plus 1 MiB it is closed (2026-11-10). Repeated `get`s are not coalesced, since frames carry no request id and every `get` gets its own `doc`.
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
- **History on exotic volumes:** history falls back to copies where hard links fail.
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
- **D7, two folders:** a fixed server folder (settings, identity, registries, log, lock) and a data folder (`apps/`) the page can move, stored as `dataDir`. `-data` became `-dir` (the server folder). Autostart, the program file and the Apps entry belong to the default server folder only.
  - Cost: two servers with different server folders but one `dataDir` would share `apps/`; nothing prevents it, since a lock in the data folder would add a file the user did not ask for.
- **D8, the Windows Apps & features entry** is one fixed key: written by every `start`, kept by `stop`, removed by `delete`.
- **D9, the log** is cut to 512 KiB (half the limit) when a line would take it past 1 MiB; one file.
- **D10, the data folder:** a missing folder whose parent exists is created silently; both folders holding data is refused; only the target holding data adopts it and lists its apps; deleting a data folder removes only `apps/`, and the folder only if then empty; an unreachable data folder at start stops the server with a log line in the server folder.
- **Moving data copies, then switches, then deletes** (never a rename across the switch), so a crash leaves a consistent state. Cost: the data is copied, not renamed, even on one volume.
- **The CLI's stop request is always `{}`;** the CLI removes autostart itself after the server is gone (otherwise a dying server could unregister what `start` just registered). `stop` and `delete` end by waiting until the page is gone and the lock is free, since the OS-level stop is asynchronous.
- **`checkDataDir` writes a probe file** to test that a folder is writable. Cost: a GET touches the folder briefly.
- **`removeServerDir` proceeds without the lock** where the file system has none, as the server itself does.
- **The Windows task** is built from XML for the current user. It was verified on 2026-11-09.
- **The systemd unit** is at a fixed path, and `Uninstall` deletes it even on a `systemctl` failure.
- **A program path with `$`** is refused by `start` on Linux (2026-11-10) rather than escaped: whether systemd undoes `$$` in the executable cannot be checked without a Linux machine. Cost: such a user moves the program first.
  - Cost: a user manager with a custom `XDG_CONFIG_HOME` reports "unit does not exist".

### The client package (agreed with the user, 2026-11-10)

- **C1, one client for every app:** the transport and the sync engine moved out of the todo app into `client/`, published as `@flighter7/workspace-client`. An app passes its app id and a `SyncAdapter`, which merges its document. The engine decides when to sync and runs the protocol. The server merges nothing.
  - Why: every next app would otherwise copy the most fragile code: the signaling-free answer, flow control, pairing confirmation, retries, the write guard.
  - Cost: a change to the engine reaches an app only through a new package version.
- **C2, no Protobuf:** considered as a single source for the contract and turned down. The contract stays prose in `protocol.md`, pinned by the vectors in `testdata/conformance/`.
  - Why: every planned client is a TypeScript web app. Protobuf would generate only about 15 message types; the drift that hurts is in what it does not describe (the SDP answer, the pairing code, chunking, the error policy), and the vectors cover those too.
  - Cost: a client in another language (Kotlin, Swift) would write its own message types, checked against the same vectors.
- **C3, the wire did not change:** `proto` stays 1, and the frames are JSON as before. Nothing had been released, so there was no old client to keep.
- **C4, a contract change is one commit:** `protocol.md`, the vectors, the server and `client/` change together, and `proto` goes up for any change an old client cannot handle.
- **C5, publishing is by hand:** the user publishes to npm; no workflow and no agent does. An app tries a change against `link:../workspace-server/client` before it is published.
  - Cost: no provenance attestation on the published package.
- **C6, the package version is the server's tag** (`0.1.0` for `v0.1.0`), so a package and the server it was tested against are easy to pair up.
- **C7, a pnpm package of its own, not a workspace:** the root stays a Go module, and Go ignores the folder. `client/.gitattributes` keeps LF, which Prettier needs, whatever `core.autocrlf` says.
- **C8, licence:** CC BY-NC-ND 4.0 for the whole repository, `LICENSE` at the root. `prepack` copies it into the package.
  - Cost: Creative Commons does not recommend its licences for software, and ND forbids others to distribute changed copies. The author's own apps are not affected.
- **C9, todo's devices did not pair again:** the package kept todo's pairing record shape, the data channel label `workspace` and every error code. todo passes its own key `todo.server` and app id `todo`.

### Process

- **Development (R10):** the Go rewrite was developed directly on `main`, one reviewed task at a time. Every commit carries the orchestrator's attribution line.

## Limits

- IPv4 and the LAN only. No accounts, one document per app, no merging.
- No code signing.
- On Windows, a logon-started server may flash a console window for an instant before it lets go of it (see "Commands, autostart and removal"; none was seen at a real logon on 2026-11-10). A process without a console gets no console events, so at logoff Windows ends it hard. That is safe: writes are atomic, and the OS releases the lock.
- A server started with `-http-port` that is not in `config.json` is not found by `probeServer`; opening the file then runs into the lock and exits 1, and `stop` falls back to the OS-level stop.
- Two servers whose `dataDir` is the same folder share `apps/` unprotected.
- Deleting the program file by hand on Windows leaves its Apps & features entry; Windows does not clean it up.
- The mDNS responder:
  - guesses the arrival interface on Windows, by subnet;
  - suppresses only copies of a query on one interface within 250 ms, not the duplicate answers of RFC 6762 §7.4 that other responders sent;
  - has not been tried on a Windows machine with several adapters.
- `.local` resolution costs about 1 s on Windows.
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
- The store: atomic write, history (order within one second, old names, a crash leftover), the copy fallback, conflicts, the JSON check.
- The protocol session over a fake channel that keeps frames sent after a close apart, so "error frame, then close" is pinned; the queue cap; pairing confirmation; the sender goroutine exiting after every kind of close.
- The hub: authentication, broadcast, rate limit, removal, and its failure branches (a lost secret race, unwritable `devices.json`, a failing welcome, internal errors), with a hook inside the welcome to pin the orders a race can take.
- mDNS packet handling, as a pure function, plus tests on the real port 5353.
- The web guard and handlers.
- The autostart file builders (the task XML's element order included), `runCommand`'s error wrapping through a helper process, and the order of the `Unregister`/`Uninstall` steps.
- The program entry's values and the PowerShell helper's quoting; deleting the program file on Unix.
- On Windows only: the detached start (no console window, the system folder as working folder), the entry written to and deleted from a scratch registry key, and the helper deleting a file whose name holds spaces, quotes, `$` and a backtick, and one that is open for a while. The task list parser is pure and runs everywhere.
- `runServer`: start, restart, the lock, the address change; stop, reset, data move and adopt, deletion and its order, main and side servers.
- The data folders: every verdict, the copy, reset and the safe deletions.
- The log cap, under concurrent writers.
- The commands with fakes: interactive, `run`, `start`, `stop`, `delete`.
- The control client against a fake server.
- An integration test: the real UDP listener on loopback with a Pion client using the client-built answer. It pairs, puts and gets, and receives `changed` on a second client. It also checks that a wrong password is ignored.
- The listener's admission (password, session cap, recently ended ufrags) by feeding binding requests straight to its packet handler, and the teardown's drain with a stand-in peer connection.
- The contract vectors (`testdata/conformance/`), played against the `Session`, `EncodeCode` and the reference answer builder, and by the client package against its client.
- The client package:
  - its unit tests: framing, the pairing code, the protocol client, the pairing store, the answer;
  - the engine against a small union-merge adapter and a `FakeServer`;
  - the page lifecycle with stand-in event targets;
  - its e2e: the built package in Chromium, against a server built from this checkout. Two devices pair, merge and receive each other's edit through a `changed` push; a used code gets `badSecret`; another app id gets `unknownApp`.

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
