# Protocol

This is the contract between `workspace-server` and an app's client. The reference client is the npm package in [client/](../client/README.md), `@flighter/wss-client`; the todo app (`FLighter7/todo`) syncs through it.

- **Changing the contract:** change it only together with the client package and the vectors in [testdata/conformance/](../testdata/conformance), in the same commit. The vectors hold this contract as data, and both the Go tests and the package's tests play them. Bump `proto` for any change an old client cannot handle.
- **Reading order:** [architecture.md](architecture.md) explains how the server implements this contract and why. This file says only what a client must know.

## Overview

- **The connection:** a device talks to the server over one WebRTC data channel.
- **No signaling:**
  - after a one-time pairing, the device builds the server's side of the connection itself, from data in the pairing code;
  - no message is exchanged before the first STUN packet.
- **Authentication:** DTLS is authenticated by the server certificate's fingerprint, pinned in the pairing code. After that the device sends `hello` with its token, or `pair` with a one-time secret.
- **Scope:** IPv4 and the local network only.
- **The data:** each app holds a set of documents, plain files on the server's disk, in any format. A device lists the set, reads documents at the etags the listing named, and writes them in transactions that apply all or nothing and fail on a conflict; bodies are streamed in pieces, and a read or a write cut off resumes from where it stopped. See "Documents and the set" and "Transactions".
- **Servers as clients:** a workspace-server can link to another as its subordinate. It speaks this same protocol as a device of kind `server`; see "Servers: roles, server-kind devices and link sessions". An app's client never needs that part.
- **Fallback servers:** a main lists its subordinates in every `welcome`, and a device may sync with any of them, with the same device id and token, when the main is out of reach; see "Subordinates: fallback servers for a device".
- **Divergence:** when a subordinate's copy of an app and its main's both changed apart, the devices of the app are asked which copy to keep; see "Divergence".
- **Switching the main:** a subordinate can take the role of main over; every `welcome` names the family's `term`, and a device follows the main of the highest term it reaches; see "Switching the main".

## The pairing code

The page's **Pair a device** creates a one-time **secret**:

- 128 random bits, base64url;
- valid for 10 minutes or one successful pairing, whichever comes first;
- kept in memory only;
- issuing a new secret revokes the previous one.

The **pairing code** is `wsrv1.` followed by the base64url encoding, without padding, of this JSON:

```json
{
  "v": 1,
  "id": "<serverId>",
  "n": "<server name>",
  "p": "<icePwd>",
  "f": "<base64url, no padding, of the 32-byte SHA-256 certificate fingerprint>",
  "o": 47801,
  "h": ["<uuid>.local", "192.168.1.10"],
  "s": "<secret>"
}
```

- `serverId`: 12 characters `[A-Za-z0-9]`.
- `icePwd`: 24 characters `[A-Za-z0-9]`.
- `o`: the UDP port.
- `h`: the server's `.local` name first, then its current LAN IPv4 addresses.
- The code carries no token. The device receives its token in `paired` and keeps it. The server stores only its SHA-256.
- The page shows the code as a QR code and as text with a Copy button.

### The `.local` name

The server answers mDNS for its own `<uuid v4>.local` name, lowercase, with **A records only**. It answers AAAA with an NSEC record.

- **Why only a UUID name works:** WebKit connects to a `.local` host candidate only when the name looks like a browser-generated one (`<uuid>.local`).
- **Why A records only:** libwebrtc prefers IPv6 when a name has both kinds of record, and the server listens on IPv4 only.

Put the name first among the candidates. Resolving it costs about 1 s on Windows, which waits that long for AAAA. The IP candidates make the connection independent of mDNS.

## The client's side of the connection

1. Create `RTCPeerConnection({iceServers: []})`.
2. Create one data channel, then an offer, then call `setLocalDescription`.
3. Set this remote answer, with lines joined by CRLF:

```
v=0
o=- <now> 2 IN IP4 127.0.0.1
s=-
t=0 0
a=group:BUNDLE <mid>
a=ice-lite
a=msid-semantic: WMS
m=application <udpPort> UDP/DTLS/SCTP webrtc-datachannel
c=IN IP4 0.0.0.0
a=candidate:<n> 1 udp <priority> <host> <udpPort> typ host      (one per host in `h`, the UUID name first)
a=end-of-candidates
a=ice-ufrag:<8 random [A-Za-z0-9], new for every connection>
a=ice-pwd:<icePwd>
a=fingerprint:sha-256 <fingerprint, uppercase colon hex>
a=setup:passive
a=mid:<mid>
a=sctp-port:5000
a=max-message-size:262144
```

- `<mid>` is copied from the client's own offer.
- The server is ICE-lite with a fixed password. The client chooses the server's ufrag, and it must be new for every connection: the server keys connections by it.
- The server accepts a connection only when the first STUN binding request carries `USERNAME` `<serverUfrag>:<clientUfrag>` and a MESSAGE-INTEGRITY that verifies against `icePwd`. A request with a wrong password is dropped silently, so the client simply sees no connection.

## Session lifetime

- **The data channel must open** within 20 s of the connection starting. Otherwise the server closes the peer connection.
- **The first frame must authenticate** (`hello` or `pair`) within 10 s of the channel opening. Otherwise the server closes the session, without an error frame.
- **No idle timeout.** The browser's ICE consent checks (RFC 7675, about every 5 s) keep a live session up. The server notices a vanished client after about 11 s.
- **The session ends** when:
  - the data channel closes;
  - or the connection goes `disconnected`, `failed` or `closed`.
