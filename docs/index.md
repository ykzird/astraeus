# Documentation

The [README](../README.md) is the way in. Everything below assumes you have it
running, or are about to.

## Using it

| Document | What it covers |
| --- | --- |
| [Playback and delivery](playback.md) | Direct play, remux, transcode; the capability manifest; HDR and hardware encoders; audio tracks; image subtitles and OCR; the adaptive ladder |
| [Configuration and operations](configuration.md) | Scanning libraries, every flag, the security headers, the access gate, rate limiting and tracing |
| [HTTP API](api.md) | Every endpoint, the playback request and response, resume state and metrics |
| [Deployment](../deploy/README.md) | The container and the systemd unit, backups and upgrades, and what the unit hardens |
| [TLS in front of it](../deploy/tls/README.md) | Terminating TLS, the identity the access gate believes, client certificates, and the trust boundary that makes any of it worth something |

## Working on it

| Document | What it covers |
| --- | --- |
| [Development](development.md) | The tests, what they assert, and the browser harnesses |
| [CONTRIBUTING.md](../CONTRIBUTING.md) | The rules a change here follows |
| [SPECIFICATION.md](../SPECIFICATION.md) | The design and the reasoning behind it |
| [TODO.md](../TODO.md) | What is missing, and what is known to be unverified |
| [CONTEXT.md](../CONTEXT.md) | The domain glossary |
| [web/README.md](../web/README.md) | The front end specifically |

## State and history

| Document | What it covers |
| --- | --- |
| [handoff.md](https://github.com/ykzird/astraeus/blob/main/docs/handoff.md) | Where the project is, written for whoever picks it up next — person or agent: working agreements, environment traps, how to verify each claim, and what is known to be unverified. Repository only, so that a release archive stays free of the project's own working documents |
