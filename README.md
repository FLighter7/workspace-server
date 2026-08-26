# workspace-server

A thin local hub. It keeps versioned JSON documents in a folder on this machine and hands them to whichever apps on the local network ask for them. There is no database, no schema, and no knowledge of what any document means — an app stores what it likes and merges it itself.

It exists so an app's data survives things the browser does not promise to: clearing site data, storage eviction, and Safari deleting everything after seven days without a visit.

## What it is not

It is not a source of truth. Apps that use it keep their own local copy and treat the server as one more peer — the one that happens to have a disk and no garbage collector of its own. That is what lets an app keep working when this machine is off or the phone is out of the house.

It also does not merge. Two devices writing the same document resolve that between themselves, using the etag this server hands out. See "Concurrency" below.

## Running it

```
DATA_ROOT=./data \
TLS_CERT=./certs/server-cert.pem \
TLS_KEY=./certs/server-key.pem \
ALLOWED_ORIGINS=https://you.github.io/app \
npm start
```

On startup it prints the URL, the token, and a pairing code to paste or scan into an app.

| Variable          | Meaning                                                             |
| ----------------- | ------------------------------------------------------------------- |
| `DATA_ROOT`       | Required. Folder documents are stored in.                           |
| `PORT`            | Default `8443`.                                                     |
| `TLS_CERT`        | Certificate. Required in practice — see below.                      |
| `TLS_KEY`         | Matching private key. Both or neither.                              |
| `ALLOWED_ORIGINS` | Comma-separated. `localhost:5173` and `:4173` are always allowed.   |
| `TOKEN`           | Overrides the stored token. Otherwise generated once into `.token`. |
| `PUBLIC_HOST`     | Hostname to advertise. Defaults to `<hostname>.local`.              |

## HTTPS is not optional

A page served over HTTPS cannot call a plain-HTTP address on the local network. Chrome and Edge relax that only once local network access has been granted, and **Safari refuses outright — it blocks mixed content even to `http://localhost`**. Since an installed PWA is served over HTTPS, a server without TLS is reachable from nothing but its own machine.

Certificates come from [mkcert](https://github.com/FiloSottile/mkcert):

```
mkcert -install
mkcert -cert-file certs/server-cert.pem -key-file certs/server-key.pem \
       "$(hostname).local" 192.168.x.x localhost 127.0.0.1
```

Every device that talks to the server needs mkcert's root CA installed. On iOS that is two steps, and the second is easy to miss: install the profile, **then** Settings → General → About → Certificate Trust Settings and turn on full trust. Without the second step iOS still refuses the certificate.

Put the machine's address on a DHCP reservation. The certificate names an IP, and a certificate that names last week's IP is a certificate for nothing.

## The local network permission

Chrome and Edge block a public page from reaching a private address until the user allows it, and no response header can grant it on the page's behalf — `Access-Control-Allow-Private-Network` is sent, and it is not enough on its own. The first sync from a given site raises a prompt; allow it once per device. Measured on this machine, a blocked request fails in about 5 ms with a bare `TypeError`, indistinguishable from the server being switched off.

## API

```
GET  /api/health                    -> 200 {name, version}          no token needed
GET  /api/apps/:app/docs/:doc       -> 200 <body> + ETag, or 404
PUT  /api/apps/:app/docs/:doc       -> 201 created / 200 updated + ETag
```

Everything except `/api/health` needs `Authorization: Bearer <token>`.

`:app` and `:doc` must match `^[a-z0-9-]{1,64}$`. Anything else is a 400 — the names become path segments, and an allowlist cannot express "go up a directory" the way a blocklist has to anticipate it.

### Concurrency

Every `PUT` must carry a precondition:

- `If-None-Match: *` — create; 412 if the document already exists.
- `If-Match: "<etag>"` — update; 412 if it changed since you read it.

There is no unconditional write. Every client either read an etag or got a 404, so every client can say which case it is in, and an accidental clobber becomes impossible to express rather than merely discouraged.

On a 412 the client re-reads, merges again, and retries. The etag is a SHA-256 of the stored bytes, so identical content always has the same etag, and a write that changes nothing is free to detect.

Bodies are stored as the exact bytes sent, never reparsed and reserialized — otherwise a body that came back differently spaced would change etag without anyone editing it, and every client would see a phantom conflict. They are still checked for being valid JSON; they are just never rewritten.

Writes are atomic: a temporary file in the same directory, flushed, then renamed over the target. A reader sees the old document or the new one, never half of either.

## Storage layout

```
<DATA_ROOT>/
  .token
  todo/
    data.json
```

Plain files, readable and editable by hand while the server runs — it reads on every request and caches nothing.

## Tests

```
npm test        # vitest
npm run build   # tsc --noEmit
```

The HTTP tests drive Fastify through `inject`, so they need no port, no certificate, and have no teardown race.
