# Security policy

## Reporting a vulnerability

**Please report privately**, through GitHub's
[Report a vulnerability](https://github.com/ykzird/astraeus/security/advisories/new)
form (the repository's **Security** tab). That opens a draft advisory only you
and the maintainer can see. If you cannot use it, open an issue that says you
have a security report and nothing more, and a private channel will be arranged.

Please include what you did, what happened, what you expected, and the version
(`astraeus-server version`) or commit. A proof of concept is welcome; a working
exploit against someone else's instance is not.

There is no bug bounty. There is a genuine thank-you in the release notes unless
you would rather there was not.

## What is in scope

This is a media server that is expected to sit behind an access gate and, on
some installs, on the public internet. The parts worth attacking, and the parts
that have had the most thought:

- **The access gate** (`internal/access`) and anything that lets a request past
  it — including the `proxy` mode's trusted-address check and identity headers.
- **The API** (`internal/api`): path traversal in the artwork and subtitle
  routes, the media-object allow-list, request-body handling, and anything that
  turns a request into a filesystem path or an outbound URL.
- **The artwork proxy** (`internal/images`), which fetches from an upstream and
  is the closest thing here to an SSRF surface.
- **The streaming session manager** (`internal/streaming`): whether a client can
  name a file it should not reach, or exhaust the session cap.
- **The container and the systemd unit**, in `deploy/`.

## What is not a vulnerability

- **No TLS.** The server speaks plain HTTP by design and is meant to sit behind a
  reverse proxy; `deploy/` covers that. An exposed plaintext port is a
  configuration problem, not a defect.
- **No per-user access control.** The gate decides whether a request is admitted,
  not what it may see: every admitted viewer sees the whole library. That is a
  documented limitation (`TODO.md`), not a bug.
- **A progress position the client made up.** The server validates the range and
  clears a finished position; it cannot tell a real position from a plausible
  wrong one, and the worst case is a resume in the wrong place.
- **`--auth-mode none` on a public address.** The default is documented as
  LAN-only, and the runbook turns the gate on before publishing a port.

## Supported versions

The latest tagged release. There are no maintenance branches: a fix lands on
`main` and goes out in the next tag.
