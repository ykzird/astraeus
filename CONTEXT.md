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
* **TranscodeJob**: A background task managed by the server to convert a `MediaObject` from its original format into a format that satisfies a `StreamSession`'s requirements. A `StreamSession` may be mediated by a `TranscodeJob` if the `MediaObject` cannot be played directly.

### Playback
* **PlaybackProgress**: the position a `User` reached in a `LeafEntity`, recorded per user and entity so a `StreamSession` can resume there. A `LeafEntity` whose `PlaybackProgress` reaches the closing fraction of its duration is **Finished**, and its progress is cleared rather than kept.

### Access
* **User**: An authenticated individual with access to the application.
* **AccessPolicy**: The set of rules governing access to the application. Currently implemented as a global gate for the entire instance, determining whether a `User` can access the `Library`.
* **Viewer**: The identity a `PlaybackProgress` belongs to. It is the identity the `AccessPolicy` attaches to a request; when the gate is disabled the instance has a single Viewer, named by the domain as the local viewer.
