# Protocol

This is the contract between `workspace-server` and an app's client. The reference client is the npm package in [client/](../client/README.md), `@flighter7/wss-client`; the todo app (`FLighter7/todo`) syncs through it.

- **Changing the contract:** change it only together with the client package and the vectors in [testdata/conformance/](../testdata/conformance), in the same commit. The vectors hold this contract as data, and both the Go tests and the package's tests play them. Bump `proto` for any change an old client cannot handle.
- **Reading order:** [architecture.md](architecture.md) explains how the server implements this contract and why. This file says only what a client must know.

## Overview

- **The connection:** a device talks to the server over one WebRTC data channel.
- **No signaling:**
  - after a one-time pairing, the device builds the server's side of the connection itself, from data in the pairing code;
  - no message is exchanged before the first STUN packet.
- **Authentication:** DTLS is authenticated by the server certificate's fingerprint, pinned in the pairing code. After that the device sends `hello` with its token, or `pair` with a one-time secret.
- **Scope:** IPv4 and the local network only.
- **Servers as clients:** a workspace-server can link to another as its subordinate. It speaks this same protocol as a device of kind `server`; see "Servers: roles, server-kind devices and link sessions". An app's client never needs that part.
- **Fallback servers:** a main lists its subordinates in every `welcome`, and a device may sync with any of them, with the same device id and token, when the main is out of reach; see "Subordinates: fallback servers for a device".
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
- **Unread replies:** a session whose replies and pushes waiting to be sent exceed the app's `maxBytes` plus 1 MiB (1 MiB before authentication) is closed, without an error frame. A client that reads what it asks for never comes near it; one that repeats `get` without reading the `doc`s does.
- **Failed attempts:** an IP is blocked at its 10th failed `hello`/`pair` within a minute, for 10 minutes from that failure.
  - While it is blocked, every `hello` and `pair` from it gets `error {code: 'blocked'}` and the channel closes.
  - Nothing is checked and no pairing secret is consumed.
  - `blocked` is temporary, and the pairing still stands. A client keeps its pairing, tells the user to wait a few minutes, and retries later.
  - Attempts made during the block do not extend it.
- **Document size:** each app has a `maxBytes` limit, 64 MiB by default (see `tooLarge` below).

## Framing

- **Text frames:** JSON objects with a `t` field.
- **Bodies:** a frame that announces a body carries `bytes`. That is `put` from the client and `doc` from the server.
  - Exactly that many bytes follow, as binary messages of at most 16 KiB each.
  - One body is in flight per direction at a time.
  - A binary message that arrives when no body is expected, or that overruns the announced size, is a protocol error.
- **Order:** the server never interleaves a text frame inside a body it is sending. Frames requested meanwhile go after the body.
- **Flow control:** a sender pauses while more than 1 MiB waits in the channel's buffer, and resumes when the buffer drains to 256 KiB.

## Messages

The first client frame, `hello` or `pair`, carries `proto: 1`. Any other number gets `error {code: 'protocol'}` and the channel closes.

| Client → server                                         | Server → client                                                                                             |
| ------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| `hello {proto, app, device, token}`                     | `welcome {proto, role, term, serverId, name, etag, hosts, subordinates}`, or `error` and the channel closes |
| `pair {proto, app, secret, name}`                       | `paired {device, token}` then `welcome {…}`, or `error` (closes unless `unknownApp`)                        |
| `head`                                                  | `head {etag}`                                                                                               |
| `get`                                                   | `doc {etag, bytes}` + body (`etag: null`, `bytes: 0` when there is none)                                    |
| `put {ifEtag, bytes}` + body                            | `stored {etag}` or `conflict {etag}`                                                                        |
| `forget`                                                | the device entry is deleted; the server closes every session of the device                                  |
| `apps` (server-kind devices only)                       | `apps {apps}`                                                                                               |
| `announce {server, devices?, removed?}` (link sessions) | `devices {devices, removed}`                                                                                |
| `invite` (link sessions)                                | `invited {code, expiresAt}`                                                                                 |
| `term {term, server, device?, token?}` (link sessions)  | `term {term, main}`                                                                                         |
|                                                         | `changed {etag}`: to every other session of the same app after a stored write                               |
|                                                         | `apps {apps}`: to every link session after the server's app list changes                                    |
|                                                         | `devices {devices, removed}`: to every link session that announced, after the device registry changes       |
|                                                         | `takeOver`: to a subordinate's link session, when its main hands it the role of main                        |
|                                                         | `error {code, message}`                                                                                     |