- **Server-side endings:** the server ends every live session when:
  - its LAN addresses change (it rebuilds the listener, and clients reconnect);
  - the UDP port changes;
  - the server stops.
- **When the data channel closes, the client should close its peer connection.** Otherwise a connection whose server went away lingers.


## Limits

- **Sessions:** at most 64 at once, authenticated or not. Binding requests beyond that are ignored.
- **Unread replies:** what the server has queued for a session and not yet sent may hold at most 1 MiB of text frames and at most 4 `doc` bodies besides the one being sent; a body waits in the queue as an open file, never as its bytes. A session that would queue more is closed, without an error frame. A client that reads what it asks for never comes near it; one that repeats `get` without reading the `doc`s does. A text frame that finds the queue empty is always taken.
- **Failed attempts:** an IP is blocked at its 10th failed `hello`/`pair` within a minute, for 10 minutes from that failure.
  - While it is blocked, every `hello` and `pair` from it gets `error {code: 'blocked'}` and the channel closes.
  - Nothing is checked and no pairing secret is consumed.
  - `blocked` is temporary, and the pairing still stands. A client keeps its pairing, tells the user to wait a few minutes, and retries later.
  - Attempts made during the block do not extend it.
- **The quota:** each app's documents together may take at most its quota, 50 MiB by default, set on the server's page (see `quota` below).
- **A list page** carries at most 500 entries, documents and tombstones together. A client counts on no page size.

## Framing

- **Text frames:** JSON objects with a `t` field.
- **Bodies:** a frame that announces a body carries `bytes`, the document's whole size, and `offset`, where the body starts (0 when absent). That is `put` from the client and `doc` from the server.
  - Exactly `bytes − offset` bytes follow: the document's bytes past the offset, as binary messages, cut into pieces of at most 16 KiB from the offset on. A receiver takes pieces of any size up to 16 KiB.
  - No binary message follows when `offset` equals `bytes`.
  - One body is in flight per direction at a time.
  - A binary message that arrives when no body is expected, or that overruns the announced size, is a protocol error.
- **Order:** the server never interleaves a text frame inside a body it is sending. Frames requested meanwhile go after the body. Every answer to a `put` follows its body, a refusal included.
- **Flow control:** a sender pauses while more than 1 MiB waits in the channel's buffer, and resumes when the buffer drains to 256 KiB.
- **Streaming:** neither side needs a body whole in memory. The server writes a `put` body to disk as its pieces arrive and sends a `doc` body from the file as the channel takes it; the reference client sends a `Blob` a slice at a time and can hand a `doc` body to the app piece by piece.

## Documents and the set

An app holds a **set** of documents, kept by the server as plain files in the app's folder.

- **A document** is named by a key of three parts, which every frame about a document carries:
  - `path`: its folder, `""` for the app's root, otherwise segments of `[a-z0-9_-]` joined by `/`, at most 10 of them;
  - `id`: `[a-z0-9_-]{1,64}`, chosen by the client;
  - `type`: `[a-z0-9]{1,8}`, the file's extension, part of the identity (`a/r.jpg` and `a/r.png` are two documents).

  The server keeps it as `<path>/<id>.<type>`, `<id>.<type>` at the root: its **canonical form**, at most 255 characters. No segment and no id may be a Windows device name (`con`, `prn`, `aux`, `nul`, `com0`–`com9`, `lpt0`–`lpt9`). Uppercase, dots and empty segments are not allowed. A key that breaks a rule is refused with `badPath`. Moving a document is deleting it and creating another. The server checks no format: the bytes are the app's.
- **`etag`** is the lowercase hex SHA-256 of a document's bytes, so two identical writes agree, on every server.
- **`revision`** names the whole set: the lowercase hex SHA-256 of the lines `<canonical key>\0<etag>\n`, sorted by key byte for byte, or `null` for an empty set. Servers holding the same files have the same revision, so a client compares sets by it alone.
- **Tombstones:** the set lists the documents deleted from it, with when (`at`, RFC 3339 UTC), the newest 1,000. A document created again drops its tombstone.
- **Files changed by hand:** the server checks the app's folder at every `hello` for the app, and takes in what changed there: a changed file gets a new etag, a new file with a valid name becomes a document, a vanished one a tombstone. A `get` or a `commit` that finds a file changed since the server last saw it takes it in the same way. The set then has a new revision, and every session of the app gets `changed`. A file whose name is not a key is not a document, and is never shown or touched. A file the server cannot read (another program holds it, or its permissions refuse the server) is left as the set last held it, and taken in once it can be read; it keeps no device out, but a `commit` that touches it is answered `internal` and its transaction stays open, until the file can be read.

## Messages

The first client frame, `hello` or `pair`, carries `proto: 1`. Any other number gets `error {code: 'protocol'}` and the channel closes.

`proto` stayed 1 when the set of documents, with its transactions, replaced the single JSON document an app had before (2026-11-13): nothing had been released. A client of the single document cannot sync with this server.

