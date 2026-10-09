/* ==========================================================================
   Astraeus Media — front-end
   --------------------------------------------------------------------------
   Vanilla ES2020, no build step, no dependencies. Served as a static file.

   Conventions enforced throughout:

     * Nothing derived from the API is ever written with innerHTML. Every
       entity name originates from a filename on disk, so all text goes
       through textContent / document.createTextNode via the el() helper
       below. The only HTML in this app is the static shell in index.html.
     * Every fetch has an AbortController timeout, so the UI can never spin
       forever. Failures always surface in the error banner.
     * Navigation state lives in location.hash, so breadcrumbs are real
       anchors and the back button behaves.
   ========================================================================== */

(function () {
  "use strict";

  /* The pure timeline maths, clock formatting and subtitle track classification
     live in core.js so they can be unit-tested without a browser
     (web/core.test.js). Aliasing them here keeps every call site below reading
     exactly as it did when they were local. */
  const {
    FINISHED_FRACTION, RESUME_MIN_SECONDS,
    formatClock, mediaTime, pad2, producedWindow, progressAction, sourceTime,
    subtitleDeliverable, subtitleNeedsBurn,
  } = window.AstraeusCore;
  /* Aliased because the local wrapper below keeps the name call sites use. */
  const { resumeOffsetFor: resumeOffsetFromProgress } = window.AstraeusCore;
  const { awaitJob: awaitJobOutcome, JOB_WAIT_MS: awaitJobWaitMs } = window.AstraeusCore;
  const {
    ENGINE_NATIVE: ENGINE_NATIVE, ENGINE_NATIVE_HLS: ENGINE_NATIVE_HLS,
    ENGINE_HLS_JS: ENGINE_HLS_JS, engineLabel: engineLabelFor,
    subtitleSelectable: subtitleSelectable, orphanedSessionId: orphanedSessionId,
    shouldRenegotiateAfterFailure: shouldRenegotiateAfterFailure,
    fetchFailure: fetchFailure,
  } = window.AstraeusCore;

  /* ── 1. DOM references ───────────────────────────────────────────────── */

  const dom = {
    breadcrumbs: document.getElementById("breadcrumbs"),
    healthPill: document.getElementById("health-pill"),
    errorBanner: document.getElementById("error-banner"),
    errorText: document.getElementById("error-text"),
    errorRetry: document.getElementById("error-retry"),
    errorDismiss: document.getElementById("error-dismiss"),
    libraryList: document.getElementById("library-list"),
    navSummary: document.getElementById("nav-summary"),
    navEmpty: document.getElementById("nav-empty"),
    continueSection: document.getElementById("continue-section"),
    continueList: document.getElementById("continue-list"),
    filterToggle: document.getElementById("filter-toggle"),
    incompleteCount: document.getElementById("incomplete-count"),
    incompleteDesc: document.getElementById("incomplete-desc"),
    enrichButton: document.getElementById("enrich-button"),
    actionStatus: document.getElementById("action-status"),
    canvas: document.getElementById("main-canvas"),
    canvasContent: document.getElementById("canvas-content"),
    playerLayer: document.getElementById("player-layer"),
    contextSub: document.getElementById("context-sub"),
    contextBody: document.getElementById("context-body"),
    toasts: document.getElementById("toasts"),
  };

  /* ── 2. Tiny DOM + format helpers ────────────────────────────────────── */

  /**
   * Build an element. `props` supports class, text, dataset, style (an object
   * of custom properties) and boolean DOM properties such as disabled/hidden;
   * anything else becomes an attribute. Children may be nodes, strings,
   * arrays, or null/false to skip.
   */
  function el(tag, props, children) {
    const node = document.createElement(tag);
    if (props) {
      for (const key of Object.keys(props)) {
        const value = props[key];
        if (value === null || value === undefined || value === false) continue;
        if (key === "class") {
          node.className = value;
        } else if (key === "text") {
          node.textContent = String(value);
        } else if (key === "dataset") {
          for (const dk of Object.keys(value)) node.dataset[dk] = String(value[dk]);
        } else if (key === "style" && typeof value === "object") {
          for (const sk of Object.keys(value)) node.style.setProperty(sk, String(value[sk]));
        } else if (typeof value === "boolean" && key in node) {
          node[key] = value;
        } else {
          node.setAttribute(key, String(value));
        }
      }
    }
    appendChildren(node, children);
    return node;
  }

  function appendChildren(parent, children) {
    if (children === null || children === undefined || children === false) return;
    if (Array.isArray(children)) {
      for (const child of children) appendChildren(parent, child);
      return;
    }
    parent.append(children instanceof Node ? children : document.createTextNode(String(children)));
  }

  function clear(node) {
    while (node.firstChild) node.removeChild(node.firstChild);
  }

  /**
   * Build an icon from the registry in icons.js, which index.html loads before
   * this file. If that script is missing the UI stays usable: every control
   * also carries its own accessible name, so only the glyph is lost.
   */
  function icon(name, options) {
    if (window.AstraeusIcons && typeof window.AstraeusIcons.icon === "function") {
      return window.AstraeusIcons.icon(name, options);
    }
    return el("span", { class: "icon icon-missing", "aria-hidden": "true" });
  }

  /**
   * Swap a button's glyph and optional visible label. syncTransport runs on
   * every timeupdate, so an identical update is skipped rather than rebuilt.
   */
  function setIconButton(button, name, label) {
    if (!button) return;
    const nextLabel = label || "";
    if (button.dataset.icon === name && button.dataset.iconLabel === nextLabel) return;
    button.dataset.icon = name;
    button.dataset.iconLabel = nextLabel;
    clear(button);
    button.append(icon(name));
    if (nextLabel) button.append(el("span", { class: "btn-label", text: nextLabel }));
  }

  const countFmt = new Intl.NumberFormat(undefined, { maximumFractionDigits: 0 });
  const byteFmt = new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 });
  const dateFmt = new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  });

  function formatCount(value) {
    return countFmt.format(typeof value === "number" && isFinite(value) ? value : 0);
  }

  function formatBytes(value) {
    if (typeof value !== "number" || !isFinite(value) || value < 0) return "—";
    if (value === 0) return "0\u00A0B";
    const units = ["B", "KB", "MB", "GB", "TB", "PB"];
    let amount = value;
    let unit = 0;
    while (amount >= 1024 && unit < units.length - 1) {
      amount /= 1024;
      unit += 1;
    }
    return byteFmt.format(amount) + "\u00A0" + units[unit];
  }

  function formatDate(iso) {
    if (!iso) return "—";
    const date = new Date(iso);
    if (isNaN(date.getTime())) return "—";
    return dateFmt.format(date);
  }

  function plural(n, singular, pluralForm) {
    return n === 1 ? singular : pluralForm || singular + "s";
  }

  function basename(filePath) {
    if (typeof filePath !== "string" || !filePath) return "—";
    const parts = filePath.split(/[\\/]/);
    return parts[parts.length - 1] || filePath;
  }

  function fnv1a(text) {
    let hash = 0x811c9dc5;
    for (let i = 0; i < text.length; i += 1) {
      hash ^= text.charCodeAt(i);
      hash = Math.imul(hash, 0x01000193);
    }
    return hash >>> 0;
  }

  /* ── 3. API client ───────────────────────────────────────────────────── */

  /* Relative to the document, so the app works from any mount point: the
     server serves this directory at "/" and the API at "/api/…". */
  const API_BASE = "api/";
  const REQUEST_TIMEOUT_MS = 15000;

  class ApiError extends Error {
    constructor(message, details) {
      super(message);
      const info = details || {};
      this.name = "ApiError";
      this.status = info.status || 0;
      this.code = info.code || "";
      this.path = info.path || "";
      /* The parsed error envelope, when the server sent one. Carries the
         `decision` block on some failures, so the UI can explain itself. */
      this.body = info.body || null;
    }
  }

  async function apiFetch(path, options) {
    const opts = options || {};
    const method = opts.method || "GET";
    const controller = new AbortController();
    const timer = setTimeout(function () {
      controller.abort();
    }, REQUEST_TIMEOUT_MS);

    let response;
    try {
      response = await fetch(API_BASE + path, {
        method: method,
        headers: opts.body === undefined ? undefined : { "Content-Type": "application/json" },
        body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
        signal: controller.signal,
        cache: "no-store",
        credentials: "same-origin",
        /* A position reported as the page goes away has to outlive the
           document, which the browser will not let an ordinary request do. */
        keepalive: opts.keepalive === true,
      });
    } catch (error) {
      clearTimeout(timer);
      const failure = fetchFailure("send", error, REQUEST_TIMEOUT_MS);
      if (failure.kind === "timeout") {
        throw new ApiError(failure.message, { path: path, code: "timeout" });
      }
      throw new ApiError(
        "Could not reach the Astraeus API (" + API_BASE + path + "). Is the server running?",
        { path: path, code: "network" }
      );
    }

    /* The timer stays armed while the body is read. Clearing it here cleared it
       once the headers had arrived, so a server that sent a Content-Length and
       then stalled mid-body hung the request forever - and the module header's
       claim that every fetch has a timeout was false for exactly that case
       (W-9 of the 2026-10-09 review). */
    let text = "";
    try {
      text = await response.text();
    } catch (error) {
      const failure = fetchFailure("body", error, REQUEST_TIMEOUT_MS);
      if (failure.kind === "timeout") {
        throw new ApiError(failure.message, { path: path, code: "timeout" });
      }
      /* A body that failed to arrive for another reason is treated as empty, so
         the status still decides what the caller is told. */
      text = "";
    } finally {
      clearTimeout(timer);
    }

    if (!response.ok) {
      let message = "";
      let code = "";
      let parsedBody = null;
      if (text) {
        try {
          const parsed = JSON.parse(text);
          if (parsed && typeof parsed === "object") {
            parsedBody = parsed;
            if (typeof parsed.message === "string") message = parsed.message;
            if (typeof parsed.code === "string") code = parsed.code;
          }
        } catch (error) {
          /* Non-JSON error bodies still have to reach the user verbatim. */
          message = text.slice(0, 200);
        }
      }
      throw new ApiError(message || "The API responded with HTTP " + response.status + ".", {
        status: response.status,
        code: code,
        path: path,
        body: parsedBody,
      });
    }

    if (response.status === 204 || !text) return null;
    try {
      return JSON.parse(text);
    } catch (error) {
      throw new ApiError("The API returned a response that was not valid JSON.", {
        status: response.status,
        code: "invalid_json",
        path: path,
      });
    }
  }

  function expectArray(value, what) {
    if (!Array.isArray(value)) {
      throw new ApiError("The API did not return a list of " + what + ".", {
        code: "unexpected_shape",
      });
    }
    return value;
  }

  const api = {
    health: function () {
      return apiFetch("health");
    },
    libraries: function () {
      return apiFetch("libraries");
    },
    libraryEntities: function (libraryId) {
      return apiFetch("libraries/" + encodeURIComponent(libraryId) + "/entities");
    },
    entity: function (entityId) {
      return apiFetch("entities/" + encodeURIComponent(entityId));
    },
    scan: function (libraryId) {
      return apiFetch("libraries/" + encodeURIComponent(libraryId) + "/scan", { method: "POST" });
    },
    enrich: function () {
      return apiFetch("metadata/enrich", { method: "POST" });
    },
    /* A job's state and, once it has finished, its result. Scanning and
       enriching answer 202 with one of these to poll rather than holding the
       request open, because the request used to be cancelled by the client's own
       timeout partway through a large library (W-2). */
    job: function (id) {
      return apiFetch("jobs/" + encodeURIComponent(id));
    },
    /* Negotiates delivery for one entity. The body is a client capability
       manifest plus where in the source to begin; see playbackRequestBody().
       A body replaces the server's browser defaults outright rather than
       merging with them, so every request carries the full profile. */
    playback: function (entityId, body) {
      return apiFetch("entities/" + encodeURIComponent(entityId) + "/playback", {
        method: "POST",
        body: body,
      });
    },
    /* Where the viewer got to. PUT replaces the stored position rather than
       appending one, so a repeated report corrects it instead of piling up. */
    saveProgress: function (entityId, body, options) {
      return apiFetch("entities/" + encodeURIComponent(entityId) + "/progress", {
        method: "PUT",
        body: body,
        keepalive: options && options.keepalive === true,
      });
    },
    /* Forget a position outright; this is what starting over means. */
    clearProgress: function (entityId, options) {
      return apiFetch("entities/" + encodeURIComponent(entityId) + "/progress", {
        method: "DELETE",
        keepalive: options && options.keepalive === true,
      });
    },
    /* What is worth resuming, most recently watched first. The entity is
       embedded in each row, so the rail needs no second lookup. */
    progress: function () {
      return apiFetch("progress");
    },
  };

  /* ── 4. State ────────────────────────────────────────────────────────── */

  const state = {
    libraries: [],
    libraryId: null,
    entities: [],
    entitiesLibraryId: null,
    entityIndex: new Map(),
    detail: null,
    route: null,
    loading: false,
    busyAction: null,
    /* Everything the player knows. Replaced wholesale by resetPlayback(). */
    playback: emptyPlayback(),
    /* Volume and mute outlive any one session, so they live here rather than
       on `playback`, and are mirrored into localStorage. */
    audio: loadAudioPreference(),
    filterIncomplete: false,
    loadToken: 0,
    retry: null,
    booted: false,
    /* "Failed to load" is not "empty": these record which of the two we are in
       so the canvas never invites a scan for a list it never received. */
    librariesFailed: false,
    entitiesFailed: false,
    /* The continue-watching rail, most recently watched first. Empty covers
       both "nothing in progress" and "never loaded"; the section hides for
       either, so it never costs the rail an empty heading. */
    continueWatching: [],
    /* Set when a route change was driven by a deliberate user gesture, so
       the canvas (not <body>) receives focus once the new view is painted. */
    focusTarget: null,
  };

  /* status: idle      — nothing negotiated yet
             loading   — /playback in flight, or hls.js is being fetched
             ready     — a source is attached; paused/playing tracked live
             segmented — server chose HLS and nothing here can demux it
             error     — negotiation, network or decode failure */
  /* Volume and mute, remembered across sessions and reloads. Kept outside the
     per-session playback state because a new film must not reset the volume. */
  const AUDIO_STORAGE_KEY = "astraeus.audio";

  function loadAudioPreference() {
    const fallback = { volume: 1, muted: false };
    try {
      const raw = window.localStorage.getItem(AUDIO_STORAGE_KEY);
      if (!raw) return fallback;
      const parsed = JSON.parse(raw);
      if (!parsed || typeof parsed !== "object") return fallback;
      let volume = Number(parsed.volume);
      if (!isFinite(volume) || volume < 0 || volume > 1) volume = 1;
      return { volume: volume, muted: parsed.muted === true };
    } catch (error) {
      /* Private mode, a full quota or corrupt JSON: default quietly. */
      return fallback;
    }
  }

  function saveAudioPreference() {
    try {
      window.localStorage.setItem(AUDIO_STORAGE_KEY, JSON.stringify(state.audio));
    } catch (error) {
      /* Playback still works; we just forget the choice next time. */
    }
  }

  function audioIsMuted() {
    return state.audio.muted || state.audio.volume === 0;
  }

  /**
   * Push the stored preference onto the element. Called on every start, so a
   * rebuilt <video> can never come up at full volume, and once at init so the
   * element is primed before anything plays.
   */
  function applyAudioPreference() {
    const muted = audioIsMuted();
    try {
      /* Only assign on a real change: this runs on every timeupdate, and a
         redundant write can fire volumechange each time. */
      if (playerVideo.volume !== state.audio.volume) playerVideo.volume = state.audio.volume;
      if (playerVideo.muted !== muted) playerVideo.muted = muted;
    } catch (error) {
      /* Some engines refuse volume before metadata; the next start retries. */
    }
    const slider = document.getElementById("player-volume");
    if (slider) {
      slider.value = String(state.audio.volume);
      slider.setAttribute("aria-valuetext", muted ? "muted" : Math.round(state.audio.volume * 100) + "%");
    }
    const mute = document.getElementById("player-mute");
    if (mute) {
      const label = muted ? "Unmute" : "Mute";
      mute.setAttribute("aria-pressed", muted ? "true" : "false");
      mute.setAttribute("aria-label", label);
      mute.title = label;
      setIconButton(mute, muted ? "volume-mute" : "volume", null);
    }
    const level = document.getElementById("player-volume-level");
    if (level) {
      level.textContent = muted ? "Muted" : Math.round(state.audio.volume * 100) + "%";
    }
  }

  function setVolume(value) {
    const next = Number(value);
    if (!isFinite(next)) return;
    state.audio.volume = Math.min(1, Math.max(0, next));
    /* Raising the level is an unmute; leaving it at zero is a mute. */
    if (state.audio.volume > 0) state.audio.muted = false;
    applyAudioPreference();
  }

  function toggleMute() {
    const wasMuted = audioIsMuted();
    state.audio.muted = !wasMuted;
    /* Unmuting from silence has to restore an audible level. */
    if (!state.audio.muted && state.audio.volume === 0) state.audio.volume = 1;
    applyAudioPreference();
    saveAudioPreference();
  }

  /* ── 4. State ────────────────────────────────────────────────────────── */

  /**
   * The body of a playback request.
   *
   * The endpoint decodes this straight into the client's capability manifest:
   * sending a body replaces the server's browser defaults entirely, it does not
   * merge with them, and Validate() rejects a manifest with no codec lists. So
   * the full profile goes on every request, mirroring
   * streaming.BrowserCapability() on the server.
   *
   * `preferredHeight` is omitted for Auto, which asks the server for a ladder
   * topped at the browser's own horizontal ceiling (1920) — the server derives
   * the height from the source's aspect ratio. A chosen height is a ceiling the
   * player may step down from, not a guarantee of that exact rendition.
   *
   * `audioTrackIndex` is omitted for Auto as well: the contract reserves both
   * a missing field and 0 for "the server's choice", so neither is ever sent.
   *
   * `burnSubtitleIndex` names an image-based subtitle track to burn into the
   * picture. It shares the audio field's convention — absent and 0 both mean
   * "do not burn" — so it is only ever sent for a real global stream index.
   */
  function playbackRequestBody(startSeconds, preferredHeight, audioTrackIndex, burnSubtitleIndex) {
    const body = {
      containers: ["mp4", "webm", "hls"],
      video_codecs: ["h264", "vp9", "av1"],
      audio_codecs: ["aac", "opus", "mp3", "vorbis"],
      max_width: 1920,
      max_bitrate_kbps: 120000,
      max_bit_depth: 8,
      max_audio_channels: 2,
      // No browser is assumed to render HDR: the server tone maps a PQ source to
      // SDR for this profile rather than handing a compositor a stream it would
      // show washed out. Kept explicit to mirror BrowserCapability() exactly.
      supports_hdr: false,
      supports_hls: true,
      subtitles: true,
    };
    if (typeof preferredHeight === "number" && preferredHeight > 0) body.preferred_height = preferredHeight;
    if (typeof audioTrackIndex === "number" && isFinite(audioTrackIndex) && audioTrackIndex > 0) {
      body.audio_track_index = audioTrackIndex;
    }
    if (
      typeof burnSubtitleIndex === "number" &&
      isFinite(burnSubtitleIndex) &&
      burnSubtitleIndex > 0
    ) {
      body.burn_subtitle_index = burnSubtitleIndex;
    }
    if (typeof startSeconds === "number" && isFinite(startSeconds) && startSeconds > 0) {
      body.start_seconds = startSeconds;
    }
    return body;
  }

  function emptyPlayback() {
    return {
      entityId: null,
      title: null,
      status: "idle",
      mode: null,
      url: null,
      sessionId: null,
      objectId: null,
      decision: null,
      mediaInfo: null,
      reasons: [],
      error: null,
      duration: 0,
      currentTime: 0,
      seeking: false,
      started: false,
      /* One of ENGINE_NATIVE, ENGINE_NATIVE_HLS or ENGINE_HLS_JS; core.js owns
         the names and the wording, so a comparison cannot drift from what the
         player sets. */
      engine: null,
      /* True whenever delivery is segmented, i.e. generated while playing. */
      segmented: false,
      /* Why a segmented stream could not be handed to a demuxer; kept on the
         session so the rendered note can explain it, not just the toast. */
      segmentedCause: null,
      seekableStart: 0,
      seekableEnd: 0,
      /* Where this session begins in the source. Media time 0 is source time
         `sessionStart`, so source time = media time + sessionStart. The server
         also echoes it back; this is that value. */
      sessionStart: 0,
      /* A resume offset that direct play still has to apply itself: the server
         serves the whole original file and cannot start it mid-file, so the
         seek waits for the element to have a timeline. Always 0 for segmented
         delivery, where the server really did start at the offset. */
      resumeAt: 0,
      /* The whole film's duration, from media_info — not what this session has
         produced. The seek bar spans this. */
      sourceDuration: 0,
      /* Preferred ladder ceiling (null = Auto) and what the server actually
         chose. A choice caps the ladder; it never pins one rendition. */
      preferredHeight: null,
      targetHeight: 0,
      /* Requested audio stream index (null = Auto) and the index the server
         actually chose for this session; the menu reports the latter. */
      audioTrackIndex: null,
      targetAudioStreamIndex: 0,
      /* True while a quality change or an out-of-range seek is re-negotiating. */
      qualityBusy: false,
      /* Subtitle tracks as the server described them. Only an entry carrying a
         `url` can actually be delivered; the rest are image-based. */
      subtitles: [],
      /* "off", or the key of the selected deliverable track. */
      subtitleSelection: "off",
      /* Overlay chrome, kept per session so a new one starts visible. */
      controlsVisible: true,
      fullscreen: false,
      /* True when the browser refused to autostart: the stream is attached and
         ready, just waiting for a real gesture. */
      blockedAutoplay: false,
      /* Carries the viewer's subtitle choice across a re-negotiation. */
      subtitlePreference: null,
    };
  }

  function activeLibrary() {
    for (const library of state.libraries) {
      if (library.id === state.libraryId) return library;
    }
    return null;
  }

  function rememberEntity(entity) {
    if (entity && typeof entity.id === "string" && entity.id) {
      state.entityIndex.set(entity.id, entity);
    }
  }

  function parentIdOf(entity) {
    /* The Go model marshals parent_id as *string, so a real parent is a
       non-empty string and top-level entities arrive either without the key
       or — for older rows — with "". Treat both as "no parent". */
    if (!entity) return null;
    const id = entity.parent_id;
    return typeof id === "string" && id.length > 0 ? id : null;
  }

  function childCountOf(entity) {
    if (!entity || (entity.type !== "Series" && entity.type !== "Season")) return null;
    let count = 0;
    for (const candidate of state.entities) {
      if (parentIdOf(candidate) === entity.id) count += 1;
    }
    return count;
  }

  function displayTitle(entity) {
    if (!entity) return "Untitled";
    const metadata = entity.metadata;
    if (metadata && typeof metadata.title === "string" && metadata.title.trim()) {
      return metadata.title;
    }
    if (typeof entity.name === "string" && entity.name.trim()) return entity.name;
    return "Untitled";
  }

  function metadataOf(entity) {
    return (entity && entity.metadata) || null;
  }

  function extraOf(entity) {
    const metadata = metadataOf(entity);
    return (metadata && metadata.extra) || null;
  }

  function episodeLabel(entity) {
    const extra = extraOf(entity);
    if (!extra) return null;
    const season = extra.season;
    const episode = extra.episode;
    if (season && episode) return "S" + pad2(season) + "E" + pad2(episode);
    if (season) return "Season " + season;
    if (episode) return "Episode " + episode;
    return null;
  }

  function yearOf(entity) {
    const extra = extraOf(entity);
    if (extra && extra.year) return String(extra.year);
    return null;
  }

  /* ── 5. Artwork ──────────────────────────────────────────────────────── */

  /* Artwork is only ever loaded from this server's own proxy.
     poster_url/backdrop_url point at /api/images/..., which fetches the
     provider's image server-side and caches it; the provider therefore never
     learns who is watching, or from where. The metadata's own absolute URL is
     deliberately not used even when it is present - fetching image.tmdb.org
     from the browser hands a third party the viewer's address, and the policy
     the server sends (img-src 'self') would refuse it anyway. With no proxy
     configured, the deterministic gradient derived from the id stands in,
     which leaks nothing. */
  function remoteArt(entity) {
    if (!entity) return null;
    const candidate = entity.backdrop_url || entity.poster_url;
    if (typeof candidate === "string" && candidate) return candidate;
    return null;
  }

  function artVars(seed) {
    const hash = fnv1a(String(seed || "astraeus"));
    const hueA = hash % 360;
    const hueB = (hueA + 38 + ((hash >>> 8) % 132)) % 360;
    return { "--art-h1": String(hueA), "--art-h2": String(hueB) };
  }

  function monogram(entity) {
    const title = displayTitle(entity);
    for (let i = 0; i < title.length; i += 1) {
      const ch = title.charAt(i);
      if (/[\p{L}\p{N}]/u.test(ch)) return ch.toUpperCase();
    }
    return "★";
  }

  function artNode(entity, extraClass) {
    const node = el(
      "span",
      {
        class: "art" + (extraClass ? " " + extraClass : ""),
        style: artVars(entity && entity.id ? entity.id : displayTitle(entity)),
        "aria-hidden": "true",
      },
      el("span", { class: "art-mono", text: monogram(entity) })
    );
    const src = remoteArt(entity);
    if (src) {
      const img = el("img", {
        class: "art-img",
        src: src,
        alt: "",
        width: 1280,
        height: 720,
        loading: "lazy",
        decoding: "async",
      });
      img.addEventListener("error", function () {
        img.remove();
      });
      node.prepend(img);
    }
    return node;
  }

  function setAmbient(entity) {
    const seed = entity && entity.id ? entity.id : "astraeus";
    const hash = fnv1a(seed);
    document.documentElement.style.setProperty("--ambient-h", String(hash % 360));
  }

  /* ── 6. Shared UI pieces ─────────────────────────────────────────────── */

  function statusFlag(entity) {
    const incomplete = entity && entity.status === "Incomplete";
    return el("span", {
      class: "flag " + (incomplete ? "flag-incomplete" : "flag-complete"),
      text: incomplete ? "Needs metadata" : "Complete",
    });
  }

  function entityCard(entity, options) {
    const opts = options || {};
    const current = opts.current === true;
    const parent = parentIdOf(entity) ? state.entityIndex.get(parentIdOf(entity)) : null;
    const details = [];
    const episode = episodeLabel(entity);
    if (episode) details.push(episode);
    const year = yearOf(entity);
    if (year) details.push(year);
    const childCount = childCountOf(entity);
    if (childCount !== null) details.push(formatCount(childCount) + " " + plural(childCount, "item"));
    if (parent) details.push("in " + displayTitle(parent));

    return el(
      "button",
      {
        type: "button",
        class: "card",
        "data-action": "open-entity",
        "data-entity-id": entity.id,
        "data-focus-key": "card:" + entity.id,
        "aria-current": current ? "true" : null,
        "aria-label":
          displayTitle(entity) +
          ", " +
          entity.type +
          ", " +
          (entity.status === "Incomplete" ? "needs metadata" : "complete"),
      },
      [
        artNode(entity, "card-art"),
        el("span", { class: "card-body" }, [
          el("span", { class: "card-type", text: entity.type || "Entity" }),
          el("span", { class: "card-title", text: displayTitle(entity) }),
          details.length
            ? el("span", { class: "card-sub", text: details.join(" · ") })
            : null,
          el("span", { class: "card-flags" }, statusFlag(entity)),
        ]),
      ]
    );
  }

  function collectionNode(entities, options) {
    const opts = options || {};
    const list = el("ul", { class: "collection" });
    const currentNode = state.route && state.route.segs[0] === "entity" ? state.route.segs[1] : null;
    for (const entity of entities) {
      list.append(
        el(
          "li",
          null,
          entityCard(entity, { current: opts.current === true && entity.id === currentNode })
        )
      );
    }
    enableArrowNav(list, ".card");
    return list;
  }

  /* Left/right move through a collection without leaving the keyboard. */
  function enableArrowNav(container, selector) {
    container.addEventListener("keydown", function (event) {
      const keys = ["ArrowRight", "ArrowLeft", "Home", "End"];
      if (keys.indexOf(event.key) === -1) return;
      const items = Array.prototype.slice.call(container.querySelectorAll(selector));
      const active = document.activeElement;
      const index = items.indexOf(active);
      if (index === -1) return;
      let next = index;
      if (event.key === "ArrowRight") next = Math.min(items.length - 1, index + 1);
      if (event.key === "ArrowLeft") next = Math.max(0, index - 1);
      if (event.key === "Home") next = 0;
      if (event.key === "End") next = items.length - 1;
      if (next === index) return;
      event.preventDefault();
      items[next].focus();
    });
  }

  function canvasState(options) {
    const opts = options || {};
    const children = [
      opts.spinner
        ? el("span", { class: "spinner", "aria-hidden": "true" })
        : el("span", { class: "state-mark", "aria-hidden": "true" }),
      el("h1", { class: "state-title", text: opts.title || "" }),
    ];
    if (opts.body) children.push(el("p", { class: "state-body", text: opts.body }));
    if (opts.actions && opts.actions.length) {
      children.push(el("div", { class: "hero-actions" }, opts.actions));
    }
    return el("div", { class: "canvas-state" }, children);
  }

  function metaRow(term, value, valueClass) {
    if (value === null || value === undefined || value === "") return null;
    return el("div", { class: "meta-row" }, [
      el("dt", { text: term }),
      el("dd", { class: valueClass || null, text: String(value) }),
    ]);
  }

  function sectionNode(title, children) {
    return el("section", { class: "ctx-section" }, [
      el("h3", { class: "ctx-heading", text: title }),
    ].concat(children.filter(Boolean)));
  }

  /* ── 7. Rendering ────────────────────────────────────────────────────── */

  function render() {
    const restoreKey = focusKeyOf(document.activeElement);
    renderNav();
    renderBreadcrumbs();
    renderCanvas();
    renderContext();
    if (restoreKey) restoreFocus(restoreKey);
  }

  function focusKeyOf(node) {
    if (!node || !node.dataset || typeof node.dataset.focusKey !== "string") return null;
    return node.dataset.focusKey;
  }

  function restoreFocus(key) {
    let next = null;
    try {
      next = document.querySelector('[data-focus-key="' + CSS.escape(key) + '"]');
    } catch (error) {
      next = null;
    }
    if (next && next !== document.activeElement) {
      next.focus({ preventScroll: true });
    }
  }

  /* After a user-driven navigation, hand focus to the canvas so keyboard and
     screen-reader users land inside the new view instead of on <body>. */
  function applyFocusTarget() {
    if (state.focusTarget !== "canvas") return;
    state.focusTarget = null;
    dom.canvas.focus({ preventScroll: true });
  }

  /* 7a. Left sidebar ------------------------------------------------------ */

  function renderNav() {
    const library = activeLibrary();
    clear(dom.libraryList);
    dom.navEmpty.hidden = state.libraries.length > 0;

    for (const item of state.libraries) {
      const selected = item.id === state.libraryId;
      const scanning = state.busyAction === "scan:" + item.id;
      dom.libraryList.append(
        el("li", { class: "library-item" + (selected ? " is-selected" : "") }, [
          el(
            "a",
            {
              class: "library-link",
              href: hashFor("library", item.id),
              "data-focus-key": "lib:" + item.id,
              "aria-current": selected ? "true" : null,
            },
            [
              el("span", { class: "library-name", text: item.name || "Untitled library" }),
              el("span", { class: "library-kind", text: kindLabel(item.kind) }),
              el("span", {
                class: "library-path",
                text: item.path || "",
                title: item.path || "",
              }),
            ]
          ),
          el("button", {
            type: "button",
            class: "btn btn-small btn-quiet",
            "data-action": "scan",
            "data-library-id": item.id,
            "data-focus-key": "scan:" + item.id,
            disabled: scanning,
            "aria-label": "Scan library " + (item.name || "Untitled library"),
            text: scanning ? "Scanning…" : "Scan",
          }),
        ])
      );
    }

    const total = state.entities.length;
    let incomplete = 0;
    for (const entity of state.entities) {
      if (entity.status === "Incomplete") incomplete += 1;
    }

    if (state.librariesFailed) {
      dom.navSummary.textContent = "Could not load libraries";
    } else if (library && state.entitiesFailed) {
      /* "0 entities" would be a claim we cannot make. */
      dom.navSummary.textContent = kindLabel(library.kind) + " · entities unavailable";
    } else if (library) {
      dom.navSummary.textContent =
        kindLabel(library.kind) +
        " · " +
        formatCount(total) +
        " " +
        plural(total, "entity", "entities") +
        " · " +
        formatCount(incomplete) +
        " incomplete";
    } else {
      dom.navSummary.textContent = state.booted ? "No library selected" : "Loading…";
    }

    dom.incompleteCount.textContent = formatCount(incomplete);
    dom.incompleteCount.dataset.empty = incomplete === 0 ? "true" : "false";
    dom.incompleteDesc.textContent =
      incomplete === 0
        ? ", nothing needs attention"
        : ", " + formatCount(incomplete) + " " + plural(incomplete, "entity", "entities") + " need metadata";
    dom.filterToggle.setAttribute("aria-pressed", state.filterIncomplete ? "true" : "false");
    dom.enrichButton.disabled = state.busyAction === "enrich";
    dom.enrichButton.textContent = state.busyAction === "enrich" ? "Enriching…" : "Enrich metadata";

    renderContinueWatching();
  }

  /**
   * Paint the continue-watching rail.
   *
   * Rows reuse the library-list classes so the two sections read as the same
   * kind of thing, and each row is a real anchor into the router, exactly like
   * a library row. State is the only source: a refresh that fails leaves the
   * section hidden rather than showing anything half-read.
   */
  function renderContinueWatching() {
    /* This list repaints on its own, from a background refresh. Rebuilding it
       must not drop the reader's place if their focus was inside a row. */
    const restoreKey = focusKeyOf(document.activeElement);
    clear(dom.continueList);
    dom.continueSection.hidden = state.continueWatching.length === 0;

    for (const entry of state.continueWatching) {
      const entity = entry.entity;
      const progress = entry.progress || {};
      /* Position and duration are measured against the media file, so an
         absent duration means the readout cannot claim to know the end. */
      const duration = Number(progress.duration_seconds);
      const position = Number(progress.position_seconds);
      const readout =
        isFinite(duration) && duration > 0
          ? formatClock(position) + " of " + formatClock(duration)
          : formatClock(position);
      const kind = typeof entity.type === "string" && entity.type ? entity.type : "Entity";

      dom.continueList.append(
        el("li", { class: "library-item" }, [
          el(
            "a",
            {
              class: "library-link",
              href: hashFor("entity", entity.id),
              "data-focus-key": "continue:" + entity.id,
              title: displayTitle(entity),
            },
            [
              el("span", { class: "library-name", text: displayTitle(entity) }),
              el("span", { class: "continue-meta", text: kind + " · " + readout }),
            ]
          ),
        ])
      );
    }

    if (restoreKey) restoreFocus(restoreKey);
  }

  function kindLabel(kind) {
    if (kind === "movies") return "Movies";
    if (kind === "shows") return "Shows";
    return kind ? String(kind) : "Unknown";
  }

  /* 7b. Breadcrumbs ------------------------------------------------------- */

  function breadcrumbTrail() {
    const trail = [];
    const library = activeLibrary();
    if (library) {
      trail.push({
        kind: "library",
        id: library.id,
        label: library.name || "Untitled library",
        type: "Library",
      });
    }
    if (!state.route || state.route.segs[0] !== "entity") return trail;

    const chain = [];
    let current =
      state.entityIndex.get(state.route.segs[1]) ||
      (state.detail ? state.detail.entity : null);
    let guard = 0;
    while (current && guard < 24) {
      guard += 1;
      chain.unshift({
        kind: "entity",
        id: current.id,
        label: displayTitle(current),
        type: current.type || "Entity",
      });
      const parentId = parentIdOf(current);
      if (!parentId) break;
      let parent = state.entityIndex.get(parentId);
      if (!parent && state.detail && state.detail.parent && state.detail.parent.id === parentId) {
        parent = state.detail.parent;
      }
      current = parent;
    }
    return trail.concat(chain);
  }

  function hashFor(kind, id) {
    return "#/" + kind + "/" + encodeURIComponent(id);
  }

  function renderBreadcrumbs() {
    clear(dom.breadcrumbs);
    const trail = breadcrumbTrail();
    if (!trail.length) {
      dom.breadcrumbs.append(
        el("li", { class: "crumb" }, el("span", { class: "crumb-current", text: "Astraeus" }))
      );
      return;
    }
    trail.forEach(function (node, index) {
      const last = index === trail.length - 1;
      const li = el("li", { class: "crumb" });
      if (last) {
        li.append(
          el("span", {
            class: "crumb-current",
            "aria-current": "page",
            title: node.type,
            text: node.label,
          })
        );
      } else {
        li.append(
          el("a", {
            class: "crumb-link",
            href: hashFor(node.kind, node.id),
            "data-focus-key": "crumb:" + node.kind + ":" + node.id,
            title: "Go to " + node.type + ": " + node.label,
            text: node.label,
          })
        );
      }
      dom.breadcrumbs.append(li);
    });
  }

  /* 7c. Canvas ------------------------------------------------------------ */

  function renderCanvas() {
    clear(dom.canvasContent);
    dom.canvas.setAttribute("aria-busy", state.loading ? "true" : "false");

    if (!state.booted) {
      dom.canvasContent.append(canvasState({ spinner: true, title: "Loading…" }));
      return;
    }

    if (state.libraries.length === 0) {
      /* "We could not ask" and "there are none" look identical in the data, so
         they have to be told apart by how we got here. */
      dom.canvasContent.append(
        state.librariesFailed
          ? canvasState({
              title: "Could not load libraries",
              body:
                "The Astraeus API could not be reached, so the library list is unknown. " +
                "This is a loading failure, not an empty server.",
              actions: [
                el("button", {
                  type: "button",
                  class: "btn btn-accent",
                  "data-action": "retry-boot",
                  text: "Retry",
                }),
              ],
            })
          : canvasState({
              title: "No libraries yet",
              body:
                "Astraeus has no libraries registered. Create one with POST /api/libraries, " +
                "then reload this page to browse it.",
            })
      );
      return;
    }

    const route = state.route;
    if (!route) {
      dom.canvasContent.append(
        canvasState({
          title: "Pick a library",
          body: "Choose a library from the left to start browsing your media.",
        })
      );
      return;
    }

    if (route.segs[0] === "library") {
      renderLibraryCanvas();
      return;
    }

    if (route.segs[0] === "entity") {
      if (state.detail) {
        const type = state.detail.entity.type;
        if (type === "Movie" || type === "Episode") {
          renderLeafCanvas(state.detail);
        } else {
          renderContainerCanvas(state.detail);
        }
      } else if (state.loading) {
        dom.canvasContent.append(canvasState({ spinner: true, title: "Loading…" }));
      } else {
        dom.canvasContent.append(
          canvasState({
            title: "Could not load this entity",
            body: "The selection could not be read from the API. See the message above.",
            actions: [
              el("button", {
                type: "button",
                class: "btn btn-accent",
                "data-action": "retry",
                text: "Retry",
              }),
            ],
          })
        );
      }
    }
  }

  function libraryEntitiesForDisplay() {
    const all = state.entities;
    if (state.filterIncomplete) {
      return all.filter(function (entity) {
        return entity.status === "Incomplete";
      });
    }
    return all.filter(function (entity) {
      return !parentIdOf(entity);
    });
  }

  function renderLibraryCanvas() {
    const library = activeLibrary();
    if (!library) {
      dom.canvasContent.append(
        canvasState({
          title: "Library not found",
          body: "That library is not in the current list. It may have been deleted.",
        })
      );
      return;
    }

    const shown = libraryEntitiesForDisplay();
    let incomplete = 0;
    for (const entity of state.entities) {
      if (entity.status === "Incomplete") incomplete += 1;
    }

    const head = el("header", { class: "browse-head" }, [
      el("div", { class: "browse-head-text" }, [
        el("p", {
          class: "eyebrow",
          text: "Library · " + kindLabel(library.kind),
        }),
        el("h1", { class: "browse-title", text: library.name || "Untitled library" }),
        el("p", {
          class: "browse-sub",
          text: state.entitiesFailed
            ? library.path || "No path recorded"
            : (library.path || "No path recorded") +
              " · " +
              formatCount(state.entities.length) +
              " " +
              plural(state.entities.length, "entity", "entities") +
              " · " +
              formatCount(incomplete) +
              " incomplete",
        }),
      ]),
    ]);

    const browse = el("div", { class: "browse" }, [head]);

    if (state.loading && state.entities.length === 0) {
      browse.append(el("p", { class: "browse-sub", text: "Loading entities…" }));
    } else if (state.entitiesFailed) {
      /* The list never arrived. Saying "empty" here would tell the user their
         media is gone and offer to re-scan a library that is perfectly fine. */
      browse.append(
        canvasState({
          title: "Could not load this library's entities",
          body:
            "The request for the contents of “" +
            (library.name || "this library") +
            "” failed, so this list is unknown — not necessarily empty. The library itself is untouched.",
          actions: [
            el("button", {
              type: "button",
              class: "btn btn-accent",
              "data-action": "retry-route",
              text: "Retry",
            }),
          ],
        })
      );
    } else if (state.entities.length === 0) {
      browse.append(
        canvasState({
          title: "This library is empty",
          body: "Nothing has been imported from " + (library.path || "this path") + " yet. Run a scan to look for media.",
          actions: [
            el("button", {
              type: "button",
              class: "btn btn-accent",
              "data-action": "scan",
              "data-library-id": library.id,
              text: "Scan library",
            }),
          ],
        })
      );
    } else if (shown.length === 0) {
      browse.append(
        canvasState({
          title: "Nothing needs attention",
          body: "Every entity in this library already has metadata. Turn off the incomplete filter to see everything.",
          actions: [
            el("button", {
              type: "button",
              class: "btn",
              "data-action": "toggle-filter",
              text: "Show all",
            }),
          ],
        })
      );
    } else {
      if (state.filterIncomplete) {
        browse.append(
          el("p", {
            class: "browse-sub",
            text:
              "Showing every entity that still needs metadata, at any depth of the hierarchy.",
          })
        );
      }
      browse.append(collectionNode(shown, { current: false }));
    }

    dom.canvasContent.append(browse);
  }

  /* The canvas's own Play affordance, distinct from the player's toggle: while
     the player is up this one is inside the inert underlay. */
  function heroPlayButton(entity, detail) {
    const objects = Array.isArray(detail.objects) ? detail.objects : [];
    const button = el("button", {
      type: "button",
      class: "btn btn-primary",
      "data-action": "play",
      "data-focus-key": "play-hero",
      disabled: objects.length === 0,
      "aria-label": "Play " + displayTitle(entity),
    });
    setIconButton(button, "play", "Play");
    return button;
  }

  function renderLeafCanvas(detail) {
    const entity = detail.entity;
    const metadata = metadataOf(entity);
    const hero = el("article", { class: "hero" }, [
      artNode(entity, "hero-art"),
      el("div", { class: "hero-scrim", "aria-hidden": "true" }),
    ]);

    const titleId = "canvas-title";
    const body = el("div", { class: "hero-body" }, [
      el("p", { class: "eyebrow" }, [
        el("span", { text: entity.type || "Entity" }),
        statusFlag(entity),
        episodeLabel(entity) ? el("span", { text: episodeLabel(entity) }) : null,
        yearOf(entity) ? el("span", { text: yearOf(entity) }) : null,
      ]),
      el("h1", { class: "hero-title", id: titleId, text: displayTitle(entity) }),
      el("p", {
        class: "hero-desc",
        text:
          (metadata && metadata.description) ||
          "No description has been attached to this entity yet. Enrich metadata to fill this in.",
      }),
      el("div", { class: "hero-actions" }, [
        heroPlayButton(entity, detail),
      ]),
    ]);

    hero.append(body);
    dom.canvasContent.append(hero);
  }

  function renderContainerCanvas(detail) {
    const entity = detail.entity;
    const metadata = metadataOf(entity);
    const children = Array.isArray(detail.children) ? detail.children : [];
    const shown = state.filterIncomplete
      ? children.filter(function (child) {
          return child.status === "Incomplete";
        })
      : children;

    const head = el("header", { class: "browse-head" }, [
      artNode(entity, "browse-head-art"),
      el("div", { class: "browse-head-text" }, [
        el("p", { class: "eyebrow" }, [
          el("span", { text: entity.type || "Entity" }),
          statusFlag(entity),
        ]),
        el("h1", { class: "browse-title", text: displayTitle(entity) }),
        el("p", {
          class: "browse-sub",
          text:
            formatCount(children.length) +
            " " +
            plural(children.length, "child", "children") +
            (state.filterIncomplete ? " · filtered to incomplete" : ""),
        }),
        metadata && metadata.description
          ? el("p", { class: "browse-desc", text: metadata.description })
          : null,
      ]),
    ]);

    const browse = el("div", { class: "browse" }, [head]);
    if (children.length === 0) {
      browse.append(
        canvasState({
          title: "Nothing inside yet",
          body: "This " + String(entity.type || "entity").toLowerCase() + " has no children recorded.",
        })
      );
    } else if (shown.length === 0) {
      browse.append(
        canvasState({
          title: "Nothing needs attention",
          body: "All children of this entity already have metadata.",
          actions: [
            el("button", {
              type: "button",
              class: "btn",
              "data-action": "toggle-filter",
              text: "Show all",
            }),
          ],
        })
      );
    } else {
      browse.append(collectionNode(shown, { current: true }));
    }
    dom.canvasContent.append(browse);
  }

  /* 7d. Context panel ----------------------------------------------------- */

  function renderContext() {
    clear(dom.contextBody);
    const route = state.route;

    if (route && route.segs[0] === "entity") {
      if (!state.detail) {
        dom.contextSub.textContent = "Loading…";
        dom.contextBody.append(el("p", { class: "muted", text: "Loading…" }));
        return;
      }
      const entity = state.detail.entity;
      dom.contextSub.textContent = (entity.type || "Entity") + " · " + displayTitle(entity);
      renderEntityContext(state.detail);
      return;
    }

    if (route && route.segs[0] === "library") {
      const library = activeLibrary();
      dom.contextSub.textContent = library ? library.name || "Untitled library" : "Library";
      renderLibraryContext(library);
      return;
    }

    dom.contextSub.textContent = "Nothing selected";
    dom.contextBody.append(
      el("p", {
        class: "muted small",
        text: "Select a movie, episode, series or season to inspect it here.",
      })
    );
  }

  function renderLibraryContext(library) {
    if (!library) {
      dom.contextBody.append(el("p", { class: "muted small", text: "Library unavailable." }));
      return;
    }
    let incomplete = 0;
    for (const entity of state.entities) {
      if (entity.status === "Incomplete") incomplete += 1;
    }

    dom.contextBody.append(
      sectionNode("Library", [
        el("dl", { class: "meta-grid" }, [
          metaRow("Name", library.name || "Untitled library"),
          metaRow("Kind", kindLabel(library.kind)),
          metaRow("Path", library.path || "—", "mono"),
          metaRow("Created", formatDate(library.created_at)),
          metaRow("Entities", formatCount(state.entities.length), "num"),
          metaRow("Incomplete", formatCount(incomplete), "num"),
          metaRow("ID", library.id, "mono"),
        ]),
      ])
    );

    const top = state.entities.filter(function (entity) {
      return !parentIdOf(entity);
    });
    dom.contextBody.append(
      sectionNode(
        "Top-level entities",
        top.length
          ? [
              el(
                "ul",
                { class: "stack-list" },
                top.map(function (entity) {
                  return el("li", null, el("div", { class: "stack-item" }, [
                    el("span", { class: "stack-text", text: displayTitle(entity) }),
                    el("span", { class: "stack-index", text: entity.type || "" }),
                  ]));
                })
              ),
            ]
          : [el("p", { class: "muted small", text: "No entities recorded for this library yet." })]
      )
    );

    dom.contextBody.append(
      sectionNode("Actions", [
        el("button", {
          type: "button",
          class: "btn btn-block",
          "data-action": "scan",
          "data-library-id": library.id,
          "data-focus-key": "scan-context",
          disabled: state.busyAction === "scan:" + library.id,
          text: state.busyAction === "scan:" + library.id ? "Scanning…" : "Scan library",
        }),
      ])
    );
  }

  function renderEntityContext(detail) {
    const entity = detail.entity;
    const metadata = metadataOf(entity);
    const objects = Array.isArray(detail.objects) ? detail.objects : [];
    const children = Array.isArray(detail.children) ? detail.children : [];
    const parent = detail.parent || null;
    const isLeaf = entity.type === "Movie" || entity.type === "Episode";

    dom.contextBody.append(
      sectionNode("Metadata", [
        el("dl", { class: "meta-grid" }, [
          metaRow("Title", displayTitle(entity)),
          metaRow("Type", entity.type || "—"),
          metaRow("Status", entity.status || "—"),
          episodeLabel(entity) ? metaRow("Season / Episode", episodeLabel(entity)) : null,
          yearOf(entity) ? metaRow("Year", yearOf(entity)) : null,
          metaRow("Provider", metadata && metadata.provider ? metadata.provider : "None", null),
          parent ? metaRow("Parent", displayTitle(parent)) : null,
          metaRow("Updated", formatDate(entity.updated_at)),
          metaRow("ID", entity.id, "mono"),
        ]),
        el("p", {
          class: "muted small",
          text:
            (metadata && metadata.description) ||
            "No description recorded. This entity has not been enriched yet.",
        }),
      ])
    );

    dom.contextBody.append(renderPlaybackSection(entity, objects, isLeaf));
    dom.contextBody.append(renderQueueSection(entity, objects, children, isLeaf));
    dom.contextBody.append(renderFilesSection(objects));
  }

  function isLeafType(type) {
    return type === "Movie" || type === "Episode";
  }

  function modeLabel(mode) {
    if (mode === "direct_play") return "Direct play";
    if (mode === "remux") return "Remux → HLS";
    if (mode === "transcode") return "Transcode → HLS";
    return "Not negotiated";
  }

  function modeClass(mode) {
    if (mode === "direct_play" || mode === "remux" || mode === "transcode") {
      return "mode-" + mode;
    }
    return "mode-unknown";
  }

  /* ── Subtitle selector ───────────────────────────────────────────────── */

  function subtitleOptionId(key) {
    return "subtitle-opt-" + String(key).replace(/[^A-Za-z0-9_-]/g, "-");
  }

  /* Native radios give us the group semantics, single tab stop, arrow-key
     traversal and "checked" announcement for free; the styling is purely
     visual so nothing about the control becomes custom or unreachable. */
  function subtitleOptionNode(opts) {
    const id = subtitleOptionId(opts.key);
    const input = el("input", {
      type: "radio",
      name: "subtitle-track",
      id: id,
      value: String(opts.key),
      checked: opts.checked === true,
      disabled: opts.disabled === true,
      "data-action": "select-subtitle",
      "data-subtitle-key": String(opts.key),
      "data-focus-key": "subtitle:" + String(opts.key),
    });
    return el("label", { class: "subtitle-option", for: id }, [
      input,
      el("span", { text: opts.label }),
    ]);
  }

  function subtitleSelectorNode(pb, live) {
    const list = Array.isArray(pb.subtitles) ? pb.subtitles : [];
    if (!list.length) return null;

    const options = [
      subtitleOptionNode({
        key: "off",
        label: "Off",
        checked: pb.subtitleSelection === "off",
        disabled: false,
      }),
    ];

    for (const sub of list) {
      const key = subtitleKey(sub);
      /* An image track with no URL has no way to reach a <track>, so the only
         way it can be shown is burned into the picture. Offer it and say so in
         the label: the choice is real, it just costs a server-side re-encode
         rather than an instant toggle. An image track the server has read into
         text carries a URL and is offered like any other.

         A track that can be neither delivered nor burned is listed as disabled
         and says why. Offering it as a working control meant choosing it
         repainted the radio and then did nothing, which reads as a broken
         player (W-6 of the 2026-10-09 review). */
      const selectable = subtitleSelectable(sub);
      let label = subtitleLabel(sub);
      if (!selectable) {
        label += " (unavailable)";
      } else if (subtitleNeedsBurn(sub)) {
        label += " (burned in)";
      }
      options.push(
        subtitleOptionNode({
          key: key,
          label: label,
          /* A track that cannot be chosen is not shown as chosen, even if the
             stored preference names it - a checked radio that does nothing is
             the bug. */
          checked: selectable && pb.subtitleSelection === key,
          disabled: !live || !selectable,
        })
      );
    }

    return el("fieldset", { class: "subtitle-group" }, [
      el("legend", { class: "ctx-heading", text: "Subtitles" }),
      el("div", { class: "subtitle-options" }, options),
    ]);
  }

  function renderPlaybackSection(entity, objects, isLeaf) {
    const pb = state.playback;
    const hasObjects = Array.isArray(objects) && objects.length > 0;

    /* A negotiation belongs to one entity only. The controls themselves live
       on the player overlay (see buildPlayerOverlay); this panel is purely
       informational: what was decided, and what is happening. */
    const isCurrent = pb.entityId === entity.id;
    const status = isCurrent ? pb.status : "idle";
    const playable = isLeafType(entity.type) && hasObjects;
    const live = isCurrent && playbackIsLive(pb);
    const growing = live && isEventPlaylistPlayback();
    /* Two clocks meet here. The element and `seekableBounds()` speak MEDIA
       time, which restarts at zero for each session; the viewer sees SOURCE
       time, where source = media + sessionStart. Anything shown must go
       through these helpers, or a session resumed an hour in reports the
       produced part as if it began at the opening titles. */
    const sourceTotal = isCurrent ? sourceDurationOf(pb) : 0;
    const produced = live ? producedWindowOf(pb) : null;
    const producedTo = produced ? produced.to : 0;
    const durationText =
      growing && producedTo > 0
        ? "produced " + formatClock(producedTo)
        : sourceTotal > 0
        ? formatClock(sourceTotal)
        : "";

    const children = [];

    if (isCurrent && pb.mode) {
      children.push(
        el("div", { class: "delivery-facts" }, [
          el("span", { class: "mode-badge " + modeClass(pb.mode), text: modeLabel(pb.mode) }),
          pb.mediaInfo && pb.mediaInfo.container
            ? el("span", { class: "chip", text: String(pb.mediaInfo.container) })
            : null,
          pb.mediaInfo && pb.mediaInfo.video_codec
            ? el("span", { class: "chip", text: String(pb.mediaInfo.video_codec) })
            : null,
          pb.mediaInfo && pb.mediaInfo.audio_codec
            ? el("span", { class: "chip", text: String(pb.mediaInfo.audio_codec) })
            : null,
          pb.mediaInfo && pb.mediaInfo.width && pb.mediaInfo.height
            ? el("span", {
                class: "chip",
                text: pb.mediaInfo.width + "×" + pb.mediaInfo.height,
              })
            : null,
          durationText ? el("span", { class: "chip", text: durationText }) : null,
          /* What the server actually chose, when it is not the source height. */
          pb.targetHeight > 0 && pb.targetHeight !== sourceHeightOf(pb)
            ? el("span", { class: "chip", text: "playing at " + pb.targetHeight + "p" })
            : null,
        ])
      );
    }

    /* Say plainly how far ahead this session can jump without re-buffering.
       Same source-time basis as the readout `syncTransport` maintains, so the
       two never disagree. */
    if (growing && producedTo > 0) {
      children.push(
        el("p", { class: "delivery-facts" }, [
          el("span", {
            class: "chip chip-live",
            id: "player-window",
            text: "Produced to " + formatClock(producedTo),
          }),
        ])
      );
    }

    if (isCurrent && pb.decision && Array.isArray(pb.decision.reasons) && pb.decision.reasons.length) {
      children.push(
        el(
          "ul",
          { class: "reasons" },
          pb.decision.reasons.map(function (reason) {
            return el("li", null, [
              icon("arrow-right"),
              el("span", { text: String(reason) }),
            ]);
          })
        )
      );
    }

    children.push(el("p", { class: "playback-note", id: "playback-note", "data-state": noteState(status, playable), text: noteText(entity, status, playable, pb) }));
    children.push(
      el("p", {
        class: "muted small",
        text: hasObjects
          ? basename(objects[0].file_path) + " · " + (objects[0].mime_type || "unknown type")
          : "No media object attached.",
      })
    );

    return sectionNode("Playback", children);
  }

  function noteState(status, playable) {
    if (status === "segmented") return "warn";
    if (status === "error") return "alert";
    if (status === "ready" || status === "playing" || status === "paused") return "ok";
    return playable ? "info" : "warn";
  }

  function noteText(entity, status, playable, pb) {
    if (!isLeafType(entity.type)) {
      return (
        "This is a " +
        String(entity.type || "container").toLowerCase() +
        ", which holds children but has no media file of its own — so there is nothing to play. " +
        "Select a movie or an episode instead."
      );
    }
    if (!playable) {
      return "No media file is recorded for this entity, so there is nothing to play.";
    }
    if (status === "loading") {
      return pb && pb.segmented
        ? "Preparing segmented delivery…"
        : "Negotiating delivery with the server…";
    }
    if (status === "error") {
      return pb && pb.error ? pb.error : "Playback failed.";
    }
    if (status === "segmented") return segmentedMessage(pb, pb.segmentedCause);
    if (status === "ready" || status === "playing" || status === "paused") {
      if (pb.blockedAutoplay) {
        return "The browser would not start this stream on its own — press Play to begin. The session is ready; nothing has failed.";
      }
      if (pb.mode === "direct_play") {
        return "Direct play: the server is sending the original file over HTTP range requests, so seeking is exact.";
      }
      const engine = engineLabelFor(pb.engine);
      return (
        "Segmented delivery via " +
        engine +
        ". The server runs ffmpeg when playback starts and produces segments in order, so this" +
        " stream is generated while it plays. Seeking within what has already been produced is" +
        " instant; seeking further ahead restarts ffmpeg at that point, which takes a moment" +
        " but reaches anywhere in the film."
      );
    }
    return (
      "Press Play to negotiate delivery. The server picks direct play when the file is already " +
      "browser-compatible, and remux or transcode otherwise."
    );
  }

  function renderQueueSection(entity, objects, children, isLeaf) {
    if (isLeaf) {
      if (!objects.length) {
        return sectionNode("Queue", [
          el("p", { class: "muted small", text: "Nothing queued — no media objects are recorded." }),
        ]);
      }
      return sectionNode(
        "Queue",
        [
          el(
            "ul",
            { class: "stack-list" },
            objects.map(function (object, index) {
              return el("li", null, el("div", { class: "stack-item" }, [
                el("span", { class: "stack-index", text: String(index + 1).padStart(2, "0") }),
                el("span", { class: "stack-text" }, [
                  el("span", { class: "queue-title", text: basename(object.file_path) }),
                  el("span", {
                    class: "card-sub",
                    text: formatBytes(object.size) + " · " + (object.mime_type || "unknown type"),
                  }),
                ]),
              ]));
            })
          ),
        ]
      );
    }

    if (!children.length) {
      return sectionNode("Children", [
        el("p", { class: "muted small", text: "This container has no children recorded." }),
      ]);
    }

    return sectionNode(
      "Children · " + formatCount(children.length),
      [
        el(
          "ul",
          { class: "stack-list" },
          children.map(function (child) {
            const current = state.route && state.route.segs[1] === child.id;
            return el(
              "li",
              null,
              el(
                "button",
                {
                  type: "button",
                  class: "queue-item",
                  "data-action": "open-entity",
                  "data-entity-id": child.id,
                  "data-focus-key": "queue:" + child.id,
                  "aria-current": current ? "true" : null,
                },
                [
                  el("span", { class: "queue-kind", text: child.type || "Entity" }),
                  el("span", { class: "queue-title", text: displayTitle(child) }),
                  child.status === "Incomplete"
                    ? el("span", { class: "flag flag-incomplete", text: "Needs metadata" })
                    : null,
                ]
              )
            );
          })
        ),
      ]
    );
  }

  function renderFilesSection(objects) {
    if (!objects.length) {
      return sectionNode("Files", [
        el("p", { class: "muted small", text: "No media objects are recorded for this entity." }),
      ]);
    }
    return sectionNode(
      "Files · " + formatCount(objects.length),
      objects.map(function (object) {
        return el("div", { class: "file-card" }, [
          el("p", { class: "file-path", text: object.file_path || "—" }),
          el("div", { class: "file-tags" }, [
            el("span", { class: "chip", text: formatBytes(object.size) }),
            el("span", { class: "chip", text: object.mime_type || "unknown type" }),
            el("span", { class: "chip", text: formatDate(object.created_at) }),
          ]),
        ]);
      })
    );
  }

  /* ── 8. Feedback: banner, toasts, status line ────────────────────────── */

  function showError(message, retry) {
    state.retry = typeof retry === "function" ? retry : null;
    dom.errorText.textContent = message;
    dom.errorRetry.hidden = !state.retry;
    dom.errorBanner.hidden = false;
  }

  function clearError() {
    state.retry = null;
    dom.errorBanner.hidden = true;
    dom.errorText.textContent = "";
    dom.errorRetry.hidden = true;
  }

  function toast(message, kind) {
    const node = el("div", {
      class: "toast toast-" + (kind || "info"),
      text: message,
    });
    dom.toasts.append(node);
    const lifetime = kind === "error" ? 11000 : 7000;
    setTimeout(function () {
      node.classList.add("toast-out");
      setTimeout(function () {
        node.remove();
      }, 260);
    }, lifetime);
  }

  function setActionStatus(message, kind) {
    dom.actionStatus.textContent = message || "";
    if (kind) {
      dom.actionStatus.dataset.state = kind;
    } else {
      delete dom.actionStatus.dataset.state;
    }
  }

  function scanSummary(result) {
    const info = result && typeof result === "object" ? result : {};
    const parts = [
      formatCount(info.files_seen) + " files seen",
      formatCount(info.entities_created) + " created",
      formatCount(info.entities_reused) + " reused",
      formatCount(info.objects_created) + " objects added",
      formatCount(info.objects_updated) + " objects updated",
    ];
    const warnings = Array.isArray(info.warnings) ? info.warnings : [];
    return (
      "Scan complete — " +
      parts.join(", ") +
      (warnings.length
        ? ". " + formatCount(warnings.length) + " " + plural(warnings.length, "warning", "warnings") + "."
        : ".")
    );
  }

  function enrichSummary(result) {
    const info = result && typeof result === "object" ? result : {};
    return (
      "Enrichment complete — " +
      formatCount(info.processed) +
      " processed, " +
      formatCount(info.enriched) +
      " enriched, " +
      formatCount(info.failed) +
      " failed."
    );
  }

  /* ── 9. Actions ──────────────────────────────────────────────────────── */


  /** How often a job's state is polled while it runs. */
  const JOB_POLL_MS = 1000;

  /**
   * Wait for a job the server accepted and return its result, or throw.
   *
   * The decision - how long to wait, what a finished job with an error means,
   * what an inline answer means - is `core.js`'s awaitJob, which is unit-tested.
   * This only supplies the browser's clock and the API call.
   */
  async function awaitJob(accepted, onProgress) {
    const outcome = await awaitJobOutcome(accepted, {
      intervalMs: JOB_POLL_MS,
      sleep: function (ms) {
        return new Promise(function (resolve) {
          setTimeout(resolve, ms);
        });
      },
      status: function (id) {
        return api.job(id);
      },
      onProgress: onProgress,
    });

    if (outcome.outcome === "timeout") {
      throw new ApiError(
        "The server is still working on this after " +
          Math.round(awaitJobWaitMinutes()) +
          " minutes. It has not been cancelled; reload the page to see the result.",
        { code: "job_timeout" }
      );
    }
    if (outcome.outcome === "failed") {
      throw new ApiError(outcome.error, { code: "job_failed" });
    }
    return outcome.value;
  }

  function awaitJobWaitMinutes() {
    return awaitJobWaitMs / 60000;
  }

  async function doScan(libraryId) {
    if (state.busyAction) return;
    const library = state.libraries.find(function (item) {
      return item.id === libraryId;
    });
    const label = library ? library.name || "library" : "library";
    state.busyAction = "scan:" + libraryId;
    clearError();
    render();
    try {
      /* The scan is accepted rather than performed, so the caller waits for it
         and refreshes the list as it goes: on a large library that is the
         difference between a spinner and watching the titles appear. */
      const accepted = await api.scan(libraryId);
      const result = await awaitJob(accepted, function () {
        if (state.libraryId === libraryId) {
          refreshEntities(libraryId).catch(function () {
            /* A refresh that fails mid-scan is not the scan failing; the
               completed pass below refreshes again. */
          });
        }
      });
      if (state.libraryId === libraryId) {
        await refreshEntities(libraryId);
      }
      const summary = scanSummary(result);
      setActionStatus(summary, "ok");
      toast(summary, "success");
      const warnings = result && Array.isArray(result.warnings) ? result.warnings : [];
      if (warnings.length) {
        toast(
          "Scan warning: " + String(warnings[0]).slice(0, 220),
          "warn"
        );
      }
    } catch (error) {
      const message = "Scanning “" + label + "” failed. " + error.message;
      setActionStatus(message, "error");
      showError(message, function () {
        doScan(libraryId);
      });
    } finally {
      state.busyAction = null;
      render();
    }
  }

  async function doEnrich() {
    if (state.busyAction) return;
    state.busyAction = "enrich";
    clearError();
    render();
    try {
      const accepted = await api.enrich();
      const result = await awaitJob(accepted, function (status) {
        /* Enrichment is per entity, so the count is the useful progress. */
        if (typeof status.result !== "undefined") return;
        setActionStatus("Enriching…", "ok");
      });
      if (state.libraryId) await refreshEntities(state.libraryId);
      if (state.detail) await loadEntity(state.detail.entity.id, currentToken());
      const summary = enrichSummary(result);
      setActionStatus(summary, "ok");
      toast(summary, "success");
    } catch (error) {
      const message = "Enrichment failed. " + error.message;
      setActionStatus(message, "error");
      showError(message, function () {
        doEnrich();
      });
    } finally {
      state.busyAction = null;
      render();
    }
  }

  /* ── 9b. Player ──────────────────────────────────────────────────────── */

  /* The <video> lives in #player-layer, which renderCanvas() never clears, so
     a repaint (filter toggle, toast, metadata refresh) cannot interrupt it. */
  const playerVideo = document.createElement("video");
  playerVideo.className = "player-video";
  playerVideo.setAttribute("playsinline", "");
  playerVideo.setAttribute("preload", "metadata");
  playerVideo.setAttribute("aria-label", "Media player");
  playerVideo.addEventListener("loadedmetadata", function () {
    if (!state.playback.url) return;
    const pb = state.playback;
    /* Direct play resumes here: the server ignored the offset because it
       serves the whole file, so the seek waits until the element has a
       timeline. Cleared before the seek, so a further event cannot re-apply
       it, and the element's own clock stays the source clock (sessionStart
       is 0 on this path). */
    if (pb.resumeAt > 0) {
      const target = pb.resumeAt;
      pb.resumeAt = 0;
      try {
        playerVideo.currentTime = target;
      } catch (error) {
        /* A source that will not seek simply plays from the beginning. */
      }
    }
    pb.duration = isFinite(playerVideo.duration) ? playerVideo.duration : 0;
    if (pb.status === "loading") pb.status = "ready";
    if (!pb.started) pb.status = "ready";
    syncTransport();
    /* The seek bar's max depends on the duration, which we only learn now. */
    render();
  });
  playerVideo.addEventListener("durationchange", function () {
    if (!state.playback.url) return;
    state.playback.duration = isFinite(playerVideo.duration) ? playerVideo.duration : 0;
    syncTransport();
  });
  playerVideo.addEventListener("timeupdate", function () {
    if (!state.playback.url) return;
    state.playback.currentTime = playerVideo.currentTime;
    syncTransport();
  });
  playerVideo.addEventListener("play", function () {
    /* The sidebar note may still be explaining an autoplay block. */
    const wasBlocked = state.playback.blockedAutoplay === true;
    state.playback.started = true;
    state.playback.status = "ready";
    state.playback.blockedAutoplay = false;
    /* Start the ten-second chain here rather than on metadata: this is the
       first moment there is real elapsed playback to report. */
    scheduleProgressReport(state.playback);
    syncTransport();
    showPlayerControls();
    scheduleControlsHide();
    if (wasBlocked) render();
  });
  playerVideo.addEventListener("pause", function () {
    /* A pause is a natural bookmark — the viewer is probably walking away —
       and it freezes the clock, so the periodic chain has nothing left to
       report. Safe to call during teardown too: a session that is no longer
       live, or no longer current, is skipped inside reportProgress. */
    stopProgressReporting();
    reportProgress(state.playback);
    syncTransport();
    /* Never fade away on a paused frame. */
    showPlayerControls();
  });
  playerVideo.addEventListener("ended", function () {
    stopProgressReporting();
    /* Reporting here is what lets the server clear a position in the closing
       minutes instead of offering to resume three seconds from the end. */
    reportProgress(state.playback);
    syncTransport();
    showPlayerControls();
  });
  playerVideo.addEventListener("progress", syncTransport);
  playerVideo.addEventListener("seeked", syncTransport);
  playerVideo.addEventListener("error", onVideoError);

  /* The overlay is a sibling of the <video> and, like it, is created once and
     never re-created by a canvas repaint. Only its contents are rebuilt. */
  const playerOverlay = document.createElement("div");
  playerOverlay.className = "player-overlay";
  playerOverlay.id = "player-overlay";
  playerOverlay.setAttribute("role", "group");
  playerOverlay.setAttribute("aria-label", "Player controls");

  const CONTROLS_HIDE_DELAY_MS = 3200;
  let controlsHideTimer = null;
  /* True only while the pointer rests on a *control strip*. The overlay fills
     the whole player, so tracking the overlay itself pinned the bar open
     whenever the player appeared under a stationary pointer — which is exactly
     what happens when the hero Play button is clicked. */
  let pointerInsideControls = false;
  /* How the user last touched the page. `:focus-visible` alone is not enough:
     Chrome matches it on a range input even after a mouse drag, which would pin
     the bar for the rest of the session. */
  let keyboardFocusActive = false;
  const supportsFocusVisible =
    typeof CSS !== "undefined" && typeof CSS.supports === "function" && CSS.supports("selector(:focus-visible)");

  function controlStripOf(node) {
    if (!(node instanceof Element)) return null;
    return node.closest(".player-bar, .player-titlebar");
  }

  /* ── hls.js: lazily loaded, single instance, always torn down ────────── */

  const HLS_SCRIPT_SRC = "vendor/hls.min.js";
  /* A stalled request must not hang the player on a spinner forever; matches
     the API client's own request budget. */
  const HLS_SCRIPT_TIMEOUT_MS = 15000;

  /* One injection for the life of the page. Rejected loads reset the promise
     so a later attempt can retry rather than caching the failure forever. */
  let hlsLoaderPromise = null;
  let hlsScriptElement = null;
  /* At most one live hls.js instance; reassigned only after the previous one
     has been destroyed. */
  let hlsInstance = null;
  /* <track> elements currently attached to the player, paired with the key of
     the server track they represent. Emptied on every teardown so a stale
     selector can never show a previous title's tracks. */
  let subtitleTrackRefs = [];

  function loadHlsLibrary() {
    if (window.Hls) return Promise.resolve(window.Hls);
    /* One injection per page, one promise per injection. Returning the cached
       promise is what keeps concurrent callers sharing it — the executor must
       never decide to "wait for the one in flight", because that promise is
       this one and nothing would ever settle it. */
    if (hlsLoaderPromise) return hlsLoaderPromise;

    hlsLoaderPromise = new Promise(function (resolve, reject) {
      /* A script left behind by an earlier failed attempt has already had its
         chance; keeping it would also make the old code's in-flight check
         below misfire. Clear it before injecting a fresh one. */
      if (hlsScriptElement) {
        hlsScriptElement.remove();
        hlsScriptElement = null;
      }

      let settled = false;
      const timer = setTimeout(function () {
        fail(new Error("timed out loading " + HLS_SCRIPT_SRC));
      }, HLS_SCRIPT_TIMEOUT_MS);

      function succeed(HlsCtor) {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        resolve(HlsCtor);
      }

      function fail(error) {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        if (hlsScriptElement) {
          hlsScriptElement.remove();
          hlsScriptElement = null;
        }
        /* Drop the cache so a retry can inject again rather than awaiting a
           promise that has already failed. */
        hlsLoaderPromise = null;
        reject(error);
      }

      const script = document.createElement("script");
      script.src = HLS_SCRIPT_SRC;
      script.async = true;
      script.dataset.astraeusHls = "1";
      script.addEventListener("load", function () {
        if (window.Hls) succeed(window.Hls);
        else fail(new Error("hls.js loaded but did not expose window.Hls"));
      });
      script.addEventListener("error", function () {
        fail(new Error("could not load " + HLS_SCRIPT_SRC));
      });
      hlsScriptElement = script;
      document.head.append(script);
    });

    return hlsLoaderPromise;
  }

  function destroyHls() {
    if (!hlsInstance) return;
    const instance = hlsInstance;
    hlsInstance = null;
    try {
      instance.stopLoad();
      instance.detachMedia();
      instance.destroy();
    } catch (error) {
      /* A half-dead instance is not worth reporting; it is already detached. */
    }
  }

  /* hls.js drives the element through a MediaSource object URL, so the
     element's own `error` event must not be reported twice. */
  function hlsOwnsMedia() {
    return !!hlsInstance;
  }

  function isEventPlaylistPlayback() {
    /* Only segmented (server-generated) streams grow while they play. */
    return state.playback.mode !== "direct_play" && state.playback.segmented === true;
  }

  /** True once a source is attached and the transport can actually drive it. */
  function playbackIsLive(pb) {
    if (!pb || !pb.url) return false;
    const status = pb.status;
    return status === "ready" || status === "playing" || status === "paused";
  }

  /** Duration the server reported, used only when the element has none yet. */
  function mediaInfoDuration(pb) {
    const info = pb && pb.mediaInfo;
    const value = info ? Number(info.duration_seconds) : NaN;
    return isFinite(value) && value > 0 ? value : 0;
  }

  /* ── Source timeline ─────────────────────────────────────────────────────
     A session started at `start_seconds: S` has media time 0 at source time S.
     Everything the viewer sees — the clock, the seek bar, the quality switch —
     is therefore expressed in source time, and only the element talks in media
     time. */

  /** The whole film's length, in source seconds. */
  function sourceDurationOf(pb) {
    if (pb.sourceDuration > 0) return pb.sourceDuration;
    const fromInfo = mediaInfoDuration(pb);
    if (fromInfo > 0) return fromInfo;
    /* Direct play has no media_info gap to fill: the element knows the file. */
    if (!pb.segmented && isFinite(playerVideo.duration) && playerVideo.duration > 0) {
      return playerVideo.duration + pb.sessionStart;
    }
    return 0;
  }

  /** Where the playhead is, in source seconds. */
  function currentSourceTime(pb) {
    return sourceTime(playerVideo.currentTime, pb.sessionStart);
  }

  /** The part of the source this session can already reach without re-buffering. */
  function producedWindowOf(pb) {
    return producedWindow(seekableBounds(), pb.sessionStart);
  }

  /* ── Resuming and progress reports ───────────────────────────────────────
     A position is only worth offering back when the viewer actually got
     somewhere: a few seconds in is still "the beginning" to anyone watching.
     The server has its own rule for the other end — a report in the closing
     minutes is treated as finished and clears the row instead. */

  /* RESUME_MIN_SECONDS and FINISHED_FRACTION come from core.js with
     progressAction, so the decision and the numbers it uses cannot drift. */
  /* The server's own rule for the other end: a position in the last 5% is
     "watched through" and is cleared rather than stored. Mirrored locally so a
     remembered position can never offer a resume the server has refused. */

  /* A report on every timeupdate would be several PUTs a second for a number
     that barely moved; ten seconds of playback is the most that can be lost
     to a crash or a hard close. */
  const PROGRESS_REPORT_INTERVAL_MS = 10000;

  /* The one live report chain. Module state rather than session state because
     it is a timer, and a timer outlives the session object it was armed for —
     which is exactly why every report re-checks that session. */
  let progressReportTimer = null;

  /**
   * The offset a fresh negotiation should start at, or 0.
   *
   * Read from the detail the canvas is showing, so it can only ever be this
   * entity's own position. An explicit start-over clears it (restartPlayback),
   * which is what stops the next Play from resuming the offset the viewer just
   * abandoned.
   */
  function resumeOffsetFor(entity) {
    const detail = state.detail;
    if (!detail || !detail.entity || detail.entity.id !== entity.id) return 0;
    /* The decision itself is pure and lives in core.js, next to the report
       path's, because the two used to disagree: a position in the last half
       second was "too near the end to resume" here while the report path called
       anything in the last 5% finished. One rule now decides both. */
    return resumeOffsetFromProgress(detail.progress, isLeafType(entity.type));
  }

  /**
   * Send the position this session is at, or forget the stored one when there
   * is nothing worth resuming.
   *
   * `options.final` marks a report made while the session is being put away:
   * it is allowed through even though the session may already have been
   * detached from `state.playback`, and it is the one the caller will not get
   * another chance to make.
   *
   * Failures are swallowed here. The film matters more than the bookmark: a
   * position that cannot be stored is not worth a dialog, not worth stopping
   * playback for, and the next report will try again anyway. An explicit
   * start-over is the exception and warns where it is issued.
   */
  function reportProgress(pb, options) {
    const opts = options || {};
    if (!pb || typeof pb.entityId !== "string" || !pb.entityId) return;
    if (!playbackIsLive(pb)) return;
    /* The gate that keeps a late report off the wrong entity: a queued report
       belongs to the session it was armed for, and the URL below is built from
       that session's own id, never from whatever is playing now. Because
       navigation replaces `state.playback` wholesale, a stale report either
       fails this check or carries the id it belongs to. */
    if (opts.final !== true && state.playback !== pb) return;

    const duration = sourceDurationOf(pb);
    const position = currentSourceTime(pb);

    /* What to do with this report is a pure decision, in core.js, because the
       case that lost data was a disagreement between two callers: the request
       and the local copy each applied their own rule, and both of them read a
       position of 0 as "the viewer is at the beginning".
       `pb.started` is the flag the play handler sets, so a report from before
       playback began says nothing about where the viewer is. StartVideo sets
       status "ready" synchronously, before a byte of media has loaded, and
       stopping or navigating away inside that window used to send 0 - which the
       old code answered with a DELETE. The bookmark was gone (W-1 of the
       2026-10-09 review). */
    const action = progressAction({
      started: pb.started === true,
      position: position,
      duration: duration,
    });
    if (action === "skip") return;

    /* The local copy takes the same decision as the request, so the detail can
       never claim a position the server was not told about, or keep offering a
       resume the server has cleared. */
    rememberProgress(pb.entityId, action === "save" ? position : null, duration);

    const attempt =
      action === "save"
        ? api.saveProgress(pb.entityId, { position_seconds: position, duration_seconds: duration }, opts)
        : api.clearProgress(pb.entityId, opts);
    attempt.then(
      function () {
        /* This report is what the rail now has to agree with. A failed request
           changed nothing server-side, so only success asks for the refresh —
           and a report made as the page goes away has no rail left to paint. */
        if (opts.keepalive === true) return;
        refreshContinueWatching();
      },
      function () {
        /* Best effort, as above. */
      }
    );
  }

  /** Forget a stored position. `warn` is for a viewer who asked for it. */
  function forgetProgress(entityId, options) {
    const opts = options || {};
    if (typeof entityId !== "string" || !entityId) return;
    api.clearProgress(entityId, opts).then(
      function () {
        /* A cleared position can only leave the rail, and a viewer who asked
           for it is looking at the result, so this one does not wait for the
           refresh rate limit. */
        loadContinueWatching();
      },
      function (error) {
        if (opts.warn !== true) return;
        toast(
          "Could not clear the saved position. " +
            (error && error.message ? error.message : "The server did not answer."),
          "warn"
        );
      }
    );
  }

  /**
   * Keep the loaded detail's copy of the position at the value just reported.
   *
   * The detail is fetched once per navigation, so without this a viewer who
   * stops and presses Play again would be sent back to the position the page
   * loaded with — the beginning, for an entity that had never been played.
   * A position the server would clear is cleared locally as well, which is
   * also what an explicit start-over does.
   */
  function rememberProgress(entityId, position, duration) {
    const detail = state.detail;
    if (!detail || !detail.entity || detail.entity.id !== entityId) return;
    /* `position` is the decision's answer: a number to remember, or null to
       forget. It does not re-decide, because that re-decision is what cleared
       the bookmark - a passive report of 0 was read here as "at the beginning"
       even when the request path had already decided to say nothing. */
    if (position === null || position === undefined) {
      detail.progress = null;
      return;
    }
    detail.progress = {
      position_seconds: position,
      duration_seconds: duration,
      percent: duration > 0 ? (100 * position) / duration : 0,
      finished: false,
      updated_at: new Date().toISOString(),
    };
  }

  function stopProgressReporting() {
    if (progressReportTimer !== null) {
      clearTimeout(progressReportTimer);
      progressReportTimer = null;
    }
  }

  /**
   * Arm the ten-second report for `pb`. A single chain, so a second `play`
   * cannot stack timers, and it re-arms itself only while that exact session
   * is still playing — a pause, an end, a failure or a navigation ends it.
   */
  function scheduleProgressReport(pb) {
    stopProgressReporting();
    progressReportTimer = setTimeout(function () {
      progressReportTimer = null;
      if (state.playback !== pb || !playbackIsLive(pb)) return;
      reportProgress(pb);
      if (!playerVideo.paused && !playerVideo.ended) scheduleProgressReport(pb);
    }, PROGRESS_REPORT_INTERVAL_MS);
  }

  function handleHlsError(HlsCtor, instance, recovery, data) {
    if (!data) return;
    const pb = state.playback;
    if (instance !== hlsInstance || !pb.url) return;

    if (!data.fatal) {
      /* hls.js recovers from non-fatal errors on its own. */
      return;
    }

    const type = data.type;
    if (type === HlsCtor.ErrorTypes.NETWORK_ERROR && !recovery.network) {
      recovery.network = true;
      try {
        instance.startLoad();
        return;
      } catch (error) {
        /* fall through to the failure path */
      }
    }
    if (type === HlsCtor.ErrorTypes.MEDIA_ERROR && !recovery.media) {
      recovery.media = true;
      try {
        instance.recoverMediaError();
        return;
      } catch (error) {
        /* fall through to the failure path */
      }
    }

    /* Both recoveries have been tried. Before reporting a failure, ask the server
       for a new session at where the viewer is: a session it has reaped answers
       404, and `startLoad()` above cannot bring it back, but a new session can.
       This happens once - a second failure is real (S-10 of the 2026-10-09
       review). */
    const position = currentSourceTime(pb);
    if (shouldRenegotiateAfterFailure({
      sessionCurrent: state.playback === pb,
      alreadyRenegotiated: recovery.renegotiated === true,
      position: position,
    })) {
      recovery.renegotiated = true;
      toast("The stream expired; resuming…", "info");
      resumeSession({
        startSeconds: position,
        preferredHeight: pb.preferredHeight,
        audioTrackIndex: pb.audioTrackIndex,
      });
      return;
    }

    failSegmentedPlayback(
      "The stream stopped: " +
        (data.details || data.type || "unknown HLS error") +
        (data.reason ? " (" + data.reason + ")" : "") +
        ". Recovery was not possible."
    );
  }

  function failSegmentedPlayback(message) {
    const pb = state.playback;
    const text = "Playback failed for “" + (pb.title || "this title") + "”: " + message;
    /* Clear the url first so teardown's own events are not mistaken for a
       fresh failure, then release everything. */
    pb.url = null;
    pb.status = "error";
    pb.error = text;
    resetActiveMedia();
    closePlayerLayer();
    setActionStatus(text, "error");
    showError(text, function () {
      doPlay();
    });
    toast(text, "error");
    render();
  }

  /**
   * Blink (Chrome/Chromium/Edge) reports a non-empty canPlayType for HLS even
   * though it ships no HLS demuxer — measured: "maybe" on Chromium 152. So
   * canPlayType alone is a liar and trusting it produces exactly the silently
   * dead player we must avoid. Native HLS is a WebKit/Safari feature; require
   * both a positive canPlayType and a non-Blink engine before attempting it.
   */
  function nativeHlsSupport() {
    const probe = document.createElement("video");
    const claim =
      probe.canPlayType("application/vnd.apple.mpegurl") ||
      probe.canPlayType("application/x-mpegURL");
    if (!claim) return false;
    return !isBlinkEngine();
  }

  function isBlinkEngine() {
    const uaData = navigator.userAgentData;
    if (uaData && Array.isArray(uaData.brands)) {
      return uaData.brands.some(function (brand) {
        return /Chromium|Google Chrome|Microsoft Edge/i.test(brand.brand || "");
      });
    }
    return /Chrome|Chromium|Edg\//.test(navigator.userAgent);
  }

  /**
   * The last-resort explanation, used only when a segmented stream has no
   * demuxer at all: no native HLS (Safari/WebKit) and no usable MSE player.
   * `cause` distinguishes the two ways that happens so the message is true.
   */
  function segmentedMessage(pb, cause) {
    const target = pb && pb.mode ? modeLabel(pb.mode) : "segmented delivery";
    const reason =
      pb && Array.isArray(pb.reasons) && pb.reasons.length ? " " + pb.reasons.join(" ") : "";
    let why;
    if (cause === "no-mse") {
      why =
        " This browser has no Media Source Extensions support, and it has no native HLS" +
        " demuxer either, so nothing here can play a playlist.";
    } else if (cause === "no-library") {
      why =
        " The bundled HLS player (vendor/hls.min.js) could not be loaded, so the playlist" +
        " has no demuxer on this page.";
    } else {
      why =
        " Neither native HLS nor the bundled MSE player (vendor/hls.min.js) is available" +
        " here, so the playlist has no demuxer.";
    }
    return (
      "This title needs " +
      target +
      ", which this browser cannot demux on its own." +
      reason +
      why +
      " Nothing was started. Open this title in a browser with native HLS support, or restore" +
      " vendor/hls.min.js and try again."
    );
  }

  function mediaErrorText(code) {
    switch (code) {
      case 1:
        return "the load was aborted";
      case 2:
        return "a network error stopped the download";
      case 3:
        return "the browser could not decode the media";
      case 4:
        return "the source format is not supported by this browser";
      default:
        return "the browser reported an unknown media error";
    }
  }

  function playbackErrorMessage(entity, error) {
    const name = entity ? displayTitle(entity) : "this title";
    if (error && error.status === 409) {
      return "Cannot deliver “" + name + "”: " + error.message;
    }
    if (error && error.status === 400 && error.code === "no_media") {
      return "“" + name + "” has no media file to play.";
    }
    if (error && error.code === "network") {
      return "Could not reach the playback endpoint for “" + name + "”. " + error.message;
    }
    return "Could not start playback for “" + name + "”. " + (error && error.message ? error.message : "");
  }

  function onPlayRejection(error) {
    if (!error) return;
    /* A newer load superseded this one; not a failure worth reporting. */
    if (error.name === "AbortError") return;

    if (error.name === "NotAllowedError") {
      /* Every play() in this file runs after an await, so it sits outside the
         user-gesture task and an autoplay block is the common case, not an
         edge case. The stream is attached and ready — it was simply not
         allowed to start. Leaving the status at "ready" is what makes the next
         Play press toggle playback, instead of negotiating a second session
         and leaving a second ffmpeg running for a stream that already exists. */
      state.playback.status = "ready";
      state.playback.blockedAutoplay = true;
      const blocked =
        "The browser blocked playback until you interact with the page. Press Play to start.";
      setActionStatus(blocked, null);
      toast(blocked, "warn");
      render();
      return;
    }

    /* A genuine media failure: this one really is an error. */
    state.playback.status = "error";
    const message = "The browser could not start playback: " + (error.message || error.name) + ".";
    setActionStatus(message, "error");
    toast(message, "error");
    render();
  }

  function onVideoError() {
    const pb = state.playback;
    /* Teardown (removeAttribute + load) can surface here; only report a real
       failure for a source we are actually trying to play. Clearing url first
       stops the teardown from re-entering this handler. */
    if (!pb.url) return;
    /* hls.js owns the element through a MediaSource; its ERROR event carries
       far better diagnostics, so let that handler report the failure. */
    if (hlsOwnsMedia()) return;
    const message =
      "Playback failed for “" +
      (pb.title || "this title") +
      "”: " +
      mediaErrorText(playerVideo.error ? playerVideo.error.code : 0) +
      ".";
    pb.url = null;
    pb.status = "error";
    pb.error = message;
    resetActiveMedia();
    /* The media is gone, so the overlay would be a dead control bar: close it
       exactly as Stop and the segmented-failure path do. */
    closePlayerLayer();
    setActionStatus(message, "error");
    showError(message, function () {
      doPlay();
    });
    toast(message, "error");
    render();
  }

  /**
   * The actually-seekable window, straight off the element. For a segmented
   * stream the server grows an EVENT playlist as ffmpeg produces segments, so
   * this end moves; we never invent a duration we do not have.
   */
  function seekableBounds() {
    const pb = state.playback;
    let start = 0;
    let end = 0;
    if (playerVideo.seekable && playerVideo.seekable.length > 0) {
      const last = playerVideo.seekable.length - 1;
      const rawStart = playerVideo.seekable.start(last);
      const rawEnd = playerVideo.seekable.end(last);
      if (isFinite(rawStart)) start = rawStart;
      if (isFinite(rawEnd)) end = rawEnd;
    }
    if (!(end > 0) && pb.duration > 0 && isFinite(pb.duration)) {
      end = pb.duration;
    }
    if (end < 0) end = 0;
    if (start < 0) start = 0;
    return { start: start, end: end };
  }

  function syncTransport() {
    const pb = state.playback;
    const seek = document.getElementById("player-seek");
    const time = document.getElementById("player-time");
    const toggle = document.getElementById("player-toggle");
    const window_ = document.getElementById("player-window");
    const mediaLive = !!pb.url;

    const bounds = mediaLive ? seekableBounds() : { start: 0, end: 0 };
    pb.seekableStart = bounds.start;
    pb.seekableEnd = bounds.end;

    /* Everything below is source time: the seek bar spans the whole film even
       when this session has only produced part of it. */
    const total = mediaLive ? sourceDurationOf(pb) : 0;
    const now = mediaLive ? currentSourceTime(pb) : 0;
    const produced = mediaLive ? producedWindowOf(pb) : null;

    if (seek) {
      seek.disabled = !mediaLive || pb.qualityBusy === true || !(total > 0);
      if (total > 0) {
        seek.min = "0";
        seek.max = String(total);
        /* Never fight the user while the range itself has focus. */
        if (document.activeElement !== seek) {
          seek.value = String(Math.min(Math.max(now, 0), total));
        }
      }
      seek.setAttribute(
        "aria-valuetext",
        total > 0 ? formatClock(now) + " of " + formatClock(total) : "unavailable"
      );
    }
    if (time) {
      time.textContent =
        total > 0
          ? formatClock(now) + " / " + formatClock(total)
          : "0:00 / 0:00";
    }
    if (window_) {
      /* How far ahead this session can jump without re-buffering. Only worth
         saying when that is less than the whole film. */
      if (mediaLive && produced && total > 0 && produced.to < total - 1) {
        window_.hidden = false;
        window_.textContent = "Produced to " + formatClock(produced.to);
      } else {
        window_.hidden = true;
        window_.textContent = "";
      }
    }
    if (toggle) {
      const playing = mediaLive && !playerVideo.paused && !playerVideo.ended;
      setIconButton(toggle, playing ? "pause" : "play", playing ? "Pause" : "Play");
      toggle.setAttribute("aria-label", playing ? "Pause" : "Play");
    }
    applyAudioPreference();
    syncQualityControl();
    syncAudioTrackControl();
    /* Fullscreen can be left with Esc or a swipe, so the button is driven from
       the document state rather than from what we last asked for. */
    const fullscreen = document.getElementById("player-fullscreen");
    if (fullscreen) {
      const on = fullscreenActive();
      const label = on ? "Exit fullscreen" : "Enter fullscreen";
      fullscreen.setAttribute("aria-pressed", on ? "true" : "false");
      fullscreen.setAttribute("aria-label", label);
      fullscreen.title = label;
      setIconButton(fullscreen, on ? "fullscreen-exit" : "fullscreen-enter", on ? "Exit" : "Fullscreen");
    }
  }

  /* Order matters: kill hls.js before touching the element, otherwise the
     MediaSource teardown surfaces as a spurious media error. */
  function resetActiveMedia() {
    /* The clock is about to disappear, so nothing is left for the periodic
       chain to report; a session that continues (a quality switch) re-arms it
       from its next `play`. */
    stopProgressReporting();
    destroyHls();
    teardownVideo();
  }

  /* ── Subtitle tracks ─────────────────────────────────────────────────── */

  /** Stable identifier for a server track; `index` is authoritative. */
  function subtitleKey(sub) {
    if (sub && typeof sub.index === "number" && isFinite(sub.index)) return String(sub.index);
    return String((sub && sub.language) || "") + ":" + String((sub && sub.label) || "");
  }

  /* `subtitleDeliverable` and `subtitleNeedsBurn` come from core.js, where
     web/core.test.js can reach them without a browser. Deliverable means the
     server handed us a URL, so the browser can play it as a <track>;
     needs-burn means the track is an image the server could not read, which is
     the only case that costs a re-encode. */

  /** The server track the menu's key names, or null when it is gone. */
  function subtitleFor(pb, key) {
    const list = pb && Array.isArray(pb.subtitles) ? pb.subtitles : [];
    for (const sub of list) {
      if (subtitleKey(sub) === key) return sub;
    }
    return null;
  }

  /** The index the server is burning in now, or 0 when no burn is active. */
  function burnedSubtitleIndex(pb) {
    const decision = pb && pb.decision;
    const value = decision ? Number(decision.burned_subtitle_index) : NaN;
    return isFinite(value) && value > 0 ? value : 0;
  }

  function subtitleLabel(sub) {
    if (sub && typeof sub.label === "string" && sub.label) return sub.label;
    if (sub && typeof sub.language === "string" && sub.language) return sub.language;
    if (sub && typeof sub.index === "number") return "Track " + sub.index;
    return "Subtitles";
  }

  function removeSubtitleTracks() {
    for (const ref of subtitleTrackRefs) {
      try {
        if (ref.element.track) ref.element.track.mode = "disabled";
      } catch (error) {
        /* A detached track cannot be muted; removing it is enough. */
      }
      ref.element.remove();
    }
    subtitleTrackRefs = [];
    state.playback.subtitleSelection = "off";
  }

  /**
   * Attach one <track> per deliverable server track. This has to run after the
   * media element is loaded (direct play) or attached (MSE), because a load()
   * resets the element's text tracks. Track elements are children of the
   * persistent <video>, so a canvas repaint cannot disturb them.
   */
  function applySubtitleTracks() {
    removeSubtitleTracks();
    const pb = state.playback;
    const list = Array.isArray(pb.subtitles) ? pb.subtitles : [];
    let defaultKey = null;

    for (const sub of list) {
      if (!subtitleDeliverable(sub)) continue;
      const key = subtitleKey(sub);
      const element = el("track", {
        kind: "subtitles",
        src: sub.url,
        srclang: sub.language ? String(sub.language) : "und",
        label: subtitleLabel(sub),
      });
      playerVideo.append(element);
      subtitleTrackRefs.push({ key: key, element: element });
      /* Honour the server's default flag, but never pick one ourselves. */
      if (sub.default === true && defaultKey === null) defaultKey = key;
    }

    /* A re-negotiation rebuilds the tracks, and the server stamps its own
       `default` disposition on the new ones. Re-applying that would silently
       undo a deliberate choice — turning subtitles back on for someone who
       picked Off, or moving them off their language. So a preference carried
       across the switch wins whenever its track still exists; only a track
       that has gone away falls back to the server's default. */
    const preferred = pb.subtitlePreference;
    const keys = subtitleTrackRefs.map(function (ref) {
      return ref.key;
    });
    if (preferred === "off") {
      pb.subtitleSelection = "off";
    } else if (preferred && keys.indexOf(preferred) !== -1) {
      pb.subtitleSelection = preferred;
    } else {
      pb.subtitleSelection = defaultKey !== null ? defaultKey : "off";
    }
    /* A burn is not a <track>, so its key never appears in `keys` and the
       default fallback above cannot see it. The decision is the only record
       that one is running, and it wins: otherwise the menu would read Off while
       the picture already has subtitles baked into it. */
    const burning = burnedSubtitleIndex(pb);
    if (burning > 0) pb.subtitleSelection = String(burning);
    pb.subtitlePreference = null;
    applySubtitleModes();
  }

  /** Exactly one track shows; everything else is disabled. */
  function applySubtitleModes() {
    const wanted = state.playback.subtitleSelection;
    for (const ref of subtitleTrackRefs) {
      const track = ref.element.track;
      if (!track) continue;
      try {
        track.mode = ref.key === wanted ? "showing" : "disabled";
      } catch (error) {
        /* Some engines throw on tracks whose source failed; leave them off. */
      }
    }
  }

  /**
   * Adopt a subtitle choice from the menu.
   *
   * A text track toggles a <track> the element already owns. An image track has
   * no URL to toggle, so choosing one asks the server to burn it in — and a
   * burn changes what is encoded, so like an audio-track switch it has to
   * re-negotiate at the current position. Leaving a burn is the same switch in
   * reverse: the subtitles are already part of the picture, so "Off" and a
   * plain text track both need a session that is encoded without it.
   */
  function selectSubtitle(key) {
    const pb = state.playback;
    const next = key || "off";
    const track = next === "off" ? null : subtitleFor(pb, next);

    /* The menu never produces an unknown key; treat one like the old guard. */
    if (next !== "off" && !track) return;

    if (subtitleNeedsBurn(track)) {
      const index = Number(track.index);
      if (!isFinite(index) || index <= 0) return;
      if (!pb.url || pb.qualityBusy) return;
      /* Asking for the burn already running would only re-buffer for nothing. */
      if (index === burnedSubtitleIndex(pb)) return;
      /* Set the choice before re-negotiating: the preference carries it across
         the switch, exactly as it does for a text track. */
      pb.subtitleSelection = next;
      resumeSession({
        startSeconds: currentSourceTime(pb),
        preferredHeight: pb.preferredHeight,
        audioTrackIndex: pb.audioTrackIndex,
        burnSubtitleIndex: index,
      });
      return;
    }

    if (burnedSubtitleIndex(pb) > 0) {
      if (!pb.url || pb.qualityBusy) return;
      pb.subtitleSelection = next;
      resumeSession({
        startSeconds: currentSourceTime(pb),
        preferredHeight: pb.preferredHeight,
        audioTrackIndex: pb.audioTrackIndex,
        /* 0 is the contract's "no burn", which stops the re-encode. */
        burnSubtitleIndex: 0,
      });
      return;
    }

    if (next !== "off" && !subtitleTrackRefs.some(function (ref) { return ref.key === next; })) return;
    pb.subtitleSelection = next;
    applySubtitleModes();
    /* Repaint so the radio group reflects the choice; focus is restored by
       data-focus-key, so keyboard users keep their place. */
    render();
  }

  function teardownVideo() {
    try {
      playerVideo.pause();
    } catch (error) {
      /* Nothing useful to do if pause throws. */
    }
    removeSubtitleTracks();
    playerVideo.removeAttribute("src");
    try {
      playerVideo.load();
    } catch (error) {
      /* load() on an empty source is a no-op we do not need to report. */
    }
  }

  function stopPlayback(options) {
    const opts = options || {};
    /* Capture the session before the reset, so its transcoder can be stopped
       and its last position reported. */
    const sessionId = state.playback.sessionId;
    const session = state.playback;
    /* The position at the moment of stopping is the one worth keeping: the
       element still holds it, and `final` lets it through even though this
       session is about to be replaced. Sent before the reset so the id is
       still the one that was playing. */
    reportProgress(session, { final: true });
    /* Reset state BEFORE tearing the element down: with url already null, the
       teardown's own `error`/`emptied` events cannot be mistaken for a real
       playback failure. */
    state.playback = emptyPlayback();
    closePlayerLayer(sessionId);
    resetActiveMedia();
    if (!opts.silent) render();
  }

  /* While the player covers the canvas, the hero underneath must not be
     reachable by Tab or by a screen reader. */
  function setUnderlayInert(inert) {
    if (inert) {
      dom.canvasContent.setAttribute("inert", "");
      dom.canvasContent.setAttribute("aria-hidden", "true");
    } else {
      dom.canvasContent.removeAttribute("inert");
      dom.canvasContent.removeAttribute("aria-hidden");
    }
  }

  /* ── Player overlay: controls over the video ─────────────────────────── */

  function renderPlayerChrome() {
    const pb = state.playback;
    /* Keep the persistent <video> and overlay nodes in place; only their guts
       are rebuilt, so a repaint can never detach a playing element. */
    if (playerVideo.parentNode !== dom.playerLayer) dom.playerLayer.append(playerVideo);
    if (playerOverlay.parentNode !== dom.playerLayer) dom.playerLayer.append(playerOverlay);

    const restoreKey = focusKeyOf(document.activeElement);
    clear(playerOverlay);
    playerOverlay.append(buildPlayerTitleBar(pb));
    playerOverlay.append(buildPlayerBar(pb));
    applyControlsVisibility();
    if (restoreKey) restoreFocus(restoreKey);
  }

  function buildPlayerTitleBar(pb) {
    return el("div", { class: "player-titlebar" }, [
      el("p", { class: "player-chrome-title", text: pb.title || "Now playing" }),
      el("p", {
        class: "player-chrome-sub",
        text:
          modeLabel(pb.mode) +
          (pb.mediaInfo && pb.mediaInfo.container ? " · " + pb.mediaInfo.container : "") +
          (pb.sessionId ? " · session " + String(pb.sessionId).slice(0, 8) : ""),
      }),
    ]);
  }

  function buildPlayerBar(pb) {
    const live = playbackIsLive(pb);
    const playing = live && !playerVideo.paused && !playerVideo.ended;
    /* Source time, like syncTransport — the bar spans the whole film, not just
       what this session has produced. */
    const total = sourceDurationOf(pb);
    const now = live ? currentSourceTime(pb) : 0;
    const seekable = live && total > 0;
    const growing = live && isEventPlaylistPlayback();

    const toggle = el("button", {
      type: "button",
      class: "btn btn-primary",
      id: "player-toggle",
      "data-action": "play",
      "data-focus-key": "play",
      disabled: !live,
      "aria-label": playing ? "Pause" : "Play",
    });
    setIconButton(toggle, playing ? "pause" : "play", playing ? "Pause" : "Play");

    const restart = el("button", {
      type: "button",
      class: "btn",
      "data-action": "restart",
      "data-focus-key": "restart",
      disabled: !live,
      "aria-label": "Restart from the beginning",
    });
    setIconButton(restart, "restart", null);

    const skip = el("button", {
      type: "button",
      class: "btn",
      "data-action": "skip",
      "data-focus-key": "skip",
      disabled: !live,
      "aria-label": "Skip forward 10 seconds",
    });
    setIconButton(skip, "skip-forward", null);

    const stop = el("button", {
      type: "button",
      class: "btn btn-quiet",
      "data-action": "stop-playback",
      "data-focus-key": "stop",
      "aria-label": "Stop playback and close the player",
    });
    setIconButton(stop, "stop", "Stop");

    const fullscreenOn = document.fullscreenElement === dom.playerLayer;
    const fullscreen = el("button", {
      type: "button",
      class: "btn",
      id: "player-fullscreen",
      "data-action": "fullscreen",
      "data-focus-key": "fullscreen",
      "aria-pressed": fullscreenOn ? "true" : "false",
      "aria-label": fullscreenOn ? "Exit fullscreen" : "Enter fullscreen",
      title: fullscreenOn ? "Exit fullscreen" : "Enter fullscreen",
    });
    setIconButton(
      fullscreen,
      fullscreenOn ? "fullscreen-exit" : "fullscreen-enter",
      fullscreenOn ? "Exit" : "Fullscreen"
    );

    const seek = el("input", {
      type: "range",
      class: "seek",
      id: "player-seek",
      "data-focus-key": "seek",
      min: "0",
      max: String(seekable ? total : 100),
      step: "0.1",
      value: String(seekable ? Math.min(Math.max(now, 0), total) : 0),
      disabled: !seekable,
      "aria-label": growing ? "Seek within the produced part of the stream" : "Seek position",
      "aria-describedby": "playback-note",
      "aria-valuetext": seekable
        ? formatClock(now) + " of " + formatClock(total)
        : "unavailable",
    });

    const time = el("span", {
      class: "player-time",
      id: "player-time",
      text: seekable ? formatClock(now) + " / " + formatClock(total) : "0:00 / 0:00",
    });

    const rows = [
      el("div", { class: "player-scrub" }, [seek, time]),
      el("div", { class: "player-buttons" }, [
        toggle,
        restart,
        skip,
        stop,
        el("span", { class: "player-spacer" }),
        buildPlayerQualityControl(pb, live),
        buildPlayerAudioControl(pb, live),
        buildPlayerVolumeControls(pb, live),
        fullscreen,
      ]),
      buildPlayerSubtitleRow(pb, live),
    ];

    return el("div", { class: "player-bar" }, rows);
  }

  /**
   * Quality menu. Meaningless for direct play — nothing is re-encoded — so it
   * is withheld there rather than offered as a control that does nothing.
   */
  function buildPlayerQualityControl(pb, live) {
    if (pb.mode === "direct_play") return null;
    const options = qualityOptions(pb);
    if (!options.length) return null;

    const busy = pb.qualityBusy === true;
    const select = el("select", {
      class: "quality-select",
      id: "player-quality",
      "data-focus-key": "quality",
      "aria-label": "Playback quality",
      "aria-busy": busy ? "true" : "false",
      disabled: !live || busy,
      title: busy ? "Switching quality…" : "Playback quality",
    });
    select.append(el("option", { value: "auto", text: "Auto" }));
    for (const height of options) {
      select.append(el("option", { value: String(height), text: "Up to " + height + "p" }));
    }
    const wanted = pb.preferredHeight ? String(pb.preferredHeight) : "auto";
    select.value = options.some(function (height) { return String(height) === wanted; })
      ? wanted
      : "auto";

    return el("span", { class: "player-quality" }, [
      select,
      /* A switch means a short re-buffer; say so instead of looking broken. */
      busy ? el("span", { class: "player-quality-note", text: "Switching…" }) : null,
    ]);
  }

  /* Channel counts a viewer knows, written the way a layout is named. */
  const CHANNEL_LAYOUT_NAMES = { 1: "mono", 2: "stereo", 6: "5.1", 8: "7.1" };

  /* ffprobe's codec names, spelled the way the format is written. */
  const AUDIO_CODEC_NAMES = {
    aac: "AAC",
    ac3: "AC-3",
    eac3: "E-AC-3",
    dts: "DTS",
    truehd: "TrueHD",
    flac: "FLAC",
    opus: "Opus",
    mp3: "MP3",
    vorbis: "Vorbis",
  };

  /** Only a track with a usable numeric index can be asked for. */
  function audioTracksOf(pb) {
    const info = pb && pb.mediaInfo;
    const tracks = info && Array.isArray(info.audio_tracks) ? info.audio_tracks : [];
    return tracks.filter(function (track) {
      return !!track && typeof track.index === "number" && isFinite(track.index);
    });
  }

  function channelLayoutName(channels) {
    const count = Number(channels);
    if (!isFinite(count) || count <= 0) return "";
    return CHANNEL_LAYOUT_NAMES[count] || String(count) + "ch";
  }

  function audioCodecName(codec) {
    if (!codec) return "";
    const raw = String(codec);
    return AUDIO_CODEC_NAMES[raw.toLowerCase()] || raw.toUpperCase();
  }

  /**
   * Free-text titles and languages are untrusted and may be long; the name is
   * the part worth reading, so it comes first and the technical detail follows.
   */
  function audioTrackLabel(track, position) {
    const title = track.title ? String(track.title) : "";
    const language = track.language ? String(track.language) : "";
    const name = title || language || "Track " + (position + 1);
    const codec = audioCodecName(track.codec);
    const layout = channelLayoutName(track.channels);
    const detail = codec && layout ? codec + " " + layout : codec || layout;
    return detail ? name + " · " + detail : name;
  }

  /**
   * Audio-track menu. One track is what the session would deliver anyway, so
   * the control only appears when there is a real choice. Direct play still
   * offers it: picking a non-default track makes the server re-negotiate into
   * a remux, so the menu is not a promise the session cannot keep.
   */
  function buildPlayerAudioControl(pb, live) {
    const tracks = audioTracksOf(pb);
    if (tracks.length < 2) return null;

    const busy = pb.qualityBusy === true;
    const select = el("select", {
      class: "audio-select",
      id: "player-audio-track",
      "data-focus-key": "audio-track",
      "aria-label": "Audio track",
      "aria-busy": busy ? "true" : "false",
      disabled: !live || busy,
      title: busy ? "Switching audio track…" : "Audio track",
    });
    for (let i = 0; i < tracks.length; i += 1) {
      const track = tracks[i];
      select.append(el("option", { value: String(track.index), text: audioTrackLabel(track, i) }));
    }
    /* The server's choice is the truth, not what was asked for: an Auto
       request resolves to a concrete track and the menu names it. */
    const wanted = String(pb.targetAudioStreamIndex);
    const has = Array.prototype.some.call(select.options, function (option) {
      return option.value === wanted;
    });
    if (has) select.value = wanted;

    return el("span", { class: "player-audio-track" }, [
      select,
      /* The re-negotiation is a short re-buffer; say so rather than look stuck. */
      busy ? el("span", { class: "player-quality-note", text: "Switching…" }) : null,
    ]);
  }

  function buildPlayerVolumeControls(pb, live) {
    const muted = audioIsMuted();
    const mute = el("button", {
      type: "button",
      class: "btn btn-quiet",
      id: "player-mute",
      "data-action": "toggle-mute",
      "data-focus-key": "mute",
      "aria-pressed": muted ? "true" : "false",
      "aria-label": muted ? "Unmute" : "Mute",
      title: muted ? "Unmute" : "Mute",
    });
    setIconButton(mute, muted ? "volume-mute" : "volume", null);

    const slider = el("input", {
      type: "range",
      class: "volume",
      id: "player-volume",
      "data-focus-key": "volume",
      min: "0",
      max: "1",
      step: "0.01",
      value: String(state.audio.volume),
      disabled: !live,
      "aria-label": "Volume",
      "aria-valuetext": muted ? "muted" : Math.round(state.audio.volume * 100) + "%",
    });

    const level = el("span", {
      class: "player-volume-level",
      id: "player-volume-level",
      text: muted ? "Muted" : Math.round(state.audio.volume * 100) + "%",
    });

    return el("span", { class: "player-audio" }, [mute, slider, level]);
  }

  /** Keep the quality menu in step with the session without rebuilding it. */
  function syncQualityControl() {
    const pb = state.playback;
    const select = document.getElementById("player-quality");
    if (!select) return;
    const busy = pb.qualityBusy === true;
    select.disabled = busy || !playbackIsLive(pb);
    select.setAttribute("aria-busy", busy ? "true" : "false");
    select.title = busy ? "Switching quality…" : "Playback quality";
    const wanted = pb.preferredHeight ? String(pb.preferredHeight) : "auto";
    if (select.value !== wanted) {
      const has = Array.prototype.some.call(select.options, function (option) {
        return option.value === wanted;
      });
      if (has) select.value = wanted;
    }
  }

  /** Keep the audio menu in step with the session without rebuilding it. */
  function syncAudioTrackControl() {
    const pb = state.playback;
    const select = document.getElementById("player-audio-track");
    if (!select) return;
    const busy = pb.qualityBusy === true;
    select.disabled = busy || !playbackIsLive(pb);
    select.setAttribute("aria-busy", busy ? "true" : "false");
    select.title = busy ? "Switching audio track…" : "Audio track";
    const wanted = String(pb.targetAudioStreamIndex);
    if (select.value !== wanted) {
      const has = Array.prototype.some.call(select.options, function (option) {
        return option.value === wanted;
      });
      if (has) select.value = wanted;
    }
  }

  function buildPlayerSubtitleRow(pb, live) {
    const node = subtitleSelectorNode(pb, live);
    if (!node) return null;
    return el("div", { class: "player-bar-subs" }, node);
  }

  /* ── Overlay visibility ──────────────────────────────────────────────── */

  /**
   * True only when the bar should be pinned open by focus: the focus has to be
   * inside the controls *and* have arrived by keyboard. A mouse click leaves
   * focus on the button it pressed, and treating that as "the user is here"
   * would keep the chrome up until they clicked elsewhere.
   */
  function controlsHaveFocus() {
    const active = document.activeElement;
    if (!active || !playerOverlay.contains(active)) return false;
    if (!keyboardFocusActive) return false;
    if (!supportsFocusVisible) return true;
    try {
      return active.matches(":focus-visible");
    } catch (error) {
      return true;
    }
  }

  function applyControlsVisibility() {
    const pb = state.playback;
    const visible = pb.controlsVisible !== false;
    playerOverlay.classList.toggle("is-hidden", !visible);
    if (visible) {
      playerOverlay.removeAttribute("inert");
    } else {
      /* A hidden bar must not be tabbable, or keyboard users get stranded. */
      playerOverlay.setAttribute("inert", "");
    }
  }

  function showPlayerControls() {
    const pb = state.playback;
    if (!pb) return;
    if (pb.controlsVisible === false) {
      pb.controlsVisible = true;
      applyControlsVisibility();
    }
    scheduleControlsHide();
  }

  function scheduleControlsHide() {
    if (controlsHideTimer !== null) {
      clearTimeout(controlsHideTimer);
      controlsHideTimer = null;
    }
    const pb = state.playback;
    if (!pb || !pb.url) return;
    controlsHideTimer = setTimeout(function () {
      controlsHideTimer = null;
      maybeHideControls();
    }, CONTROLS_HIDE_DELAY_MS);
  }

  function maybeHideControls() {
    const pb = state.playback;
    if (!pb || !pb.url) return;
    if (pb.controlsVisible === false) return;
    /* Never fade away on a paused frame; the play event re-arms on resume. */
    if (playerVideo.paused || playerVideo.ended) return;
    /* Focus or hover keeps the chrome up. Both can end without an event we are
       guaranteed to see — a window that loses focus, a pointer that leaves the
       window, a trackpad click that moves nothing — so re-check shortly rather
       than giving up. Whatever pinned the bar cannot pin it forever. */
    if (controlsHaveFocus() || pointerInsideControls) {
      scheduleControlsHide();
      return;
    }
    pb.controlsVisible = false;
    applyControlsVisibility();
  }

  function clearControlsHideTimer() {
    if (controlsHideTimer !== null) {
      clearTimeout(controlsHideTimer);
      controlsHideTimer = null;
    }
  }

  /**
   * Tell the server to stop a streaming session. `DELETE /api/streams/{id}`
   * cancels ffmpeg, waits for it to exit and removes the session directory, so
   * a switch or a teardown does not leave a transcoder running.
   *
   * Fire and forget: a session the idle reaper already collected answers 404,
   * which is not worth surfacing, and a failure still leaves the reaper as a
   * backstop. `keepalive` lets the request outlive the document.
   */
  function releaseStreamSession(sessionId, keepalive) {
    if (typeof sessionId !== "string" || !sessionId) return;
    try {
      const attempt = fetch(API_BASE + "streams/" + encodeURIComponent(sessionId), {
        method: "DELETE",
        keepalive: keepalive === true,
      });
      if (attempt && typeof attempt.catch === "function") {
        attempt.catch(function () {
          /* Already stopped, or offline: the reaper is the backstop. */
        });
      }
    } catch (error) {
      /* Teardown must never throw because of a best-effort cleanup. */
    }
  }

  /**
   * Put the player surface away: no timer left running, no fullscreen left
   * owned, no chrome left behind for the next session to inherit, and no
   * server-side transcoder left running.
   */
  function closePlayerLayer(explicitSessionId) {
    clearControlsHideTimer();
    pointerInsideControls = false;
    exitFullscreenIfOwned();
    clear(playerOverlay);
    dom.playerLayer.hidden = true;
    setUnderlayInert(false);
    /* Whoever caused the teardown, the server must stop producing. */
    releaseStreamSession(
      typeof explicitSessionId === "string" ? explicitSessionId : state.playback.sessionId,
      false
    );
  }

  /* ── Fullscreen ──────────────────────────────────────────────────────── */

  function fullscreenTarget() {
    return dom.playerLayer;
  }

  function fullscreenActive() {
    return document.fullscreenElement === fullscreenTarget();
  }

  function toggleFullscreen() {
    if (dom.playerLayer.hidden) return;
    if (!document.fullscreenEnabled || typeof fullscreenTarget().requestFullscreen !== "function") {
      toast("Fullscreen is not available in this browser.", "warn");
      return;
    }
    let result;
    try {
      result = fullscreenActive()
        ? typeof document.exitFullscreen === "function"
          ? document.exitFullscreen()
          : null
        : fullscreenTarget().requestFullscreen();
    } catch (error) {
      toast("Could not change fullscreen: " + (error && error.message ? error.message : "the browser refused") + ".", "warn");
      return;
    }
    if (result && typeof result.catch === "function") {
      result.catch(function (error) {
        toast(
          "Could not " +
            (fullscreenActive() ? "leave" : "enter") +
            " fullscreen: " +
            (error && error.message ? error.message : "the browser refused") +
            ".",
          "warn"
        );
      });
    }
  }

  function onFullscreenChange() {
    state.playback.fullscreen = fullscreenActive();
    showPlayerControls();
    syncTransport();
  }

  function exitFullscreenIfOwned() {
    if (!fullscreenActive()) return;
    if (typeof document.exitFullscreen !== "function") return;
    try {
      const result = document.exitFullscreen();
      if (result && typeof result.catch === "function") {
        result.catch(function () {
          /* Leaving fullscreen is best-effort during teardown. */
        });
      }
    } catch (error) {
      /* Nothing useful to do; the layer is about to be hidden anyway. */
    }
  }

  /* ── Player keyboard shortcuts ───────────────────────────────────────── */

  /* Only genuine text entry counts: a range slider or a radio is an <input>
     too, and must not swallow the player shortcuts. */
  const TEXT_INPUT_TYPES = [
    "text", "search", "email", "url", "tel", "password", "number",
    "date", "datetime-local", "month", "week", "time",
  ];

  function isTextField(node) {
    if (!(node instanceof Element)) return false;
    if (node.isContentEditable) return true;
    const tag = node.tagName;
    if (tag === "TEXTAREA" || tag === "SELECT") return true;
    if (tag !== "INPUT") return false;
    const type = (node.getAttribute("type") || "text").toLowerCase();
    return TEXT_INPUT_TYPES.indexOf(type) !== -1;
  }

  /* Elements that already act on Space themselves. */
  function isActivatable(node) {
    return (
      node instanceof Element &&
      !!node.closest("button, a[href], summary, input, select, textarea")
    );
  }

  function focusIsElsewhere() {
    const active = document.activeElement;
    if (!active || active === document.body || active === document.documentElement) return false;
    return !dom.playerLayer.contains(active);
  }

  function onPlayerKeydown(event) {
    if (event.defaultPrevented) return;
    if (event.metaKey || event.ctrlKey || event.altKey) return;
    if (!playbackIsLive(state.playback) || dom.playerLayer.hidden) return;
    /* Only act while the player owns focus, or while nothing at all does. */
    if (focusIsElsewhere()) return;

    const target = event.target;
    if (isTextField(target)) return;
    showPlayerControls();

    const key = event.key;
    if (key === " " || key === "Spacebar" || key === "k") {
      /* Let a focused control use Space itself. */
      if (isActivatable(target)) return;
      event.preventDefault();
      doPlay();
      return;
    }
    if (key === "ArrowLeft" || key === "ArrowRight") {
      /* Range and radio inputs already handle arrows natively. */
      if (target instanceof Element && target.closest('input[type="range"], input[type="radio"]')) return;
      event.preventDefault();
      seekBy(key === "ArrowRight" ? 10 : -10);
      return;
    }
    if (key === "f" || key === "F") {
      event.preventDefault();
      toggleFullscreen();
    }
  }

  function startVideo(url, entity, autoplay) {
    const pb = state.playback;
    pb.status = "ready";
    pb.url = url;
    pb.engine = ENGINE_NATIVE;
    pb.segmented = false;
    pb.controlsVisible = true;
    dom.playerLayer.hidden = false;
    setUnderlayInert(true);
    if (playerVideo.getAttribute("src") !== url) {
      playerVideo.setAttribute("src", url);
      try {
        playerVideo.load();
      } catch (error) {
        /* load() is best-effort; the error handler reports real failures. */
      }
    }
    /* Volume travels with the player, not the session, so re-apply it here as
       well as at init: a rebuilt element must never come up at full volume. */
    applyAudioPreference();
    /* Attach tracks after load(): the load algorithm resets text tracks. The
       overlay is built afterwards so its subtitle selector already reflects
       the default selection. */
    applySubtitleTracks();
    renderPlayerChrome();
    if (autoplay !== false) {
      const attempt = playerVideo.play();
      if (attempt && typeof attempt.catch === "function") attempt.catch(onPlayRejection);
    }
    showPlayerControls();
    render();
    focusTransportIfFocusWasLost();
  }

  /**
   * Segmented (HLS) playback. Native HLS is preferred where it exists; every
   * other browser goes through the vendored hls.js over Media Source
   * Extensions. If neither is available we fall back to the honest message.
   */
  async function startSegmented(url, entity, autoplay) {
    const pb = state.playback;
    pb.segmented = true;
    pb.status = "loading";
    pb.url = url;
    pb.engine = null;
    pb.controlsVisible = true;
    dom.playerLayer.hidden = false;
    setUnderlayInert(true);
    applyAudioPreference();
    renderPlayerChrome();
    showPlayerControls();
    render();

    let HlsCtor;
    try {
      HlsCtor = await loadHlsLibrary();
    } catch (error) {
      /* The asset is missing or unreachable: stay honest, do not throw. */
      if (state.playback !== pb) return;
      fallBackToSegmentedMessage("no-library");
      return;
    }

    /* The user may have navigated away, stopped, or started something else
       while the ~600 KB script was in flight. */
    if (state.playback !== pb || pb.entityId !== entity.id || pb.status !== "loading") return;

    if (!HlsCtor || typeof HlsCtor.isSupported !== "function" || !HlsCtor.isSupported()) {
      fallBackToSegmentedMessage("no-mse");
      return;
    }

    destroyHls();
    const recovery = { network: false, media: false };
    const instance = new HlsCtor({ enableWorker: true, lowLatencyMode: false });
    hlsInstance = instance;

    instance.on(HlsCtor.Events.ERROR, function (event, data) {
      handleHlsError(HlsCtor, instance, recovery, data);
    });
    instance.on(HlsCtor.Events.MANIFEST_PARSED, function () {
      if (instance !== hlsInstance) return;
      if (autoplay !== false) {
        const attempt = playerVideo.play();
        if (attempt && typeof attempt.catch === "function") attempt.catch(onPlayRejection);
      }
      syncTransport();
    });
    /* The playlist grows as ffmpeg produces segments; keep the seek bar's
       produced window current as each fragment lands. */
    instance.on(HlsCtor.Events.FRAG_BUFFERED, syncTransport);
    instance.on(HlsCtor.Events.LEVEL_UPDATED, syncTransport);
    instance.on(HlsCtor.Events.LEVEL_LOADED, syncTransport);

    pb.engine = ENGINE_HLS_JS;
    pb.status = "ready";
    instance.attachMedia(playerVideo);
    instance.loadSource(url);
    /* MSE carries text tracks alongside the media source, so the same
       <track> elements work here as on the direct-play path. */
    applySubtitleTracks();
    /* Rebuild once the session is genuinely ready: the transport enables and
       the subtitle selector reflects the tracks just attached. */
    renderPlayerChrome();
    showPlayerControls();
    render();
  }

  function fallBackToSegmentedMessage(cause) {
    const pb = state.playback;
    pb.status = "segmented";
    pb.url = null;
    pb.engine = null;
    pb.segmentedCause = cause || null;
    resetActiveMedia();
    closePlayerLayer();
    const message = segmentedMessage(pb, cause);
    setActionStatus(message, null);
    toast(message, "warn");
    render();
  }

  function focusTransportIfFocusWasLost() {
    /* Making the hero inert can drop focus to <body> when playback was started
       from the hero button. Hand it to the transport instead of losing it. */
    if (!document.activeElement || document.activeElement === document.body) {
      const toggle = document.getElementById("player-toggle");
      if (toggle) toggle.focus({ preventScroll: true });
    }
  }

  function togglePlayPause() {
    if (!state.playback.url) return;
    if (playerVideo.paused || playerVideo.ended) {
      const attempt = playerVideo.play();
      if (attempt && typeof attempt.catch === "function") attempt.catch(onPlayRejection);
    } else {
      playerVideo.pause();
    }
  }

  async function negotiatePlayback(entity) {
    /* Replace the session first (url null), then release the old media, so the
       teardown's own events cannot be mistaken for a fresh failure. Starting a
       new session must never leave the old one running. */
    state.playback = emptyPlayback();
    const session = state.playback;
    session.entityId = entity.id;
    session.title = displayTitle(entity);
    session.status = "loading";
    resetActiveMedia();
    clearError();
    render();

    /* Where to pick up. Read from the loaded detail, which only ever holds
       this entity's own progress; a start-over has already cleared it. */
    const resumeSeconds = resumeOffsetFor(entity);

    try {
      /* Auto quality. The body still carries the full capability manifest,
         because a body replaces the server's browser defaults. */
      const result = await api.playback(entity.id, playbackRequestBody(resumeSeconds, null));
      /* Navigation or Stop may have replaced the session during the request. The
         server has already started ffmpeg for this one, so its id is released
         rather than dropped: the reaper would otherwise be the only thing that
         stopped it, and clicking through titles stacked transcodes (W-4). */
      if (state.playback !== session) {
        releaseStreamSession(orphanedSessionId(result, state.playback, session), true);
        return;
      }
      applyPlaybackResult(session, result, entity, { startSeconds: resumeSeconds, preferredHeight: null });
      startSessionMedia(session, entity, true);
      /* Direct play applies the offset client-side, so its sessionStart is 0
         and the requested offset is the one to name; segmented delivery names
         what the server actually started at. */
      const resumedFrom = session.mode === "direct_play" ? resumeSeconds : session.sessionStart;
      if (resumeSeconds >= RESUME_MIN_SECONDS && resumedFrom > 0) {
        /* The viewer pressed Play expecting the opening titles; say plainly
           why the clock does not start there, in the transport's own format. */
        const message = "Resuming from " + formatClock(resumedFrom);
        setActionStatus(message, "ok");
        toast(message, "info");
      } else if (session.mode === "direct_play") {
        setActionStatus("Playing “" + session.title + "” directly from the original file.", "ok");
      }
    } catch (error) {
      if (state.playback !== session) return;
      session.status = "error";
      session.error = playbackErrorMessage(entity, error);
      session.reasons =
        error && error.body && error.body.decision && Array.isArray(error.body.decision.reasons)
          ? error.body.decision.reasons
          : [];
      setActionStatus(session.error, "error");
      showError(session.error, function () {
        doPlay();
      });
      toast(session.error, "error");
      render();
    }
  }

  function doPlay() {
    const detail = state.detail;
    if (!detail) return;
    const entity = detail.entity;
    const objects = Array.isArray(detail.objects) ? detail.objects : [];
    const pb = state.playback;

    if (!isLeafType(entity.type)) {
      /* Containers have no media object; the transport is already disabled. */
      return;
    }
    if (!objects.length) {
      const message = "“" + displayTitle(entity) + "” has no media file recorded, so there is nothing to play.";
      toast(message, "warn");
      setActionStatus(message, null);
      return;
    }
    if (pb.entityId === entity.id && pb.status === "loading") return;

    /* Already negotiated for this entity, so Play toggles the element rather
       than negotiating again. "ready" is where a refused autoplay lands — the
       stream is attached, only the start was refused — and it must toggle
       here, not open a second session for a stream that already exists. */
    if (
      pb.entityId === entity.id &&
      pb.url &&
      (pb.status === "ready" || pb.status === "playing" || pb.status === "paused")
    ) {
      togglePlayPause();
      return;
    }
    if (pb.entityId === entity.id && pb.status === "segmented") {
      toast(segmentedMessage(pb, pb.segmentedCause), "warn");
      return;
    }
    negotiatePlayback(entity);
  }

  function restartPlayback() {
    const pb = state.playback;
    if (!pb.url) return;
    /* Start over means the stored position goes too, on the server and in the
       detail this page is holding: leaving the local copy behind would let the
       next Play resume the very offset the viewer just abandoned. Clearing the
       detail first is the part that survives a failed request. */
    if (state.detail && state.detail.entity && state.detail.entity.id === pb.entityId) {
      state.detail.progress = null;
    }
    forgetProgress(pb.entityId, { warn: true });
    /* Back to the top of the film, not just the top of what this session
       produced: seeking inside the current session cannot reach behind its own
       start, so this re-negotiates from zero when it has to. */
    seekToSource(0);
  }

  function skipForward() {
    seekBy(10);
  }

  /** Relative seek, in source time. */
  function seekBy(delta) {
    const pb = state.playback;
    if (!pb.url) return;
    seekToSource(currentSourceTime(pb) + delta);
  }

  /** Move the playhead within the current session (media time). */
  function setMediaTime(mediaSeconds) {
    const pb = state.playback;
    try {
      playerVideo.currentTime = mediaSeconds;
    } catch (error) {
      /* Ignore: the source may not be seekable. */
    }
    pb.currentTime = mediaSeconds;
    syncTransport();
  }

  /**
   * Live feedback while the range is being dragged. Only moves the playhead if
   * this session can already reach the target; anything further is left to the
   * `change` handler, so scrubbing cannot fire a re-negotiation per pixel.
   */
  function seekPreview(sourceSeconds) {
    const pb = state.playback;
    if (!pb.url || !isFinite(sourceSeconds)) return;
    const produced = producedWindowOf(pb);
    const reachable =
      !pb.segmented || (produced && sourceSeconds >= produced.from && sourceSeconds <= produced.to);
    if (reachable) setMediaTime(mediaTime(sourceSeconds, pb.sessionStart));
  }

  /**
   * Seek to a point in the film, given in source seconds.
   *
   * Inside what this session has produced, it is an ordinary seek. Beyond that
   * — a segmented stream has only produced up to some point — the session is
   * re-negotiated from there, so seeking into the unproduced part of a film
   * works instead of silently clamping.
   */
  function seekToSource(target) {
    const pb = state.playback;
    if (!pb.url) return;
    let wanted = Number(target);
    if (!isFinite(wanted)) return;
    const total = sourceDurationOf(pb);
    if (wanted < 0) wanted = 0;
    /* The server rejects a start at or past the end of the media. */
    if (total > 0 && wanted > total - 0.5) wanted = Math.max(0, total - 0.5);

    const produced = producedWindowOf(pb);
    const reachable =
      !pb.segmented || (produced && wanted >= produced.from && wanted <= produced.to);

    if (reachable) {
      setMediaTime(mediaTime(wanted, pb.sessionStart));
      return wanted;
    }
    resumeSession({ startSeconds: wanted });
    return wanted;
  }

  /* ── Quality and re-negotiation ──────────────────────────────────────── */

  /* Heights below the source that are worth offering. 1080 is the ceiling
     because a browser cannot be relied on to decode more in software. */
  const QUALITY_LADDER = [1080, 720, 480, 360];

  function sourceHeightOf(pb) {
    const info = pb && pb.mediaInfo;
    const value = info ? Number(info.height) : NaN;
    return isFinite(value) && value > 0 ? value : 0;
  }

  function qualityOptions(pb) {
    const sourceHeight = sourceHeightOf(pb);
    if (!sourceHeight) return [];
    return QUALITY_LADDER.filter(function (height) {
      return height < sourceHeight;
    });
  }

  /** What the viewer is actually watching: the server's choice, or the source. */
  function effectiveHeight(pb) {
    const decision = pb && pb.decision;
    const target = decision ? Number(decision.target_height) : NaN;
    if (isFinite(target) && target > 0) return target;
    return sourceHeightOf(pb);
  }

  /**
   * Adopt a negotiation response onto an existing session. Shared by the first
   * negotiation and by every re-negotiation.
   */
  function applyPlaybackResult(pb, result, entity, requested) {
    if (!result || typeof result.url !== "string" || !result.url) {
      throw new ApiError("The playback endpoint returned no stream URL.", {
        code: "unexpected_shape",
      });
    }
    const decision = result.decision && typeof result.decision === "object" ? result.decision : null;
    const echoed = Number(result.start_seconds);
    pb.entityId = entity.id;
    pb.title = displayTitle(entity);
    pb.mode = result.mode || (decision ? decision.mode : null) || "unknown";
    pb.url = result.url;
    pb.sessionId = result.session_id || null;
    pb.objectId = result.object_id || null;
    pb.decision = decision;
    pb.mediaInfo = result.media_info && typeof result.media_info === "object" ? result.media_info : null;
    pb.subtitles = Array.isArray(result.subtitles) ? result.subtitles : [];
    pb.reasons = decision && Array.isArray(decision.reasons) ? decision.reasons : [];
    pb.error = null;
    /* Where this stream begins in the source. Segmented delivery really does
       start there — the server hands ffmpeg the offset — so media time 0 is
       source time `sessionStart`; the server echoes the offset, and the true
       start is keyframe-aligned and may sit a second or two earlier, so this
       mapping is close, not exact. Direct play is the exception: the server
       serves the whole original file and ignores the offset (it only echoes
       it), so the file's own timeline is the source's, and the offset has to
       be applied as a seek once the element has metadata. */
    const requestedStart =
      requested && isFinite(requested.startSeconds) ? Math.max(0, requested.startSeconds) : 0;
    if (pb.mode === "direct_play") {
      pb.sessionStart = 0;
      pb.resumeAt = requestedStart > 0 ? requestedStart : 0;
    } else {
      pb.sessionStart = isFinite(echoed) && echoed >= 0 ? echoed : requestedStart;
      pb.resumeAt = 0;
    }
    pb.sourceDuration = mediaInfoDuration(pb);
    pb.targetHeight = decision && Number(decision.target_height) > 0 ? Number(decision.target_height) : 0;
    pb.preferredHeight = requested && "preferredHeight" in requested ? requested.preferredHeight : null;
    /* What the server actually selected this session, which is what the audio
       menu reports; absent or 0 means the entity has no audio track. */
    const targetAudio = decision ? Number(decision.target_audio_stream_index) : NaN;
    pb.targetAudioStreamIndex = isFinite(targetAudio) && targetAudio > 0 ? targetAudio : 0;
    pb.audioTrackIndex =
      requested && "audioTrackIndex" in requested ? requested.audioTrackIndex : null;
    pb.currentTime = 0;
    pb.duration = 0;
    return pb;
  }

  /**
   * Point the element (or hls.js) at a freshly negotiated session.
   * `resumePlaying` keeps a paused viewer paused across a quality change.
   */
  function startSessionMedia(pb, entity, resumePlaying) {
    if (pb.mode === "direct_play") {
      pb.segmented = false;
      startVideo(pb.url, entity, resumePlaying !== false);
      return;
    }
    if (nativeHlsSupport()) {
      /* startVideo() marks the session as unsegmented, so set this after it:
         a native HLS playlist still grows as ffmpeg produces it. */
      startVideo(pb.url, entity, resumePlaying !== false);
      pb.segmented = true;
      pb.engine = ENGINE_NATIVE_HLS;
      return;
    }
    startSegmented(pb.url, entity, resumePlaying !== false);
  }

  /**
   * Re-negotiate the current entity — a seek past what has been produced, or a
   * quality change — and carry on from where the viewer is.
   *
   * The old session is stopped before the request goes out, so its ffmpeg is
   * not left encoding frames nobody will watch while the new one spins up.
   * Any failure to stop it is ignored: the idle reaper is the backstop.
   */
  async function resumeSession(options) {
    const opts = options || {};
    const detail = state.detail;
    if (!detail) return;
    const pb = state.playback;
    if (!pb.url || pb.qualityBusy) return;
    const entity = detail.entity;

    /* Capture the position before the teardown resets the element. */
    const startSeconds =
      typeof opts.startSeconds === "number" && isFinite(opts.startSeconds)
        ? Math.max(0, opts.startSeconds)
        : currentSourceTime(pb);
    const preferredHeight = "preferredHeight" in opts ? opts.preferredHeight : pb.preferredHeight;
    /* Height and audio are chosen independently, so swapping one carries the
       other across the re-negotiation unless the caller asked otherwise. */
    const audioTrackIndex = "audioTrackIndex" in opts ? opts.audioTrackIndex : pb.audioTrackIndex;
    /* A burn is a property of the encoded stream too, so it rides along unless
       the caller is the one switching subtitles and named a different one. */
    const burnSubtitleIndex =
      "burnSubtitleIndex" in opts ? opts.burnSubtitleIndex : burnedSubtitleIndex(pb);
    const wasPlaying = !playerVideo.paused && !playerVideo.ended;
    const previousPreferredHeight = pb.preferredHeight;
    const previousAudioTrackIndex = pb.audioTrackIndex;
    const previousSubtitleSelection = pb.subtitleSelection;
    const previousSessionId = pb.sessionId;
    /* The tracks are about to be rebuilt from the new response, which carries
       the server's own `default` disposition again; carry the viewer's actual
       choice across so re-negotiating never rewrites it. */
    pb.subtitlePreference = pb.subtitleSelection;

    pb.qualityBusy = true;
    pb.status = "loading";
    /* Adopt the target offset now so the clock holds its place across the
       re-buffer instead of snapping back to where this session began. The
       response echo confirms it a moment later. */
    pb.sessionStart = startSeconds;
    resetActiveMedia();
    /* Nothing is reading the old stream any more, so stop its transcoder now
       rather than leaving a second ffmpeg running until the idle reaper
       notices. The new session gets a fresh id from the response. */
    releaseStreamSession(previousSessionId, false);
    renderPlayerChrome();
    render();

    try {
      const result = await api.playback(
        entity.id,
        playbackRequestBody(startSeconds, preferredHeight, audioTrackIndex, burnSubtitleIndex)
      );
      /* Navigation or Stop may have replaced the session meanwhile. Its
         transcode is released rather than left to the reaper (W-4). */
      if (state.playback !== pb) {
        releaseStreamSession(orphanedSessionId(result, state.playback, pb), true);
        return;
      }
      applyPlaybackResult(pb, result, entity, {
        startSeconds: startSeconds,
        preferredHeight: preferredHeight,
        audioTrackIndex: audioTrackIndex,
      });
      pb.qualityBusy = false;
      startSessionMedia(pb, entity, wasPlaying);
      toast(
        "Resuming “" + pb.title + "” at " + formatClock(pb.sessionStart) +
          (preferredHeight ? " · " + preferredHeight + "p" : "") + "…",
        "info"
      );
    } catch (error) {
      if (state.playback !== pb) return;
      /* A failed switch is not fatal: say so and keep the session selectable. */
      pb.qualityBusy = false;
      pb.preferredHeight = previousPreferredHeight;
      pb.audioTrackIndex = previousAudioTrackIndex;
      /* A failed burn switch left the old stream playing, so the menu must not
         keep claiming the choice that never took effect. */
      pb.subtitleSelection = previousSubtitleSelection;
      pb.status = "error";
      pb.error = playbackErrorMessage(entity, error);
      setActionStatus(pb.error, "error");
      showError(pb.error, function () {
        doPlay();
      });
      toast(pb.error, "error");
      renderPlayerChrome();
      render();
    }
  }

  function startQualitySwitch(value) {
    const pb = state.playback;
    if (!pb.url || pb.qualityBusy) return;
    const preferredHeight = value === "auto" ? null : Number(value);
    if (preferredHeight !== null && !(preferredHeight > 0)) return;
    if (preferredHeight === pb.preferredHeight) return;
    resumeSession({
      startSeconds: currentSourceTime(pb),
      preferredHeight: preferredHeight,
      /* Only the height is changing; keep the track the viewer picked. */
      audioTrackIndex: pb.audioTrackIndex,
      /* A burn is part of the stream being re-encoded, so it survives too. */
      burnSubtitleIndex: burnedSubtitleIndex(pb),
    });
  }

  function startAudioSwitch(value) {
    const pb = state.playback;
    if (!pb.url || pb.qualityBusy) return;
    const index = Number(value);
    if (!isFinite(index) || index < 0) return;
    /* The menu shows what the server chose, so that is what "already
       selected" means; re-asking for it would re-buffer for nothing. */
    if (index === pb.targetAudioStreamIndex) return;
    resumeSession({
      startSeconds: currentSourceTime(pb),
      preferredHeight: pb.preferredHeight,
      audioTrackIndex: index,
      /* Only the audio track is changing, so an active burn stays. */
      burnSubtitleIndex: burnedSubtitleIndex(pb),
    });
  }

  /* ── 10. Data loading ────────────────────────────────────────────────── */

  function currentToken() {
    return state.loadToken;
  }

  async function loadLibraries() {
    const raw = await api.libraries();
    state.libraries = expectArray(raw, "libraries");
    state.booted = true;
    for (const library of state.libraries) {
      if (library && typeof library.id === "string") state.entityIndex.delete(library.id);
    }
    /* Deliberately not awaited: the rail is a second request and the library
       paint must not queue behind it. It refreshes again on every report. */
    loadContinueWatching();
  }

  /**
   * Load a library's entities into the shared state.
   *
   * `token` is the route token the caller was given. The list is not written
   * unless it is still the current one: a slow library's answer arriving after
   * the viewer moved on used to be written and only then discarded, which meant
   * it had already overwritten the list on screen (W-7).
   *
   * Callers that do not care about staleness - the ones that are the only
   * writer in their flow - pass the current token and get the old behaviour.
   */
  async function refreshEntities(libraryId, token) {
    const raw = await api.libraryEntities(libraryId);
    if (!Array.isArray(raw)) {
      throw new ApiError("The API did not return a list of entities.", { code: "unexpected_shape" });
    }
    if (!entityListIsCurrent(token, state.loadToken)) return false;
    state.entities = raw;
    state.entitiesLibraryId = libraryId;
    for (const entity of raw) rememberEntity(entity);
    return true;
  }

  async function loadEntity(entityId, token) {
    const detail = await api.entity(entityId);
    if (token !== state.loadToken) return;
    if (!detail || !detail.entity) {
      throw new ApiError("The API returned an entity payload without an entity.", {
        code: "unexpected_shape",
      });
    }
    state.detail = detail;
    rememberEntity(detail.entity);
    if (detail.parent) rememberEntity(detail.parent);
    for (const child of Array.isArray(detail.children) ? detail.children : []) rememberEntity(child);
  }

  /* Continue watching. Reports land every ten seconds for as long as something
     plays, and a request per report would be hundreds of list fetches for a
     rail nobody reads mid-film. A report therefore only asks for a refresh and
     the fetch itself is rate-limited; because the scheduled fetch runs after
     the last report in a burst rather than the first, the list still ends up
     agreeing with the position the server holds. */
  const CONTINUE_REFRESH_INTERVAL_MS = 20000;

  /* A slower response must never overwrite a newer one, so each load carries a
     token and only the newest may paint. */
  let continueFetchToken = 0;
  let continueRefreshTimer = null;
  let continueRefreshedAt = 0;

  function refreshContinueWatching() {
    const wait = continueRefreshedAt + CONTINUE_REFRESH_INTERVAL_MS - Date.now();
    if (wait > 0) {
      if (continueRefreshTimer !== null) return;
      continueRefreshTimer = setTimeout(function () {
        continueRefreshTimer = null;
        loadContinueWatching();
      }, wait);
      return;
    }
    loadContinueWatching();
  }

  async function loadContinueWatching() {
    const token = (continueFetchToken += 1);
    continueRefreshedAt = Date.now();
    let raw;
    try {
      raw = await api.progress();
    } catch (error) {
      /* A rail the viewer can live without is not worth a dialog. Dropping the
         list is the honest outcome: what it held can no longer be vouched for,
         and the section hides itself rather than showing a stale row. */
      if (token !== continueFetchToken) return;
      state.continueWatching = [];
      renderContinueWatching();
      return;
    }
    if (token !== continueFetchToken) return;
    const entries = raw && Array.isArray(raw.entries) ? raw.entries : [];
    state.continueWatching = entries.filter(function (entry) {
      /* A row with no entity or no position is nothing to continue. */
      return entry && entry.entity && entry.entity.id && entry.progress;
    });
    for (const entry of state.continueWatching) rememberEntity(entry.entity);
    renderContinueWatching();
  }

  /* ── 11. Router ──────────────────────────────────────────────────────── */

  function parseHash() {
    const raw = location.hash.startsWith("#") ? location.hash.slice(1) : "";
    /* Fragments that are not routes (the skip link's #main-canvas, for
       instance) are left alone so in-page anchors keep working. */
    if (raw.charAt(0) !== "/") return null;
    const parts = raw.split("?");
    const segments = parts[0]
      .split("/")
      .filter(function (segment) {
        return segment.length > 0;
      })
      .map(function (segment) {
        try {
          return decodeURIComponent(segment);
        } catch (error) {
          return segment;
        }
      });
    const params = new URLSearchParams(parts[1] || "");
    return { segs: segments, params: params };
  }

  function identify(segments) {
    if (segments.length >= 2 && segments[0] === "library") {
      return { kind: "library", id: segments[1] };
    }
    if (segments.length >= 2 && segments[0] === "entity") {
      return { kind: "entity", id: segments[1] };
    }
    return null;
  }

  function navigateAndRoute(route) {
    const target = "#/" + route.segs.join("/");
    if (location.hash === target) {
      applyRoute(route);
      return;
    }
    location.hash = target;
  }

  function goTo(kind, id) {
    state.focusTarget = "canvas";
    navigateAndRoute({ segs: [kind, id], params: new URLSearchParams() });
  }

  async function applyRoute(route) {
    const identified = identify(route.segs);
    state.loadToken += 1;
    const token = state.loadToken;
    state.route = route;
    /* Leaving the current entity tears the player down; a stream must never
       outlive the view that started it. */
    stopPlayback({ silent: true });
    state.detail = null;
    dom.canvas.scrollTop = 0;
    dom.contextBody.scrollTop = 0;

    if (!identified) {
      state.loading = false;
      render();
      applyFocusTarget();
      return;
    }

    if (identified.kind === "library") {
      const library = state.libraries.find(function (item) {
        return item.id === identified.id;
      });
      if (!library) {
        state.libraryId = null;
        /* Both fields, not just the list: a leftover id made the next view
           render this library as an empty one (W-8). */
        const cleared = clearedEntityList();
        state.entities = cleared.entities;
        state.entitiesLibraryId = cleared.entitiesLibraryId;
        state.loading = false;
        render();
        applyFocusTarget();
        showError(
          "That library is not in the current list anymore. It may have been deleted.",
          function () {
            boot();
          }
        );
        return;
      }
      state.libraryId = library.id;
      state.loading = true;
      state.entitiesFailed = false;
      setAmbient(library);
      render();
      try {
        const current = await refreshEntities(library.id, token);
        if (!current) return;
        state.entitiesFailed = false;
        clearError();
      } catch (error) {
        if (token !== state.loadToken) return;
        state.entities = [];
        state.entitiesLibraryId = null;
        /* Distinguish "could not fetch" from "fetched, and it is empty". */
        state.entitiesFailed = true;
        showError("Could not load entities for “" + (library.name || "library") + "”. " + error.message, function () {
          applyRoute(route);
        });
      } finally {
        if (token === state.loadToken) {
          state.loading = false;
          render();
          applyFocusTarget();
        }
      }
      return;
    }

    /* Entity route. */
    state.loading = true;
    render();
    try {
      await loadEntity(identified.id, token);
      if (token !== state.loadToken) return;
      const libraryId = state.detail.entity.library_id;
      if (libraryId && state.entitiesLibraryId !== libraryId) {
        try {
          await refreshEntities(libraryId);
          if (token !== state.loadToken) return;
          state.libraryId = libraryId;
        } catch (error) {
          /* The entity itself loaded; a missing sibling list only costs us
             breadcrumb depth, so report it without blanking the canvas. */
          if (token === state.loadToken) {
            showError(
              "Loaded the entity, but its library listing failed. " + error.message,
              function () {
                applyRoute(route);
              }
            );
          }
        }
      }
      if (token !== state.loadToken) return;
      if (!state.libraryId && state.detail.entity.library_id) {
        state.libraryId = state.detail.entity.library_id;
      }
      setAmbient(state.detail.entity);
      clearError();
    } catch (error) {
      if (token !== state.loadToken) return;
      showError("Could not load entity “" + identified.id + "”. " + error.message, function () {
        applyRoute(route);
      });
    } finally {
      if (token === state.loadToken) {
        state.loading = false;
        render();
        applyFocusTarget();
      }
    }
  }

  function onHashChange() {
    const route = parseHash();
    if (!route) return; /* Not an app route — leave the view untouched. */
    applyRoute(route);
  }

  async function boot() {
    state.booted = false;
    state.librariesFailed = false;
    render();
    try {
      await loadLibraries();
    } catch (error) {
      state.booted = true;
      state.libraries = [];
      /* Remember that this was a failure, not an empty server. */
      state.librariesFailed = true;
      render();
      showError("Could not load libraries. " + error.message, function () {
        boot();
      });
      return;
    }
    const route = parseHash();
    if (route && identify(route.segs)) {
      applyRoute(route);
      return;
    }
    if (state.libraries.length > 0) {
      const first = state.libraries[0];
      const target = hashFor("library", first.id);
      let swapped = false;
      try {
        /* replaceState keeps the back button clean and — unlike assigning
           location.hash — does not fire hashchange, so the view loads once. */
        history.replaceState(null, "", target);
        swapped = true;
      } catch (error) {
        swapped = false;
      }
      if (!swapped) location.replace(target);
      applyRoute({ segs: ["library", first.id], params: new URLSearchParams() });
      return;
    }
    render();
  }

  async function checkHealth() {
    try {
      const health = await api.health();
      const service = health && health.service ? health.service : "astraeus";
      dom.healthPill.textContent = service + " online";
      dom.healthPill.dataset.state = "ok";
    } catch (error) {
      dom.healthPill.textContent = "API unreachable";
      dom.healthPill.dataset.state = "down";
    }
  }

  /* ── 12. Events ──────────────────────────────────────────────────────── */

  document.addEventListener("click", function (event) {
    const target = event.target instanceof Element ? event.target : null;
    if (!target) return;

    /* In-app anchors (breadcrumbs, library list) route through the hash, so
       the click never reaches the data-action branch below — record that the
       next painted view should take focus. */
    if (target.closest('a[href^="#/"]')) state.focusTarget = "canvas";

    const trigger = target.closest("[data-action]");
    if (!trigger) return;
    const action = trigger.dataset.action;

    if (action === "open-entity") {
      event.preventDefault();
      goTo("entity", trigger.dataset.entityId);
    } else if (action === "scan") {
      doScan(trigger.dataset.libraryId);
    } else if (action === "play") {
      doPlay();
    } else if (action === "restart") {
      restartPlayback();
    } else if (action === "skip") {
      skipForward();
    } else if (action === "stop-playback") {
      stopPlayback();
    } else if (action === "fullscreen") {
      toggleFullscreen();
    } else if (action === "toggle-mute") {
      toggleMute();
    } else if (action === "retry-boot") {
      boot();
    } else if (action === "retry-route") {
      /* Independent of the banner's retry, which Dismiss clears. */
      if (state.route) applyRoute(state.route);
    } else if (action === "toggle-filter") {
      state.filterIncomplete = !state.filterIncomplete;
      render();
    } else if (action === "retry") {
      if (state.retry) {
        const retry = state.retry;
        retry();
      }
    }
  });

  /* The seek bar, volume slider and quality menu are rebuilt on every render,
     so their events are delegated. */
  document.addEventListener("input", function (event) {
    const target = event.target;
    if (!(target instanceof Element)) return;
    if (target.id === "player-seek") {
      state.playback.seeking = true;
      /* Live scrub feedback only; the re-negotiation waits for `change`. */
      seekPreview(Number(target.value));
      return;
    }
    if (target.id === "player-volume") setVolume(target.value);
  });

  document.addEventListener("change", function (event) {
    const target = event.target;
    if (!(target instanceof Element)) return;
    if (target.id === "player-seek") {
      state.playback.seeking = false;
      seekToSource(Number(target.value));
      return;
    }
    if (target.id === "player-volume") {
      saveAudioPreference();
      return;
    }
    if (target.id === "player-quality") {
      startQualitySwitch(target.value);
      return;
    }
    if (target.id === "player-audio-track") {
      startAudioSwitch(target.value);
      return;
    }
    /* Covers mouse selection and arrow-key traversal of the radio group. */
    if (target.dataset && target.dataset.action === "select-subtitle") {
      selectSubtitle(target.dataset.subtitleKey);
    }
  });

  dom.filterToggle.addEventListener("click", function () {
    state.filterIncomplete = !state.filterIncomplete;
    render();
  });

  dom.enrichButton.addEventListener("click", function () {
    doEnrich();
  });

  dom.errorDismiss.addEventListener("click", function () {
    clearError();
  });

  dom.errorRetry.addEventListener("click", function () {
    const retry = state.retry;
    clearError();
    if (retry) retry();
  });

  window.addEventListener("hashchange", onHashChange);

  /* Last chance to stop a transcoder: the tab is going away, so the request
     has to outlive the document. A beacon cannot issue DELETE, so this is a
     keepalive fetch instead. */
  function releaseSessionOnUnload() {
    const pb = state.playback;
    if (!pb || !pb.url) return;
    /* The last position rides along with the stream teardown: keepalive is
       what lets both requests outlive the document. */
    reportProgress(pb, { final: true, keepalive: true });
    releaseStreamSession(pb.sessionId, true);
  }

  window.addEventListener("pagehide", releaseSessionOnUnload);
  window.addEventListener("beforeunload", releaseSessionOnUnload);

  /* ── Player overlay: interaction, auto-hide, fullscreen, shortcuts ───── */

  /* The hover latch is driven by real pointer *movement*, never by
     `mouseover`/`mouseout`. Those fire on a DOM mutation under a stationary
     cursor, and the player appears exactly under the pointer that just clicked
     the hero Play button — whose button sits in the same band as the control
     bar — so they latched "the user is on the bar" with nobody touching
     anything. `mousemove` only fires when the pointer actually moves.

     It is owned by `document`, not by the player, because the pointer leaving
     the player entirely — to the sidebar, another column, off the window — must
     clear the latch too. A layer-only listener never hears about that move, so
     the latch kept its last value and the bar stayed up for good. */
  document.addEventListener("mousemove", function (event) {
    if (dom.playerLayer.contains(event.target)) {
      pointerInsideControls = !!controlStripOf(event.target);
      showPlayerControls();
      return;
    }
    /* Only react to leaving the controls when the latch is actually set, so
       ordinary movement around the rest of the app does not keep re-arming a
       hide timer that has nothing to do. */
    if (!pointerInsideControls) return;
    pointerInsideControls = false;
    scheduleControlsHide();
  });

  /* Leaving the window stops `mousemove` reaching us at all, so the root
     element's `mouseleave` closes that case too. `<html>` persists for the life
     of the page, so unlike the old `mouseenter` this cannot fire spuriously
     when a DOM mutation appears under a stationary pointer. */
  document.documentElement.addEventListener("mouseleave", function () {
    if (!pointerInsideControls) return;
    pointerInsideControls = false;
    scheduleControlsHide();
  });

  dom.playerLayer.addEventListener("pointerdown", function (event) {
    /* A real press, so it is safe to latch — but only a mouse hovers. A touch
       tap must not leave the bar latched open with no matching departure. */
    if (event.pointerType !== "touch") {
      pointerInsideControls = !!controlStripOf(event.target);
    }
    showPlayerControls();
  });

  /* Capture phase, so the modality is recorded before any handler acts on it,
     whatever the event's target turns out to be. */
  document.addEventListener(
    "keydown",
    function () {
      keyboardFocusActive = true;
    },
    true
  );
  document.addEventListener(
    "pointerdown",
    function () {
      keyboardFocusActive = false;
    },
    true
  );

  /* Keyboard focus anywhere in the player keeps the chrome on screen. */
  dom.playerLayer.addEventListener("focusin", function () {
    showPlayerControls();
  });

  dom.playerLayer.addEventListener("focusout", function () {
    /* Focus may be moving within the overlay; re-check on the next turn. */
    setTimeout(function () {
      if (!controlsHaveFocus()) scheduleControlsHide();
    }, 0);
  });

  document.addEventListener("keydown", onPlayerKeydown);

  if ("onfullscreenchange" in document) {
    document.addEventListener("fullscreenchange", onFullscreenChange);
  }
  document.addEventListener("webkitfullscreenchange", onFullscreenChange);

  /* A second click on the app's own CSP-free world is enough to wire up the
     remaining global listeners; nothing else needs bootstrapping. */
  function init() {
    render();
    /* Prime the element with the remembered volume before anything plays. */
    applyAudioPreference();
    checkHealth();
    boot();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