`pair` may also carry `kind`, and a server-kind device's `hello` may leave `app` out; both, `apps`, `announce`, `invite`, `term` and `takeOver`, are for servers only (see "Servers: roles, server-kind devices and link sessions" and "Switching the main").

### Values

- **`etag`** is a string, or `null` when there is no document. It is the lowercase hex SHA-256 of the document's bytes, so two identical writes agree.
- **`hosts`** is `[mdnsName, ...LAN IPv4]`, the server's current list. A client can refresh the hosts it stored at pairing from it, since addresses change.
- **`role`** is `"main"` or `"subordinate"`: whether the server is linked to an upstream server (see "Servers"). A server from before roles sends none. An app's client may ignore it.
- **`term`** is the family's term, a whole number: it goes up by one every time a subordinate becomes the main (see "Switching the main"). A main names its own, a subordinate its main's. It is left out at 0, as it is by a server whose family never switched its main and by a server from before terms; absent reads as 0. An app's client may ignore it; the reference client uses it to follow the main.
- **`subordinates`** is sent by a main only, always, `[]` when it has none: the servers linked to it that serve its devices too, sorted by name. A subordinate, and a server from before subordinates, sends none. Each entry is a pairing code without the secret, `{serverId, name, password, fingerprint, port, hosts}`, with the fingerprint in uppercase colon hex (`AB:CD:…`, 32 bytes) as the answer's `a=fingerprint` line takes it. A client leaves out an entry it cannot build a connection to (see the vectors' tolerated frames). See "Subordinates: fallback servers for a device".
- **`device`** is the device id, 16 characters.
- **`token`** is 32 random bytes, base64url.
- **`name`** in `pair` is the device's display name, shown on the server's page.

### Documents

- **`put` is always conditional.**
  - `ifEtag` is the etag the client read.
  - `null` means "I believe there is no document yet".
  - A mismatch is answered with `conflict` and the current etag, which may be `null`.
- **The stored bytes** are exactly what the last writer sent. They are never reparsed or reformatted.
- **`tooLarge`:** a `put` announcing more than the app's `maxBytes` gets `error {code: 'tooLarge'}`.
  - The server checks the size on the header. It then reads and discards exactly the announced bytes, and answers.
  - Every answer to a `put` follows its body.
- **`notJson`:** a body that is not valid JSON gets `error {code: 'notJson'}`.
- **Neither error closes the channel.**
- **`changed`** goes to every other session of the app after a stored `put`, and to every session of the app when a subordinate stores what it pulled from its main. It is never sent before that session's `welcome`.
  - A `changed` may carry an etag older than one the session already received; for example, `welcome(e3)` can be followed by `changed(e2)` and then `changed(e3)`.
  - The last frame always carries the newest etag. Act on the latest etag, not on each push in turn.

### Pairing outcomes

- **Success:** the server checks the app id against its app list and checks the secret. It then creates the device entry, consumes the secret, and answers `paired` then `welcome`. The session continues authenticated.
- **Confirming:** the client confirms the pairing with its next frame on that session, any frame, after it has received `welcome`; normally it sends `head` or `get` at once.
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
- `tooLarge`
- `notJson`
- `notAuthenticated`
- `internal`
- `notMain`

`message` is English text for a developer. It is not for display, and it never contains server paths.

**A code the client does not know** should be treated as a temporary server error: report it and retry later. Do not treat it as a reason to drop the pairing. Codes may be added without a new `proto`.

- **Before authentication,** `head`, `get`, `put`, `forget`, `apps`, `announce`, `invite` and `term` get `notAuthenticated`.
- **After authentication,** a second `hello`/`pair` is a `protocol` error.
- **A frame with an unknown `t`** gets `error {code: 'protocol'}` and the channel closes, before authentication or after.
- **`hello` failures:**
  - a `hello` for an app removed from the server's list gets `unknownApp`;
  - a `hello` with a removed device or a wrong token gets `badToken`, meaning the pairing is gone;
  - a `hello` from a blocked IP gets `blocked`, and the pairing still stands (see Limits).