| Client → server | Server → client |
| --- | --- |
| `hello {proto, app, device, token}` | `welcome {proto, role, term, serverId, name, revision, hosts, subordinates, diverged}`, or `error` and the channel closes |
| `pair {proto, app, secret, name}` | `paired {device, token}` then `welcome {…}`, or `error` (closes unless `unknownApp`) |
| `head` | `head {revision}` |
| `list {revision?, cursor?}` | `list {revision, docs, removed, cursor?}`, or `stale {revision}` |
| `get {path, id, type, etag?, offset?}` | `doc {path, id, type, etag, bytes, offset}` + body, or `stale {revision}` |
| `begin {tx}` | `began {tx, staged}` |
| `put {tx, path, id, type, ifEtag, bytes, offset}` + body | `staged {path, id, type, received}` |
| `delete {tx, path, id, type, ifEtag}` | `staged {path, id, type, received: 0}` |
| `commit {tx}` | `committed {revision, docs}` or `conflict {revision, docs}` |
| `abort {tx}` | `aborted {tx}` |
| `resolve {server, keep}` | `resolved {server}` |
| `forget` | the device entry is deleted; the server closes every session of the device |
| `apps` (server-kind devices only) | `apps {apps}` |
| `diverged {app, since}` (link sessions) | nothing: the main keeps it (see "Divergence between servers") |
| `announce {server, devices?, removed?}` (link sessions) | `devices {devices, removed}` |
| `invite` (link sessions) | `invited {code, expiresAt}` |
| `term {term, server, device?, token?}` (link sessions) | `term {term, main}` |
|  | `changed {revision}`: the app's set has a new revision (see "Pushes") |
|  | `diverged {servers}`: the servers whose copy of the app diverged from their main's (see "Divergence") |
|  | `resolved {server}`: a divergence was settled (see "Divergence") |
|  | `apps {apps}`: to every link session after the server's app list changes |
|  | `devices {devices, removed}`: to every link session that announced, after the device registry changes |
|  | `takeOver`: to a subordinate's link session, when its main hands it the role of main |
|  | `resolve {app, keep, since}`: to a subordinate's link session, a choice a device of the main made for its divergence (see "Divergence between servers") |
|  | `error {code, message}` |

`pair` may also carry `kind`, and a server-kind device's `hello` may leave `app` out; both, `apps`, `diverged` from a client, `announce`, `invite`, `term`, `takeOver` and `resolve` from the server, are for servers only (see "Servers: roles, server-kind devices and link sessions" and "Switching the main").

### Values

