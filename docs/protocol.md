# Protocol

This is the contract between `workspace-server` and an app's client. The reference client is the npm package in [client/](../client/README.md), `@flighter7/workspace-client`; the todo app (`FLighter7/todo`) syncs through it.

- **Changing the contract:** change it only together with the client package and the vectors in [testdata/conformance/](../testdata/conformance), in the same commit. The vectors hold this contract as data, and both the Go tests and the package's tests play them. Bump `proto` for any change an old client cannot handle.
- **Reading order:** [architecture.md](architecture.md) explains how the server implements this contract and why. This file says only what a client must know.

## Overview

- **The connection:** a device talks to the server over one WebRTC data channel.
- **No signaling:**
  - after a one-time pairing, the device builds the server's side of the connection itself, from data in the pairing code;
  - no message is exchanged before the first STUN packet.
- **Authentication:** DTLS is authenticated by the server certificate's fingerprint, pinned in the pairing code. After that the device sends `hello` with its token, or `pair` with a one-time secret.
- **Scope:** IPv4 and the local network only.

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
  "o": 47800,
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

| Client → server                     | Server → client                                                                      |
| ----------------------------------- | ------------------------------------------------------------------------------------ |
| `hello {proto, app, device, token}` | `welcome {proto, serverId, name, etag, hosts}`, or `error` and the channel closes    |
| `pair {proto, app, secret, name}`   | `paired {device, token}` then `welcome {…}`, or `error` (closes unless `unknownApp`) |
| `head`                              | `head {etag}`                                                                        |
| `get`                               | `doc {etag, bytes}` + body (`etag: null`, `bytes: 0` when there is none)             |
| `put {ifEtag, bytes}` + body        | `stored {etag}` or `conflict {etag}`                                                 |
| `forget`                            | the device entry is deleted; the server closes every session of the device           |
|                                     | `changed {etag}`: to every other session of the same app after a stored `put`        |
|                                     | `error {code, message}`                                                              |

### Values

- **`etag`** is a string, or `null` when there is no document. It is the lowercase hex SHA-256 of the document's bytes, so two identical writes agree.
- **`hosts`** is `[mdnsName, ...LAN IPv4]`, the server's current list. A client can refresh the hosts it stored at pairing from it, since addresses change.
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
- **`changed`** goes to every other session of the app after a stored `put`. It is never sent before that session's `welcome`.
  - A `changed` may carry an etag older than one the session already received; for example, `welcome(e3)` can be followed by `changed(e2)` and then `changed(e3)`.
  - The last frame always carries the newest etag. Act on the latest etag, not on each push in turn.

### Pairing outcomes

- **Success:** the server checks the app id against its app list and checks the secret. It then creates the device entry, consumes the secret, and answers `paired` then `welcome`. The session continues authenticated.
- **Confirming:** the client confirms the pairing with its next frame on that session, any frame, after it has received `welcome`; normally it sends `head` or `get` at once.
  - If the session ends before the server hears one, the server deletes the device entry: it cannot know that the token arrived.
  - The secret stays used, so the user needs a new code.
  - A client that stored the token from a session that ended this way gets `badToken` on its next `hello`, and pairs again.
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

`message` is English text for a developer. It is not for display, and it never contains server paths.

**A code the client does not know** should be treated as a temporary server error: report it and retry later. Do not treat it as a reason to drop the pairing. Codes may be added without a new `proto`.

- **Before authentication,** `head`, `get`, `put` and `forget` get `notAuthenticated`.
- **After authentication,** a second `hello`/`pair` is a `protocol` error.
- **A frame with an unknown `t`** gets `error {code: 'protocol'}` and the channel closes, before authentication or after.
- **`hello` failures:**
  - a `hello` for an app removed from the server's list gets `unknownApp`;
  - a `hello` with a removed device or a wrong token gets `badToken`, meaning the pairing is gone;
  - a `hello` from a blocked IP gets `blocked`, and the pairing still stands (see Limits).
- **An in-flight `put` from a device the user removed** gets `badToken` and is not stored.
- **`internal`** means a server-side failure, such as a disk error. The details are in the server's log.

## What the client cannot see

- The server merges nothing and knows nothing about the document's contents. Merging is the client's job, using `conflict` and the etag.
- Changing the server's UDP port invalidates every pairing, because the device stored `o`. The server's page warns the user, and every device pairs again.
- Losing the server's `identity.json` invalidates every pairing: the certificate, `icePwd` and `serverId` change.