- **An in-flight `put` from a device the user removed** gets `badToken` and is not stored.
- **`internal`** means a server-side failure, such as a disk error. The details are in the server's log.

## Servers: roles, server-kind devices and link sessions

A workspace-server can be a **subordinate** of another, its **main**: it mirrors the main's apps on its own disk. It links to the main as a client of this protocol. Nothing here changes what an app's client does.

- **Only a main can be an upstream.** A server checks `role` in every `welcome` it gets as a server-kind device and refuses any other than `"main"`, a missing one included: when pairing, it closes the session without confirming, which undoes the pairing on the other side. So subordinates form a star of depth one around a main. Once linked, a `welcome` with `"subordinate"` means that the main stepped down to another main, and the server looks for it (see "Switching the main"); a `welcome` with no role stops the link.
- **Pairing as a server:** `pair {proto: 1, kind: "server", secret, name}`.
  - `kind` is `"device"` (the default when it is absent: the pairing described above) or `"server"`. Any other value is `protocol`, and the channel closes.
  - A server-kind pair names no app; an `app` it carries is ignored. The outcomes are those of a device's pair, without `unknownApp`.
  - The new device is admitted to every app the server lists, now and later, and to link sessions.
  - The session it pairs on is a link session: `paired` is followed by a `welcome` with `etag: null`. The pairing is confirmed by the next frame, as for a device; the subordinate sends `apps`.
- **Link sessions:** `hello {proto, device, token}` without `app`, from a server-kind device, opens one.
  - Its `welcome` has `etag: null`.
  - It has no document: `head`, `get` and `put` there are `protocol`, and the channel closes.
  - `forget` deletes the server-kind device and closes every session of it.
  - A `hello` without `app` from a device of an app gets `unknownApp`, as it always did; from an unknown device or with a wrong token, `badToken`.
- **App sessions:** a server-kind device says `hello {proto, app, device, token}` for any app the server lists, and the session works as a device's.
- **The app list:** `apps` asks for the server's apps; the answer is `apps {apps: [{id, name}, …]}`, sorted by id, `[]` when there is none.
  - Only sessions of a server-kind device may ask. Any other session gets `protocol`, and the channel closes; before authentication the answer is `notAuthenticated`.
  - Every link session also gets `apps {apps}` unasked whenever the list changes, never before its `welcome`. The answer and the push look alike: each carries the whole current list, so act on the latest.
- **Removals:** removing a server-kind device closes every session of it. A server that becomes a subordinate itself closes the sessions of its own server-kind devices; they reconnect, read the new `role`, and look for the main of their family among its servers (see "Switching the main").
- **Announcing:** `announce {server: {serverId, name, password, fingerprint, port, hosts}}` on a link session tells the main how devices reach the subordinate: one entry of `subordinates`, the same rules. The main keeps it with the server-kind device and lists it in every `welcome` from then on.
  - A subordinate that was a main and stepped down to this one (see "Switching the main") hands its own registry over in its first `announce`: `devices` and `removed`, in the form of the `devices` frame below. The main merges it into its own (the union, less every device removed on either side) and answers with the result. Only a main takes one; a subordinate answers `protocol`.
  - The answer is the main's device registry, `devices {devices}` (below).
  - The subordinate announces at every link session and again, on the same session, when its name or UDP port changes; an address change ends the link session, and the next one announces.
  - Only a link session may announce. On any other session, with no `server`, with an entry that breaks the rules, or with the main's own `serverId`, the answer is `protocol`, and the channel closes.
  - A main sends `subordinates` in every `welcome`, `[]` included, so a subordinate announces only to a main whose link-session `welcome` carries the field; a main from before announce has none, and would close the session over the unknown frame.