- **`revision`** in `welcome`, `head`, `list`, `stale`, `changed`, `committed` and `conflict` is the set's revision, a string, or `null` for an empty set.
- **`hosts`** is `[mdnsName, ...LAN IPv4]`, the server's current list. A client can refresh the hosts it stored at pairing from it, since addresses change.
- **`role`** is `"main"` or `"subordinate"`: whether the server is linked to an upstream server (see "Servers"). A server from before roles sends none. An app's client may ignore it.
- **`term`** is the family's term, a whole number: it goes up by one every time a subordinate becomes the main (see "Switching the main"). A main names its own, a subordinate its main's. It is left out at 0, as it is by a server whose family never switched its main and by a server from before terms; absent reads as 0. An app's client may ignore it; the reference client uses it to follow the main.
- **`subordinates`** is sent by a main only, always, `[]` when it has none: the servers linked to it that serve its devices too, each under the name the user gave it on the main's page or else its own, sorted by name. A subordinate, and a server from before subordinates, sends none. Each entry is a pairing code without the secret, `{serverId, name, password, fingerprint, port, hosts}`, with the fingerprint in uppercase colon hex (`AB:CD:…`, 32 bytes) as the answer's `a=fingerprint` line takes it. A client leaves out an entry it cannot build a connection to (see the vectors' tolerated frames). See "Subordinates: fallback servers for a device".
- **`diverged`** lists the divergences of the app that wait for a device's choice, `[{serverId, name, since}]`: the subordinate whose copy diverged, and since when (RFC 3339 UTC); it is left out when there is none (see "Divergence").
- **`device`** is the device id, 16 characters.
- **`token`** is 32 random bytes, base64url.
- **`name`** in `pair` is the device's display name, shown on the server's page.
- **`tx`** is a transaction id the client chooses: 16 to 64 of `[A-Za-z0-9_-]`. Another is `protocol`, and the channel closes.

### Listing

- **`list`** answers with one page of the set: `revision`, `docs: [{path, id, type, etag, bytes}]`, then `removed: [{path, id, type, at}]` (tombstones), each sorted by canonical key byte for byte, and `cursor` when more follows. The documents come first, then the tombstones, at most 500 entries in a page together. `docs` and `removed` are always there, `[]` when empty.
- **The next page:** `list {revision, cursor}` with the cursor the last page gave, opaque, and the revision that page named. A cursor without a revision, or one the server never gave, is `protocol`, and the channel closes.
- **Stale:** a `list` that names a `revision` (`null` included) is answered only at that revision. If the set has moved on, the answer is `stale {revision}` with the revision now, and the client lists again from the first page. So the pages of one listing are always of one revision.
- **`head`** answers with the revision alone: a client that remembers the revision it last synced at needs nothing more when nothing changed.

### Reading

- **`get {path, id, type, etag}`** reads a document only if it still has the etag a listing named. If it does not, or the document is not there, the answer is `stale {revision}`: the set moved on, and the client lists again. So several `get`s made from one listing read a consistent set: any change between them shows as `stale`.
- **Without `etag`** (absent or `null`), `get` reads the document as it is; one that is not there is `stale` too.
- **The answer** is `doc {path, id, type, etag, bytes, offset}` and the body: the document's bytes past `offset`.
- **Resuming a read:** `get` with `offset` (past 0, at most the document's size) sends the bytes past it. It needs `etag`, the etag of the body it resumes: one past 0 without it, or one past the document's size, is `protocol`, and the channel closes. A body that changed meanwhile is `stale`, and the client reads it from the start.
- **A read is consistent:** the server opens the document when it answers, and sends what it held then, even if a commit replaces it while the body goes out.

### Transactions

Every write is a transaction: `begin`, then `put`s and `delete`s, then `commit`, which applies all of it or nothing.

- **`begin {tx}`** opens the transaction `tx`, or returns to it. The answer `began {tx, staged}` lists what it holds, for resuming: `staged: [{path, id, type, delete?, ifEtag, bytes, received}]` (`delete: true` for a removal), `[]` for a new transaction.
  - A device has at most one open transaction per app: `begin` with another id discards the one it had.
  - An id that another device's open transaction holds is `noTransaction`: choose another (ids are random enough never to meet).
  - A transaction lives 24 hours after its last activity, and survives a restart of the server.
- **`put {tx, path, id, type, ifEtag, bytes, offset}`** and the body stage a document of `bytes` bytes. The answer, once the whole body has arrived, is `staged {path, id, type, received}`: how many bytes of it the server holds.
  - **`ifEtag`** is the etag the document must have at the commit; `null` (or absent) means it must not exist. It is checked at the commit, not at the `put`.
  - **`offset` 0** starts the body afresh, replacing whatever the transaction staged for the key.
  - **Resuming a body:** `offset` past 0 sends the bytes past it, of a body of the same `bytes` that the transaction staged before, from at most what `began` reported as `received`. Anything else (another size, an offset past what arrived, a key the transaction does not hold as a body) is `protocol`, and the channel closes.
  - **`offset` equal to `bytes`** sends no body, and only changes `ifEtag` of a body that has fully arrived.
  - **Quota:** a body larger than the quota, or one with which the set after the transaction would exceed the quota (and be larger than now), is refused with `quota`. A set already over its quota can always shrink.
  - **A refusal follows the body,** which the server reads and discards: `quota`, `badPath`, `noTransaction`, `badToken` (a device removed meanwhile), `unknownApp` (an app removed meanwhile) or `internal`. None closes the channel.
  - **A body cut off** (the session ended under it) stays staged with what arrived, written to disk; the next `begin` with the same id reports it, and a `put` resumes it.
- **`delete {tx, path, id, type, ifEtag}`** stages the removal of a document, which must have `ifEtag` at the commit, as for a `put`. The answer is `staged {…, received: 0}`.
- **`commit {tx}`** applies the transaction:
  - **`committed {revision, docs}`**: the set's new revision, and every document the transaction wrote, with its new etag, or deleted, with `etag: null`. The transaction is over.
  - **`conflict {revision, docs}`**: some documents are not at the etags the transaction expects. `docs` lists each of them with its current etag (`null` when it does not exist), and `revision` is the set's now. Nothing was applied, and **the transaction stays open**: the client reads again, merges, and stages again (`put` with `offset` equal to `bytes` only changes `ifEtag` of a body it still wants as it is), or aborts.
  - **`incomplete`**: a body has not fully arrived; the transaction stays open, and a `put` resumes it.
  - **`quota`**, **`badPath`** (a key's path is taken on disk by something that is no document, such as a folder or a file whose name differs in case): the transaction stays open.
  - **`noTransaction`**: there is no open transaction of that id for this device (never begun, discarded by another `begin`, aborted, expired, or committed already).
  - **`internal`** can come after the server's commit point, with the commit applied, possibly in part, and the transaction over. Never commit the same transaction again: list again, and decide from what the set holds. A `begin` with the same id tells whether the failure came before the commit point: the transaction is still open, with what it staged; otherwise the `begin` opens a new, empty one with that id.
- **`abort {tx}`** discards the transaction; `aborted {tx}` answers it even when the transaction was gone already.
- **The stored bytes** are exactly what the writer sent. They are never reparsed or reformatted.
- **Every write is conditional,** so a client that merges, writes with the etags it read and starts again on a conflict never loses another device's write.

### Pushes

- **`changed {revision}`** tells a session that the app's set has a new revision. It goes:
  - to every other session of the app after a `commit` (the committing session has the revision in `committed`);
  - to every session of the app when the set changes otherwise: a subordinate writing what it took from its main, files changed by hand taken in at a `hello`, a `get` or a `commit` (the committing session included when its answer is not `committed` or `conflict`, which name the revision), or a `commit` that failed after its commit point and was carried through later.

  It is never sent before that session's `welcome`: one that comes earlier is held, the newest kept, and sent right after the `welcome` unless the `welcome` names the same revision.
  - Pushes may come out of order: `welcome(r3)` can be followed by `changed(r2)` and then `changed(r3)`. The last frame always carries the newest revision; a `committed` or `conflict` counts among them, and is never followed by a `changed` of an older revision. Act on the latest revision, not on each push in turn; comparing it with the revision last synced tells whether anything is left to do.
- **`diverged`** and **`resolved`** are pushes too (see "Divergence").

### Divergence

A subordinate holds a copy of its main's apps (see "Servers"). When both servers changed the same document differently while they could not reach each other, or the two meet for the first time each holding documents that differ, the two copies of the app have **diverged**. The subordinate then keeps its own copy and stops syncing the app with its main, and every device of the app, on either server, is asked which copy to keep: the main's or the subordinate's, for the app's whole set. The servers merge nothing.

- **`diverged {servers: [{serverId, name, since}]}`** lists the divergences of the app that wait for a device's choice, `[]` once none does: `serverId` and `name` name the subordinate whose copy diverged, `since` is when (RFC 3339 UTC, with fractional seconds, to the nanosecond, when the server has them). `since` also names the divergence: a client compares it as the string it got, never parsed and formatted again. A main lists every subordinate of its own whose copy of the app diverged and that no device chose for yet; a subordinate lists itself. The list may be longer than one entry only on a main. Every `welcome` of the app carries the same list while it is not empty. A `diverged` follows every change; one may also follow a `welcome` that already named the same list.
- **`resolve {server, keep}`** is a device's choice for the divergence of `server`, a subordinate's server id from that list: `keep` is `"main"` (the subordinate's copy becomes the main's) or `"subordinate"` (the main's copy becomes the subordinate's), for the app's whole set, so documents only the other copy holds are deleted. Without a server id, or with another `keep`, it is `protocol`, and the channel closes.
- **`resolved {server}`** answers every `resolve`. The first choice wins: the other sessions of the app on that server get `resolved {server}` too, and a `diverged` without it. A later choice, or one for a server with no divergence waiting (another device chose, it is over, or there never was one), changes nothing and is answered `resolved` all the same, so the device closes its question either way.
- **Which choice is first** is the subordinate's to say. A choice made on the subordinate counts at once; one made on the main counts when it reaches the subordinate: at once while the two are linked, otherwise when they next are. If a device of the subordinate chose meanwhile, the main's choice is dropped; the devices that chose on the main were answered `resolved`, and see the outcome as the set they sync next. A choice made on the main is not shown to the subordinate's devices until it reaches the subordinate.
- **Name the servers when asking.** `"main"` and `"subordinate"` are roles, and roles move: a client asks "keep {the main's name}'s copy or {the subordinate's name}'s", with the subordinate's `name` from the entry and the main's from its own `welcome` (on a subordinate, the server it is paired with, or the main it last heard of). After a switch of the main a choice not yet carried out is dropped (the subordinate that became the main has no main to take it from), and `"main"` would name another server.
- **Afterwards** the subordinate makes the other copy equal to the one kept, and the app syncs again. The devices see the copy that was not kept change like any other write (`changed`), and merge what they hold with the set they find, as they always do; a change only that copy held stays only on the devices that still hold it. A choice made while the two servers are apart is carried out when they next meet, against both sets as they are then: what was written on the side not kept after the choice is replaced too.

### Pairing outcomes

- **Success:** the server checks the app id against its app list and checks the secret. It then creates the device entry, consumes the secret, and answers `paired` then `welcome`. The session continues authenticated.
- **Confirming:** the client confirms the pairing with its next frame on that session, any frame, after it has received `welcome`; normally it sends `head` at once.
  - If the session ends before the server hears one, the server deletes the device entry: it cannot know that the token arrived.
  - The secret stays used, so the user needs a new code.
  - A client that stored the token from a session that ended this way gets `badToken` on its next `hello`, and pairs again.
- **A subordinate:** a server linked to a main answers every `pair`, of either kind, with `error {code: 'notMain'}`, and the channel closes. It checks nothing first: no secret is consumed and no failed attempt is counted. Devices pair with the main, which hands its subordinates out to them.
- **Unknown app:** if the secret is valid but the server does not serve the app id, the answer is `error {code: 'unknownApp'}` and the channel stays open.
  - The secret is **not** consumed.
  - The server's page offers to add the app with one click. The device's next attempt then succeeds.
  - A client should report this to the user, not retry on its own.
- **Bad secret:** a wrong, expired or used secret gets `badSecret`, and the channel closes.
- **Blocked:** from a blocked IP, `pair` gets `blocked` (see Limits). The secret is not consumed, so the same code works after the block, while it is still valid.

### Errors

Codes:

- `protocol`
- `unknownApp`
- `badSecret`
- `badToken`
- `blocked`
- `badPath`
- `quota`
- `noTransaction`
- `incomplete`
- `notAuthenticated`
- `internal`
- `notMain`

`message` is English text for a developer. It is not for display, and it never contains server paths.

**A code the client does not know** should be treated as a temporary server error: report it and retry later. Do not treat it as a reason to drop the pairing. Codes may be added without a new `proto`.

- **Before authentication,** every frame but `hello` and `pair` (`head`, `list`, `get`, `begin`, `put`, `delete`, `commit`, `abort`, `resolve`, `forget`, `apps`, `diverged`, `announce`, `invite`, `term`) gets `notAuthenticated`.
- **After authentication,** a second `hello`/`pair` is a `protocol` error.
- **A frame with an unknown `t`** gets `error {code: 'protocol'}` and the channel closes, before authentication or after.
- **`protocol` always closes the channel;** every other error answers the frame and leaves the session open, except where `hello` and `pair` say otherwise.
- **`hello` failures:**
  - a `hello` for an app removed from the server's list gets `unknownApp`;
  - a `hello` with a removed device or a wrong token gets `badToken`, meaning the pairing is gone;
  - a `hello` from a blocked IP gets `blocked`, and the pairing still stands (see Limits).
- **A device removed during a session:** its `begin`, `put`, `delete` and `commit` get `badToken`, and nothing is stored.
- **An app removed during a session:** its frames get `unknownApp`.
- **The documents' codes:** `badPath`, `quota`, `noTransaction` and `incomplete` (see "Transactions").
- **`internal`** means a server-side failure, such as a disk error. The details are in the server's log. After a `commit`, see "Transactions".

## Servers: roles, server-kind devices and link sessions

A workspace-server can be a **subordinate** of another, its **main**: it mirrors the main's apps on its own disk. It links to the main as a client of this protocol. Nothing here changes what an app's client does.

- **Only a main can be an upstream.** A server checks `role` in every `welcome` it gets as a server-kind device and refuses any other than `"main"`, a missing one included: when pairing, it closes the session without confirming, which undoes the pairing on the other side. So subordinates form a star of depth one around a main. Once linked, a `welcome` with `"subordinate"` means that the main stepped down to another main, and the server looks for it (see "Switching the main"); a `welcome` with no role stops the link.
- **Pairing as a server:** `pair {proto: 1, kind: "server", secret, name}`.
  - `kind` is `"device"` (the default when it is absent: the pairing described above) or `"server"`. Any other value is `protocol`, and the channel closes.
  - A server-kind pair names no app; an `app` it carries is ignored. The outcomes are those of a device's pair, without `unknownApp`.
  - The new device is admitted to every app the server lists, now and later, and to link sessions.
  - The session it pairs on is a link session: `paired` is followed by a `welcome` with `revision: null`. The pairing is confirmed by the next frame, as for a device; the subordinate sends `apps`.
- **Link sessions:** `hello {proto, device, token}` without `app`, from a server-kind device, opens one.
  - Its `welcome` has `revision: null`.
  - It has no documents: `head`, `list`, `get`, `begin`, `put`, `delete`, `commit`, `abort` and `resolve` there are `protocol`, and the channel closes.
  - `forget` deletes the server-kind device and closes every session of it.
  - A `hello` without `app` from a device of an app gets `unknownApp`, as it always did; from an unknown device or with a wrong token, `badToken`.
- **App sessions:** a server-kind device says `hello {proto, app, device, token}` for any app the server lists, and the session works as a device's.
- **The app list:** `apps` asks for the server's apps; the answer is `apps {apps: [{id, name, quota}, …]}`, sorted by id, `[]` when there is none. `quota` is the app's quota in bytes (see "Limits"): a subordinate takes it as its own, so the two servers refuse the same sets. A client reads a missing `quota` as none given.
  - Only sessions of a server-kind device may ask. Any other session gets `protocol`, and the channel closes; before authentication the answer is `notAuthenticated`.
  - Every link session also gets `apps {apps}` unasked whenever the list changes, never before its `welcome`. The answer and the push look alike: each carries the whole current list, so act on the latest.
- **Divergence between servers:** a subordinate tells its main the state of its copy of each app over its link session, and the main hands a choice made there back the same way. The devices' side is "Divergence" above.
  - **`diverged {app, since}`** from the subordinate: `since` (RFC 3339 UTC, with fractional seconds to the nanosecond, so that two divergences that begin within one second read apart) is when its copy of `app` diverged from the main's and began to wait for a device's choice; `null` when none waits (it never diverged, the divergence is over, or a device chose, on either server). Nothing answers it. The subordinate sends it for every app the main lists at every link session, and again whenever that changes.
  - **The main** lists every subordinate whose copy waits to its own sessions of the app, and keeps it, with a choice its devices make, across a restart, until the subordinate tells `null`. A `since` other than the one it keeps is a new divergence, for which a choice made before does not count. An app the main does not list, and a server that never announced itself, are ignored.
  - Only a link session may send it; on any other session, without `app`, or with a `since` that is neither `null` nor RFC 3339, the answer is `protocol`, and the channel closes. Before authentication it is `notAuthenticated`.
  - **`resolve {app, keep, since}`**, pushed by the main to the subordinate's link session: a device of the main chose `keep` (`"main"` or `"subordinate"`) for the divergence of `app` that began at `since`. The main pushes it once the choice is made, and again whenever the subordinate tells `diverged` with that `since`, which it compares, and sends back, as the string it got. The subordinate takes it when no choice was made for that divergence yet, and ignores it otherwise; either way it then tells `diverged {app, since: null}`.
- **Removals:** removing a server-kind device closes every session of it. A server that becomes a subordinate itself closes the sessions of its own server-kind devices; they reconnect, read the new `role`, and look for the main of their family among its servers (see "Switching the main").
- **Announcing:** `announce {server: {serverId, name, password, fingerprint, port, hosts}}` on a link session tells the main how devices reach the subordinate: one entry of `subordinates`, the same rules. The main keeps it with the server-kind device and lists it in every `welcome` from then on.
  - A subordinate that was a main and stepped down to this one (see "Switching the main") hands its own registry over in its first `announce`: `devices` and `removed`, in the form of the `devices` frame below. The main merges it into its own (the union, less every device removed on either side) and answers with the result. Only a main takes one; a subordinate answers `protocol`.
  - The answer is the main's device registry, `devices {devices}` (below).
  - The subordinate announces at every link session and again, on the same session, when its name or UDP port changes; an address change ends the link session, and the next one announces.
  - Only a link session may announce. On any other session, with no `server`, with an entry that breaks the rules, or with the main's own `serverId`, the answer is `protocol`, and the channel closes.
  - A main sends `subordinates` in every `welcome`, `[]` included, so a subordinate announces only to a main whose link-session `welcome` carries the field; a main from before announce has none, and would close the session over the unknown frame.
- **The device registry:** `devices {devices: [{id, app, kind?, name?, tokenHash, pairedAt?, server?, machine?, machineName?, serverName?}, …], removed?: [{id, at}, …]}`: the main's devices, sorted by app and id, `[]` when there is none. `tokenHash` is the lowercase hex SHA-256 of the device's token: the main never sends a token.
  - Server-kind devices are in it too, first: `app: ""`, `kind: "server"`, and `server`, what each announced, once it has. They let the servers of a family reach one another (below) and let a subordinate that becomes the main admit every server of the family. A subordinate from before reads such an entry as not valid and leaves it out.
  - `name` and `pairedAt` (RFC 3339 UTC) label each device for a server that adopts the registry.
  - `machine` and `machineName` say which of the user's physical devices (a phone, a laptop) a device of an app is paired on, and what the main calls it: two apps on one phone share a `machine`. The user says so on the main's page; nothing on the wire tells it. A server-kind entry has neither, and an entry of a server with one, a `machine` that is not 1 to 64 of `[A-Za-z0-9_-]`, or a `machineName` over 64 characters, is not valid. A server that adopts the registry keeps each device's machine, under the name it gives that machine already, or else `machineName`; a subordinate from before ignores both.
  - `serverName` is the name the user gave a server-kind entry's server on the main's page, left out while it goes by the one it announced (`server.name`). An entry of an app with one, or one over 64 characters, is not valid. A main lists the server under it in `subordinates`; a server that adopts the registry keeps it; a subordinate from before ignores it.
  - `removed` lists the devices removed in the family, with when (RFC 3339 UTC), the newest 1,000; it is left out when empty. A device id is never reused, so a removal is final: a registry merged later never brings it back.
  - It answers `announce`, and every link session that announced gets it again unasked after every change: a pairing, a removal, a pairing undone, an app removed. Each carries the whole registry, so act on the latest. A link session that never announced gets none.
  - A subordinate admits `hello` from these devices, each to its own app, by hashing the token it presents and comparing. It keeps the latest registry on its disk, so it admits them after a restart too, while the main is out of reach.
  - A server-kind device of the registry (another server of the family) is admitted by a subordinate to a link session only, where it may send `term` and nothing else: `apps`, `announce` and `invite` there are `protocol`, and a `hello` for an app is `badToken`.
  - A device removed on the main is refused by a subordinate from the moment the registry reaches it: at once while linked, otherwise at its next link. Until then the subordinate still admits it.
  - A subordinate's own devices (paired before it was linked) stay admitted, and win an id the registry shares.
  - `forget` from a device of the registry on a subordinate closes its sessions there, and does not reach the main: the main keeps the device until it is removed there.
  - A subordinate the main removed (its link gets `badToken`) forgets the registry: the main's devices are no longer admitted there.
- **A pairing code for a browser on the subordinate's computer:** `invite` on a link session asks the main for a pairing code of its own, the one its page's **Pair a device** issues, so that a browser on the subordinate's computer can pair with the main without a phone. The answer is `invited {code, expiresAt}`: `code` is an ordinary `wsrv1.` pairing code of the main, `expiresAt` the time its secret expires, RFC 3339 in UTC.
  - The code names no app, as no pairing code does: the device's `pair` names its own. It is a new secret, so it revokes the code the main's page showed, as any new code does, and is good for one pairing within 10 minutes.
  - `invite {machine}` asks for a code whose device joins that machine of the main's registry (`machine` above): another app on a device the user has. The main keeps it with the secret, never in the code; a `machine` it no longer holds when the device pairs gives the device a new machine, as no `machine` does. A main from before ignores the field.
  - The device pairs with the main, not with the subordinate, and learns the subordinate from the main's `welcome` (`subordinates`); the subordinate admits it once the main's device registry reaches it.
  - Only a link session may ask. On a device's session or an app session, the answer is `protocol`, and the channel closes. A subordinate, whose codes would be refused, answers `notMain`, and the session stays open. A server-side failure is `internal`; the session stays open.
  - A main from before `invite` answers `protocol` and closes the link session, which the subordinate opens again.
- **`proto` stays 1.** Nothing an old client meets changed: `role`, `term` and `subordinates` are more `welcome` fields, which clients ignore (see the vectors' tolerated frames); `notMain` is a new code, which an old client reads as `internal` (a temporary error); `kind`, a `hello` without `app`, `apps`, `diverged`, `announce`, `invite` and `term` come only from servers, `apps` frames go only to server-kind sessions, `devices` frames only to link sessions that announced, `invited` and `term` frames only answer their requests, and `takeOver` and `resolve` go only to a subordinate's link session.

## Switching the main

A **family** is a main and the subordinates linked to it. The main is the server whose copy wins a divergence, the one devices prefer, and the source of the device registry. A subordinate can take that role over: planned, while the main is reachable, or forced, while it is not.

- **The term:** a whole number, 0 for a family that never switched its main. It goes up by one every time a subordinate becomes the main. Every `welcome` names it (`term`): a main its own, a subordinate its main's.
- **The higher main wins:** of two mains, the higher `(term, serverId)` wins, the server id compared as a string when the terms are equal. A server that meets a main higher than its own main follows it: a subordinate links to it, and a main steps down and links to it. A main never steps down without a successor.
- **Reaching one another:** every server of a family holds the registry, server-kind devices included, so each admits the others to a link session. A subordinate says `hello` with its own server-kind device and token on its main; a subordinate that became the main keeps the device and token it had, and the others still admit it.
- **`term {term, server, device?, token?}`** on a link session: the sender says it is the main of the family at `term`, reached at `server` (one entry of `subordinates`, the same rules). `device` and `token`, both or neither, are a server-kind credential for the receiver on the sender: a new main mints one for the main it replaces, which has none there.
  - The receiver follows the sender when `(term, server.serverId)` is higher than its own main's: a subordinate links to the sender with its own credential; a main steps down and links to the sender with the credential given, or with the one it kept from its own time as a subordinate, closes the sessions of its own subordinates (but the sender's), and hands its own registry over at its first `announce` there.
  - The answer is `term {term, main}`: the term and the `serverId` of the main the receiver follows now. `main` is the sender's id when it follows the sender.
  - Only a link session may send it; on any other session, without a `term` of 0 or more or a valid `server`, with the receiver's own id, or when the receiver cannot act on it (a main with no credential for the sender), the answer is `protocol`, and the channel closes. Before authentication it is `notAuthenticated`.
- **`takeOver`:** a main sends it, unasked, on the link session of the subordinate the user hands the role to. The subordinate then switches as planned (below). There is no answer: the switch shows when the subordinate tells the main `term`.
- **A planned switch:** the subordinate makes sure the documents are the same on both servers (it refuses while a divergence of the family waits, as far as it knows: its own, or another subordinate's that the main listed to it in `welcome` or `diverged`; a main asked to hand its role over refuses while it keeps any), takes the term up by one, makes the main's registry its own, mints a server-kind credential for the main, and tells it `term`. The main steps down and links to it. The main's other subordinates find their main answering as a subordinate, and look for the new main: they say `hello` to every server of the family their registry lists, and follow the highest that answers as a main above their old one. The new main also tells them `term` when it reaches them.
- **A forced switch:** the subordinate does the same without the main and without the documents' check, and drops a divergence of its own, which was from the main it no longer follows. If the main still runs, both act as main until they meet; the new main tells the old one `term` whenever it reaches it, and the old main steps down by itself. Its first link as a subordinate compares the two copies of each app afresh: copies that differ have diverged, and the devices are asked (see "Divergence"). Two subordinates made the main apart, at the same term, settle by the server id the same way.
- **Devices** keep their pairing: every server of the family admits the same device id and token. A device paired with the old main finds it answering as a subordinate, or not at all, and the new main answering as a main; the reference client then makes the new main the server it is paired with (see "Subordinates: fallback servers for a device").

## Subordinates: fallback servers for a device

A device pairs only with a main. The main's `welcome` lists its subordinates (`subordinates`, above), and a device may keep them and sync with one of them when the main is out of reach: a laptop's server travelling with a phone. Nothing about the documents changes: the subordinate holds the main's set, and the device merges with whichever server it reached, as it always does.

- **The same pairing:** the device says `hello` to a subordinate with the device id and token it got from the main. A device paired after the subordinate's last link is not known there yet and gets `badToken`, which on a subordinate does not mean the pairing is gone.
- **Which server:** the reference client connects to the main and its subordinates at once, takes the main if it answers within a short grace period, and otherwise a subordinate that did. It takes a `welcome` only from a `serverId` it knows; a `welcome` from another one is `serverChanged`, which stops sync when it comes from the main's own address. Only the main's refusals stop sync.
- **The revision and the etags** are hashes of the bytes on every server, so the revision and the etags a device last synced at are valid on all of them: a subordinate that holds the same set is not read again.
- **A divergence** is asked on every server of the family: a device on the subordinate sees the subordinate's own (`diverged`, "Divergence"), a device on the main every subordinate's, and a choice made on either counts.
- **Keep the list current:** each `welcome` from the main replaces the list; one without `subordinates` (from a subordinate, or an older server) leaves it as it was.
- **Follow the main:** after a switch of the main, a fallback answers as the main and the paired server as a subordinate, or not at all. The reference client then takes the fallback, when its `term` is at least the one the paired server last named as the main, as the server it is paired with; the server it paired with before becomes a fallback. Of two servers answering as mains (a family split for a while), it takes the higher `term`, unless the paired server answers as a main first.
- **Pairing and forgetting** go to the main: a subordinate answers `pair` with `notMain`, and a `forget` sent to a subordinate does not reach the main. A subordinate's page shows no code of its own; for a browser on its computer it shows a code of the main, which it gets with `invite`.

## What the client cannot see

- The server merges nothing and knows nothing about the documents' contents or formats. Merging is the client's job, using `stale`, `conflict` and the etags. A divergence of two servers is settled by a device's choice of one copy, whole, not by a merge.
- Changing the server's UDP port invalidates every pairing, because the device stored `o`. The server's page warns the user, and every device pairs again.
- Losing the server's `identity.json` invalidates every pairing: the certificate, `icePwd` and `serverId` change.
