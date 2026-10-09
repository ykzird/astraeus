# TLS in front of Astraeus Media

The server speaks plain HTTP by design. Terminating TLS, sending
`Strict-Transport-Security`, and establishing *who* a viewer is all belong to a
reverse proxy, and this page is the runbook for that. It uses
[Caddy](https://caddyserver.com) because it obtains and renews certificates
itself; the rules any other proxy has to satisfy are
[at the end](#any-other-proxy), and they are the part that matters.

This is also what turns the access gate into something you can expose. The gate
does not have a user database — it verifies that an identity asserted by a proxy
came from that proxy, which is the only thing that makes the assertion worth
anything. Read [the trust boundary](#the-trust-boundary-read-this-first) before
you change any address below.

Read [`deploy/README.md`](../README.md) first for installing the server itself.
This page assumes a working systemd install on `127.0.0.1:8642` with
`/etc/astraeus/` already created.

## The trust boundary (read this first)

Two halves have to line up, and a mistake in either is the difference between a
gate and a decoration:

1. **The proxy sets the identity header, and only the proxy does.** The server
   takes no interest in an identity the client sent, so the proxy must *replace*
   the header on every request rather than passing one through.
2. **The server believes that header only from a nominated address.**
   `--trusted-proxy` names it; anything else is refused with `403` and
   `code=untrusted_source` even when it presents a perfectly well-formed header.

`X-Forwarded-For` is deliberately **not** consulted for either half. The address
a connection actually came from is the only one a client cannot forge, so that
is the one the gate uses — which is also why the proxy has to run on the same
host, or connect from an address you list explicitly.

The consequence worth internalising: **anything that can reach the server from a
trusted address can claim any identity.** That is why `--trusted-proxy` should
name one address or a narrow network and never a broad range.

## Quick start

### 1. A certificate authority for your testers

Each tester gets a client certificate. This is the identity the gate sees, so it
is also what makes per-viewer playback progress work: two certificates are two
viewers.

```sh
sudo install -d -m 0700 /etc/astraeus/tls
cd /etc/astraeus/tls

# The CA that signs client certificates. Keep clients-ca.key off any backup you
# would not want to leak: it can mint an identity for anyone.
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
  -keyout clients-ca.key -out clients-ca.pem \
  -subj "/CN=Astraeus clients CA"

# One per tester. CN becomes the viewer's identity, so use something you can
# recognise in a log: an email address or a name.
for user in alice bob; do
  openssl req -newkey rsa:2048 -nodes -keyout "$user.key" -out "$user.csr" -subj "/CN=$user"
  openssl x509 -req -in "$user.csr" -CA clients-ca.pem -CAkey clients-ca.key \
    -CAcreateserial -days 365 -out "$user.crt"
done
```

Give each tester their `.crt` and `.key` and tell them to import both into their
browser. A certificate without its key is useless, and a client certificate
cannot be typed into a password box — this is why it is a good fit for a handful
of beta testers and a poor one for strangers.

### 2. A server certificate

Two arrangements; pick one.

**Your own certificate** (a wildcard from your DNS provider, an internal CA, or
anything else you already have):

```sh
cd /etc/astraeus/tls
# server.crt must carry the name clients use, and server.key must match it.
sudo cp /path/to/your/fullchain.pem server.crt
sudo cp /path/to/your/privkey.pem   server.key
```

**Or let Caddy obtain one.** Point a public DNS name at the host, make sure
ports 80 and 443 reach it, and delete the two `tls` arguments in the Caddyfile
so it reads `tls {` instead of `tls /etc/caddy/tls/server.crt ... {`. Caddy then
handles issuance and renewal and you can skip this step entirely.

### 3. Point the server at the proxy

The unit ships without `--auth-mode`, which is `none`. That is enough on a
trusted network and not enough once this proxy is the only way in, so the
installation below switches it. (It used to ship with `--auth-mode token`, which
was wrong twice over: it neither gives the proxy an identity to forward nor lets a
browser through at all.) Switch to `proxy` and name the header and the address:

```sh
sudo systemctl edit astraeus        # or edit the unit directly
```

Add to the `ExecStart` command:

```
    --auth-mode proxy \
    --auth-header X-Astraeus-User \
    --trusted-proxy 127.0.0.1/32,::1/128
```

Both loopback families are listed deliberately: **a proxy that resolves
`localhost` may well connect over IPv6**, and `127.0.0.1/32` alone does not
cover `::1`. The symptom is a `403` with `code=untrusted_source` for every
request through a proxy that is obviously running, which reads like a bug in the
proxy rather than in the address list. If your proxy runs on another host, list
that host's address instead — and understand that everything on it is then
trusted.

Then:

```sh
sudo systemctl daemon-reload
sudo systemctl restart astraeus
journalctl -u astraeus -n5 | grep 'access gate enabled'   # mode=proxy trusted_proxies=2
```

Do not add `--auth-mode token` beside these flags: a browser cannot send a bearer
token, so the UI would be refused with `401` and the identity the proxy forwards
would be ignored.

### 4. Run Caddy

Copy [`Caddyfile`](Caddyfile) to `/etc/astraeus/Caddyfile` and set the hostname:

```sh
sudo cp deploy/tls/Caddyfile /etc/astraeus/Caddyfile
sudo sed -i 's/^{$ASTRAEUS_HOST:media.example.com}/media.example.com/' /etc/astraeus/Caddyfile
```

Then start it, on the host's network so that its connection to the server comes
*from* loopback and is therefore trusted:

```sh
docker run -d --name astraeus-caddy --restart unless-stopped --network host \
  -v /etc/astraeus/tls:/etc/caddy/tls:ro \
  -v /etc/astraeus/Caddyfile:/etc/caddy/Caddyfile:ro \
  -v astraeus-caddy-data:/data \
  -e ASTRAEUS_HOST=media.example.com \
  -e ACME_EMAIL=you@example.com \
  caddy:2

docker logs astraeus-caddy | tail
```

`--network host` is not a shortcut. On a bridge network Caddy would reach the
server from the bridge address, which is not in `--trusted-proxy`, and the gate
would refuse every request. If you prefer bridge networking, name the bridge's
gateway in `--trusted-proxy` and point `reverse_proxy` at it.

If the host cannot bind port 80 — a container without the capability, or
something already there — uncomment `auto_https disable_redirects` in the
Caddyfile's global block. Clients that type the bare hostname then get a
connection error instead of a redirect to HTTPS.

The shipped `Caddyfile` is what `caddy fmt` produces; if you edit it, put it
back through the formatter rather than leaving a diff behind:

```sh
docker run --rm -i --entrypoint caddy caddy:2 fmt - < Caddyfile
```

### 5. Check it

Run all of these from a machine that is not the server, with a tester's
certificate:

```sh
# No client certificate: this must fail before any HTTP happens.
curl --cacert clients-ca.pem https://media.example.com/api/entities

# With one: 200, and the HSTS header the server itself never sends.
curl --cacert clients-ca.pem --cert alice.crt --key alice.key -D - \
  https://media.example.com/api/entities | grep -i strict-transport

# The gate refused nothing, and named the right person.
journalctl -u astraeus -n20 | grep 'http request'      # user="CN=alice"
```

Two viewers must not share a place. With `alice` reporting a position and `bob`
reading the same entity, `bob` must see no `progress` at all:

```sh
E=<entityId>
curl --cert alice.crt --key alice.key --cacert clients-ca.pem \
  -X PUT -H 'Content-Type: application/json' \
  -d '{"position_seconds":120,"duration_seconds":600}' \
  https://media.example.com/api/entities/$E/progress          # 204

curl --cert bob.crt --key bob.key --cacert clients-ca.pem \
  https://media.example.com/api/entities/$E | grep -o '"progress":[^,]*'
```

Finally, prove the boundary holds from the outside. From another host on the
network, ask the server directly — not through the proxy — for the API, with an
identity header of your own invention. It must be refused:

```sh
curl -s -o /dev/null -w '%{http_code}\n' \
  -H 'X-Astraeus-User: anyone' http://<server-lan-ip>:8642/api/entities   # 403
```

That last check only means something if the server is reachable there, so if it
answers `000` the server is bound to loopback and the test proved nothing. Bind
it to the LAN address temporarily, or trust the `::1` result above, which
exercises the same check.

## If Caddy already runs on the host

The steps above start Caddy in a container because that is the shortest path from
nothing. On a host that already runs Caddy as a service — with its own
certificates, its own DNS challenge and other sites — do not stand up a second
proxy. Add a site block instead:

```caddyfile
astraeus.example.com {
	tls /etc/astraeus/tls/server.crt /etc/astraeus/tls/server.key {
		client_auth {
			mode require_and_verify
			trust_pool file /etc/astraeus/tls/clients-ca.pem
		}
	}

	header {
		Strict-Transport-Security "max-age=31536000; includeSubDomains"
		-Server
	}

	reverse_proxy 127.0.0.1:8642 {
		header_up X-Astraeus-User {http.request.tls.client.subject}
	}
}
```

If that Caddy already obtains its certificates over a DNS challenge, drop the two
arguments after `tls` so it reads `tls {` and leave issuance and renewal to it.
The `tls` and `reverse_proxy` directives are the ones
[verified below](#what-was-verified); only the way Caddy is started differs.

Two permissions details differ from the container, because a host Caddy runs as
its own account rather than as root in a container:

- that account must be able to read `clients-ca.pem`, `server.crt` and
  `server.key` — a shared group and `install -d -m 0750` is usually enough;
- it must **not** be able to read `clients-ca.key`. That key mints identities, is
  needed only while issuing a certificate, and belongs to you rather than to the
  proxy.

A name with no public DNS record works identically, provided the name resolves
for your clients (split DNS, MagicDNS) and the certificate covers it. A wildcard
for the parent domain already covers a single-label name beneath it.

## Any other proxy

nginx, Traefik, HAProxy and `tailscale serve` all work, and the requirements are
the same four:

1. **Terminate TLS** and send `Strict-Transport-Security`. The server does not
   send it and would be lying if it did.
2. **Set the identity header on every request**, replacing any value the client
   sent. For nginx, `proxy_set_header` does that; for anything with a
   pass-through mode, verify it rather than assuming.
3. **Connect from an address in `--trusted-proxy`**, and list `::1/128` beside
   `127.0.0.1/32` if the proxy may use either.
4. **Do not buffer the media path into a timeout.** Segments are file-sized and a
   live transcode produces playlists that grow; a proxy with aggressive response
   buffering or a short `proxy_read_timeout` shows up as a player that stalls
   after the first segment. Caddy streams by default; in nginx the settings that
   matter are `proxy_buffering off` and a generous `proxy_read_timeout` for
   `/hls/`.

If you would rather use an identity-aware proxy than client certificates, the
gate already knows two: **`Tailscale-User-Login`** (set by `tailscale serve`) and
**`Cf-Access-Authenticated-User-Email`** (set by Cloudflare Access). Name the one
your proxy actually sets:

```sh
# Tailscale, and only Tailscale, in front
./astraeus-server serve --auth-mode proxy \
  --auth-header Tailscale-User-Login \
  --trusted-proxy 127.0.0.1/32,::1/128

# Cloudflare Access, and only Cloudflare, in front
./astraeus-server serve --auth-mode proxy \
  --auth-header Cf-Access-Authenticated-User-Email \
  --trusted-proxy 127.0.0.1/32,::1/128
```

Constraining the header is not optional. The gate believes a configured header
whenever it is non-empty, so a deployment that accepts `Tailscale-User-Login`
*and* `Cf-Access-Authenticated-User-Email` lets a user of whichever proxy is
actually in front present the other proxy's header and claim any identity it
likes, an administrator's included: each proxy only overwrites its own header.
Their advantage over client certificates is that testers install nothing.

## What was verified

Everything above was run, not reasoned about: a Debian host serving
`127.0.0.1:8642` behind Caddy 2 with `client_auth`, and the checks below.

| Check | Result |
| --- | --- |
| No client certificate | TLS handshake fails; no HTTP request is served |
| Valid client certificate | `HTTP/2 200`; `strict-transport-security: max-age=31536000; includeSubDomains`; no `Server` header |
| Identity reaches the gate | `user="CN=tester1"` and `user="CN=tester2"` on the request log lines |
| A client-forged identity header | overwritten by the proxy; the forged value appears nowhere in the log |
| Per-viewer progress | `tester1` reports 120 s and reads it back; `tester2` sees no `progress` on the same entity |
| Direct play through TLS | `direct_play`, then a range request returns `206` with `content-range: bytes 0-1023/83125` |
| Transcode through TLS | `transcode`, playlist and a 47 KiB segment fetched, session stopped with `204` |
| LAN address with a forged header | `403`, `code=untrusted_source` |
| Tailscale address with a forged header | `403`, `code=untrusted_source` |
| IPv6 loopback with a valid header | `403`, `code=untrusted_source` — the trap in step 3 |

## Things that will bite

- **`127.0.0.1/32` does not cover `::1`.** See step 3. This is the single most
  likely reason a correct proxy config returns `403` everywhere.
- **The request log shows the proxy, not the viewer.** Every line's `remote`
  address is the proxy's, so per-IP analysis of the log is per-proxy analysis.
  The `user` field is where the person is.
- **Rate limiting keys on identity behind `proxy` mode**, which is what you want
  per person — but `--rate-limit` is off by default, and with the gate off the
  limiter keys on the peer address, which behind a proxy is the proxy itself, so
  the limit becomes global. See [`deploy/README.md`](../README.md).
- **HSTS is a commitment.** `max-age=31536000` tells browsers to refuse plain
  HTTP to that hostname for a year. Do not send it for a name you also want to
  reach over HTTP, and consider a short `max-age` for the first week.
- **A client certificate is verified; an identity header is only asserted.** This
  arrangement is the stronger of the two, and it is why it is the one written up:
  Caddy refuses the connection unless the client proves possession of a
  certificate signed by your CA, so the identity is established by the handshake
  rather than claimed. Replace it with an identity-aware proxy — Cloudflare
  Access, or Tailscale Serve — and the server believes a header instead, which is
  not signed. That is sound only while the proxy is the *only* path to the port,
  which is the real reason the backend stays on loopback. Cloudflare signs
  `Cf-Access-Jwt-Assertion` so an origin can check the claim rather than trust
  it; this server does not validate it, so an Access deployment rests on path
  control alone.
- **Revocation is your problem.** A client certificate is valid until it expires
  and there is no list to consult. For a beta, short lifetimes and reissuing are
  simpler than standing up CRL or OCSP infrastructure.

## Not covered

- **What a viewer may do inside a library it can see.** `--access-policy`
  decides which libraries each viewer may see and who may change the library —
  see [`docs/configuration.md`](../../docs/configuration.md) — but within a
  library a viewer can see, everything in it is available to them.
- **Certificate lifecycle.** Nothing here revokes, rotates or audits a client
  certificate. `openssl` and a calendar are the whole toolchain.
- **High availability.** One proxy, one server, one host. Several servers behind
  one proxy each hold their own rate-limit buckets and their own sessions.
