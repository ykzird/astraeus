# Context

This document defines the ubiquitous language for the Spatial Media Environment project. It is a glossary of terms and their relationships. It does not contain implementation details.

## Glossary

### Media
* **MediaEntity**: A logical representation of a piece of content.
    * **ContainerEntity**: A `MediaEntity` that acts as a parent to other `MediaEntity` objects (e.g., a `Series` or a `Season`).
    * **LeafEntity**: A `MediaEntity` that represents the final node in a hierarchy (e.g., an `Episode` or a `Movie`).
* **MediaObject**: The physical file(s) on disk that represent a `LeafEntity`.
* **Library**: A logical collection of `MediaEntity` objects (e.g., "Movies", "TV Shows").

### Metadata
* **MetadataSet**: A collection of descriptive attributes (title, poster art, etc.) associated with a `MediaEntity`. A `MediaEntity` is considered **Incomplete** until a `MetadataSet` is successfully attached.
* **Provider** (`internal/metadata`): An external service (e.g., TMDB) used to fetch a `MetadataSet`.

### Streaming
* **StreamSession**: An active, stateful connection between a client and the server for delivering a `MediaEntity`.
* **ClientCapability**: A description of the client's technical capabilities (codecs, resolutions, protocols).
* **Ladder**: The set of `Rendition`s one `StreamSession` offers, so the client's player can switch between them as the network changes. Each rung is a separate encode of the same source.
* **Rendition**: One rung of a `Ladder`: a height and the bitrate ceiling the video at that height is held to.
* **Preferred height**: The height a client asks a `Ladder` to be topped at. It is a quality choice expressed as a ceiling on adaptation, not a promise of one rendition: the player may step down, which is why the quality menu sends it. `max_height` instead pins exactly one `Rendition`.
* **TranscodeJob**: A background task managed by the server to convert a `MediaObject` from its original format into a format that satisfies a `StreamSession`'s requirements. A `StreamSession` may be mediated by a `TranscodeJob` if the `MediaObject` cannot be played directly.
* **Burn-in**: Compositing an image-based subtitle (a `SubtitleTrack` with `Text` false) into the video, because a browser cannot render a timed bitmap as a subtitle track. A burn-in makes the `StreamSession` a single-rendition re-encode, and it is irreversible for that session. It is the fallback when OCR cannot read the track.
* **OCR** (Optical Character Recognition): Reading the words out of an image `SubtitleTrack`'s bitmaps so the track can be delivered as WebVTT and toggled, restyled and searched like a text track. This server does it for PGS and VobSub with an optional engine (tesseract); the engine's absence is not an error, it just leaves the track to Burn-in.
* **Image subtitle**: A `SubtitleTrack` whose cues are pictures rather than text (PGS, VobSub, DVB), reported with `Text` false. PGS and VobSub can be read by OCR; DVB has no decoder here and stays burn-only.

### Playback
* **PlaybackProgress**: the position a `User` reached in a `LeafEntity`, recorded per user and entity so a `StreamSession` can resume there. A `LeafEntity` whose `PlaybackProgress` reaches the closing fraction of its duration is **Finished**, and its progress is cleared rather than kept.

### Access
* **User**: An authenticated individual with access to the application.
* **AccessPolicy**: The set of rules governing access to the application: the gate, which decides whether a request is admitted at all, and the per-Viewer grants, which decide which `Library`s a Viewer may see and who may change them. With no policy configured every admitted Viewer sees and may change everything; with one, a Viewer the policy does not name sees nothing.
* **Viewer**: The identity a `PlaybackProgress` belongs to. It is the identity the `AccessPolicy` attaches to a request; when the gate is disabled the instance has a single Viewer, named by the domain as the local viewer.
