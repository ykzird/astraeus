# Vendored icons

`web/icons.js` is a **generated file**. Do not hand-edit it; run the generator:

```sh
node scripts/fetch-icons.mjs
```

## The set

| | |
|---|---|
| Set | **BoxIcons v2** (`bx`) |
| Style | Solid — the set's default style |
| Home | <https://icon-sets.iconify.design/bx/> |
| Source repo | <https://github.com/box-icons/boxicons> |
| Author | Boxicons |
| Licence | **MIT** — <https://github.com/box-icons/boxicons/blob/main/LICENSE> |
| Licence text | [`bx.LICENSE`](bx.LICENSE) |
| Registry size | 5,234 bytes |
| SHA-256 | `f9380e2b0ecc1ae9f03b17c74e8e2c2deacb6a74af4672c3a093a2ed71a444f7` |
| Source URL | `https://api.iconify.design/bx.json?icons=play,pause,refresh,fast-forward,stop,volume-full,volume-mute,fullscreen,exit-fullscreen,chevron-right` |

The project-wide summary lives in [`THIRD_PARTY_NOTICES.md`](../../THIRD_PARTY_NOTICES.md).

## Why vendored

Nothing this UI ships may depend on a third-party origin at runtime — the same
rule that governs `hls.min.js`. The generator fetches the icons once at
development time and bakes the path data into `web/icons.js`, so the served page
makes no external request, works offline, and cannot break when someone else's
CDN has a bad day.

Only the ten icons actually used are stored, which keeps the file at a few
kilobytes rather than shipping an entire set.

## The icons

| Registry key | BoxIcons icon | Used for |
|---|---|---|
| `play` | `bx:play` | hero Play button and the overlay play/pause toggle |
| `pause` | `bx:pause` | the overlay toggle while playback runs |
| `restart` | `bx:refresh` | replay from the beginning |
| `skip-forward` | `bx:fast-forward` | seek forward ten seconds |
| `stop` | `bx:stop` | stop playback and close the player |
| `volume` | `bx:volume-full` | the mute toggle while audio is audible |
| `volume-mute` | `bx:volume-mute` | the mute toggle while muted or at zero volume |
| `fullscreen-enter` | `bx:fullscreen` | enter fullscreen |
| `fullscreen-exit` | `bx:exit-fullscreen` | leave fullscreen |
| `arrow-right` | `bx:chevron-right` | the decision-reason list marker |

## How it is consumed

`web/icons.js` exposes one function:

```js
AstraeusIcons.icon(name, { label, className })  // -> <svg class="icon">
```

The result is `aria-hidden="true"` by default, because icons sit inside buttons
that already carry an accessible name. Pass `label` when an icon stands alone
and has to convey the meaning itself; it then becomes `role="img"` with a
`<title>`. Paths use `fill="currentColor"`, so colour comes from the surrounding
CSS and the icons follow every button variant.

`web/app.js` wraps this in a local `icon()` helper that degrades to an empty
placeholder if the registry is missing, so a failed script load cannot leave a
button without its accessible name.

## Updating

1. Add or change the entry in the `WANTED` table in
   [`scripts/fetch-icons.mjs`](../../scripts/fetch-icons.mjs), which maps each
   registry key to a BoxIcons name.
2. Run `node scripts/fetch-icons.mjs`. It fails loudly if a name does not exist
   in the set, so a typo cannot silently produce a blank icon.
3. Update the size, SHA-256 and icon table above with the values the generator
   prints.
4. Re-check the UI: a wrong name shows up as an empty square.

## On the choice of set

BoxIcons is **MIT**, which carries no attribution requirement beyond including
the copyright and permission notice — satisfied by [`bx.LICENSE`](bx.LICENSE)
and the notices file. Sets differ sharply here, and it is worth checking before
swapping:

| Set | Licence |
|---|---|
| BoxIcons (`bx`) | MIT |
| Phosphor (`ph`) | MIT |
| Tabler (`tabler`) | MIT |
| Lucide (`lucide`) | ISC |
| Solar (`solar`) | CC BY 4.0 — attribution required, and modifications must be indicated |

To move to another set, change the constants and the `WANTED` table at the top
of the generator, regenerate, and update this file and the notices file. Every
licence above still requires its notice to be reproduced, so none of them makes
the attribution files optional.