- **The device registry:** `devices {devices: [{id, app, kind?, name?, tokenHash, pairedAt?, server?}, …], removed?: [{id, at}, …]}`: the main's devices, sorted by app and id, `[]` when there is none. `tokenHash` is the lowercase hex SHA-256 of the device's token: the main never sends a token.
  - Server-kind devices are in it too, first: `app: ""`, `kind: "server"`, and `server`, what each announced, once it has. They let the servers of a family reach one another (below) and let a subordinate that becomes the main admit every server of the family. A subordinate from before reads such an entry as not valid and leaves it out.
  - `name` and `pairedAt` (RFC 3339 UTC) label each device for a server that adopts the registry.
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
  - The device pairs with the main, not with the subordinate, and learns the subordinate from the main's `welcome` (`subordinates`); the subordinate admits it once the main's device registry reaches it.
  - Only a link session may ask. On a device's session or an app session, the answer is `protocol`, and the channel closes. A subordinate, whose codes would be refused, answers `notMain`, and the session stays open. A server-side failure is `internal`; the session stays open.
  - A main from before `invite` answers `protocol` and closes the link session, which the subordinate opens again.
- **`proto` stays 1.** Nothing an old client meets changed: `role`, `term` and `subordinates` are more `welcome` fields, which clients ignore (see the vectors' tolerated frames); `notMain` is a new code, which an old client reads as `internal` (a temporary error); `kind`, a `hello` without `app`, `apps`, `announce`, `invite` and `term` come only from servers, `apps` frames go only to server-kind sessions, `devices` frames only to link sessions that announced, `invited` and `term` frames only answer their requests, and `takeOver` goes only to a subordinate's link session.

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
- **A planned switch:** the subordinate makes sure the documents are the same on both servers, takes the term up by one, makes the main's registry its own, mints a server-kind credential for the main, and tells it `term`. The main steps down and links to it. The main's other subordinates find their main answering as a subordinate, and look for the new main: they say `hello` to every server of the family their registry lists, and follow the highest that answers as a main above their old one. The new main also tells them `term` when it reaches them.
- **A forced switch:** the subordinate does the same without the main and without the documents' check. If the main still runs, both act as main until they meet; the new main tells the old one `term` whenever it reaches it, and the old main steps down by itself. Two subordinates made the main apart, at the same term, settle by the server id the same way.
- **Devices** keep their pairing: every server of the family admits the same device id and token. A device paired with the old main finds it answering as a subordinate, or not at all, and the new main answering as a main; the reference client then makes the new main the server it is paired with (see "Subordinates: fallback servers for a device").

## Subordinates: fallback servers for a device

A device pairs only with a main. The main's `welcome` lists its subordinates (`subordinates`, above), and a device may keep them and sync with one of them when the main is out of reach: a laptop's server travelling with a phone. Nothing about the documents changes: the subordinate holds the main's documents, and the device merges with whichever server it reached, as it always does.

- **The same pairing:** the device says `hello` to a subordinate with the device id and token it got from the main. A device paired after the subordinate's last link is not known there yet and gets `badToken`, which on a subordinate does not mean the pairing is gone.
- **Which server:** the reference client connects to the main and its subordinates at once, takes the main if it answers within a short grace period, and otherwise a subordinate that did. It takes a `welcome` only from a `serverId` it knows; a `welcome` from another one is `serverChanged`, which stops sync when it comes from the main's own address. Only the main's refusals stop sync.
- **The etag** is the SHA-256 of the bytes on every server, so the etag a device last synced is valid on all of them: a subordinate that holds the same document is not read again.
- **Keep the list current:** each `welcome` from the main replaces the list; one without `subordinates` (from a subordinate, or an older server) leaves it as it was.
- **Follow the main:** after a switch of the main, a fallback answers as the main and the paired server as a subordinate, or not at all. The reference client then takes the fallback, when its `term` is at least the one the paired server last named as the main, as the server it is paired with; the server it paired with before becomes a fallback. Of two servers answering as mains (a family split for a while), it takes the higher `term`, unless the paired server answers as a main first.
- **Pairing and forgetting** go to the main: a subordinate answers `pair` with `notMain`, and a `forget` sent to a subordinate does not reach the main. A subordinate's page shows no code of its own; for a browser on its computer it shows a code of the main, which it gets with `invite`.

## What the client cannot see

- The server merges nothing and knows nothing about the document's contents. Merging is the client's job, using `conflict` and the etag.
- Changing the server's UDP port invalidates every pairing, because the device stored `o`. The server's page warns the user, and every device pairs again.
- Losing the server's `identity.json` invalidates every pairing: the certificate, `icePwd` and `serverId` change.
