/* ==========================================================================
   Astraeus Media — icon registry (GENERATED FILE; do not hand-edit)
   --------------------------------------------------------------------------
   BoxIcons v2 — https://icon-sets.iconify.design/bx/
   Copyright Boxicons, licensed MIT.
   https://github.com/box-icons/boxicons/blob/main/LICENSE

   Regenerate with:  node scripts/fetch-icons.mjs
   Source:           https://api.iconify.design/bx.json?icons=play,pause,refresh,fast-forward,stop,volume-full,volume-mute,fullscreen,exit-fullscreen,chevron-right

   Only the 10 icons this UI actually uses are stored, so the file stays a
   few kilobytes instead of shipping an entire set.

   Icons:

   play              bx:play
   pause             bx:pause
   restart           bx:refresh
   skip-forward      bx:fast-forward
   stop              bx:stop
   volume            bx:volume-full
   volume-mute       bx:volume-mute
   fullscreen-enter  bx:fullscreen
   fullscreen-exit   bx:exit-fullscreen
   arrow-right       bx:chevron-right

   The MIT notice for this set must travel with this file; it is kept in
   web/vendor/bx.LICENSE and summarised in web/vendor/icons.md.
   ========================================================================== */

(function (global) {
  "use strict";

  var SVG_NS = "http://www.w3.org/2000/svg";
  var VIEW_BOX = "0 0 24 24";

  /* Frozen registry: each entry lists the <path> "d" attributes, plus an
     optional parallel list flagging paths that need fill-rule="evenodd". */
  var ICONS = Object.freeze({
    "play": { d: ["M7 6v12l10-6z"] },
    "pause": { d: ["M8 7h3v10H8zm5 0h3v10h-3z"] },
    "restart": { d: ["M10 11H7.101l.001-.009a5 5 0 0 1 .752-1.787a5.05 5.05 0 0 1 2.2-1.811q.455-.193.938-.291a5.1 5.1 0 0 1 2.018 0a5 5 0 0 1 2.525 1.361l1.416-1.412a7 7 0 0 0-2.224-1.501a7 7 0 0 0-1.315-.408a7.1 7.1 0 0 0-2.819 0a7 7 0 0 0-1.316.409a7.04 7.04 0 0 0-3.08 2.534a7 7 0 0 0-1.054 2.505c-.028.135-.043.273-.063.41H2l4 4zm4 2h2.899l-.001.008a4.98 4.98 0 0 1-2.103 3.138a4.9 4.9 0 0 1-1.787.752a5.1 5.1 0 0 1-2.017 0a5 5 0 0 1-1.787-.752a5 5 0 0 1-.74-.61L7.05 16.95a7 7 0 0 0 2.225 1.5c.424.18.867.317 1.315.408a7.1 7.1 0 0 0 2.818 0a7.03 7.03 0 0 0 4.395-2.945a7 7 0 0 0 1.053-2.503c.027-.135.043-.273.063-.41H22l-4-4z"] },
    "skip-forward": { d: ["m19 12l-7-5v10zM5 7v10l7-5z"] },
    "stop": { d: ["M7 7h10v10H7z"] },
    "volume": { d: ["M16 21c3.527-1.547 5.999-4.909 5.999-9S19.527 4.547 16 3v2c2.387 1.386 3.999 4.047 3.999 7S18.387 17.614 16 19z", "M16 7v10c1.225-1.1 2-3.229 2-5s-.775-3.9-2-5M4 17h2.697l5.748 3.832a1 1 0 0 0 1.027.05A1 1 0 0 0 14 20V4a1 1 0 0 0-1.554-.832L6.697 7H4c-1.103 0-2 .897-2 2v6c0 1.103.897 2 2 2m0-8h3c.033 0 .061-.016.093-.019a1 1 0 0 0 .38-.116c.026-.015.057-.017.082-.033L12 5.868v12.264l-4.445-2.964c-.025-.017-.056-.02-.082-.033a1 1 0 0 0-.382-.116C7.059 15.016 7.032 15 7 15H4z"] },
    "volume-mute": { d: ["m21.707 20.293l-2.023-2.023A9.57 9.57 0 0 0 21.999 12c0-4.091-2.472-7.453-5.999-9v2c2.387 1.386 3.999 4.047 3.999 7a8.1 8.1 0 0 1-1.672 4.913l-1.285-1.285C17.644 14.536 18 13.19 18 12c0-1.771-.775-3.9-2-5v7.586l-2-2V4a1 1 0 0 0-1.554-.832L7.727 6.313l-4.02-4.02l-1.414 1.414l18 18zM12 5.868v4.718L9.169 7.755zM4 17h2.697l5.748 3.832a1 1 0 0 0 1.027.05A1 1 0 0 0 14 20v-1.879l-2-2v2.011l-4.445-2.964c-.025-.017-.056-.02-.082-.033a1 1 0 0 0-.382-.116C7.059 15.016 7.032 15 7 15H4V9h.879L3.102 7.223A2 2 0 0 0 2 9v6c0 1.103.897 2 2 2"] },
    "fullscreen-enter": { d: ["M5 5h5V3H3v7h2zm5 14H5v-5H3v7h7zm11-5h-2v5h-5v2h7zm-2-4h2V3h-7v2h5z"] },
    "fullscreen-exit": { d: ["M10 4H8v4H4v2h6zM8 20h2v-6H4v2h4zm12-6h-6v6h2v-4h4zm0-6h-4V4h-2v6h6z"] },
    "arrow-right": { d: ["M10.707 17.707L16.414 12l-5.707-5.707l-1.414 1.414L13.586 12l-4.293 4.293z"] }
  });

  /* Builds an <svg> for a registry key. The icon is decorative by default, so
     that a button keeps its own accessible name; pass a label when the icon
     stands alone and has to carry the meaning itself. */
  function icon(name, options) {
    var opts = options || {};
    var svg = document.createElementNS(SVG_NS, "svg");
    svg.setAttribute("viewBox", VIEW_BOX);
    svg.setAttribute("class", opts.className ? "icon " + opts.className : "icon");
    svg.setAttribute("focusable", "false");
    if (opts.label) {
      svg.setAttribute("role", "img");
      var title = document.createElementNS(SVG_NS, "title");
      title.textContent = opts.label;
      svg.appendChild(title);
    } else {
      svg.setAttribute("aria-hidden", "true");
    }
    var spec = Object.prototype.hasOwnProperty.call(ICONS, name) ? ICONS[name] : null;
    if (!spec) return svg;
    for (var i = 0; i < spec.d.length; i += 1) {
      var path = document.createElementNS(SVG_NS, "path");
      path.setAttribute("d", spec.d[i]);
      path.setAttribute("fill", "currentColor");
      if (spec.eo && spec.eo[i]) {
        path.setAttribute("fill-rule", "evenodd");
        path.setAttribute("clip-rule", "evenodd");
      }
      svg.appendChild(path);
    }
    return svg;
  }

  global.AstraeusIcons = Object.freeze({
    icon: icon,
    names: Object.freeze(Object.keys(ICONS)),
  });
})(typeof window !== "undefined" ? window : this);
