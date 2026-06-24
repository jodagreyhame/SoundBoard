/* SoundBoard — Wails frontend bootstrap (skeleton phase).
 *
 * The authoritative design comp (design/source/"SoundBoard App.dc.html") uses
 * a template DSL ({{ }}, <sc-if>, <sc-for>, support.js runtime). That runtime is
 * a preview-only tool and is NOT shipped. This file reimplements the parts the
 * skeleton needs as plain state + render + event code:
 *   - read the full snapshot from App.GetState()
 *   - apply the theme class, render the sidebar category list + routing pill
 *   - wire the titlebar/footer buttons to the bound Go methods
 *   - subscribe to the Go->JS events (gateLevel / routingStatus / installProgress)
 *
 * The soundboard grid, mixer dock, and Audio panel are intentionally left as a
 * placeholder; they are built in phase 2 on top of this same binding contract.
 *
 * Binding contract (Wails injects these):
 *   window.go.main.App.<Method>  -> the bound Go methods (return Promises)
 *   window.runtime.EventsOn/Emit -> the live event bus
 */

(function () {
  "use strict";

  // --- bound-method access -------------------------------------------------
  // Wails exposes bindings at window.go.main.App. Guard each call so the page
  // still renders (with sample data) if opened outside the Wails runtime.
  function App() {
    return (window.go && window.go.main && window.go.main.App) || null;
  }
  function runtime() {
    return window.runtime || null;
  }

  // A safe fallback snapshot so the shell renders even before/without the Go
  // backend (e.g. previewed in a plain browser). Mirrors GetState()'s shape.
  var FALLBACK_STATE = {
    theme: "dark",
    routing: { state: "absent", detail: "VB-CABLE not detected — install it to route audio into Discord.", canEngage: false },
    categories: [
      { name: "game-clips", count: 6 }, { name: "games", count: 39 }, { name: "memes", count: 12 },
      { name: "movies", count: 36 }, { name: "game-clips", count: 2 }, { name: "reactions", count: 14 },
      { name: "game-clips", count: 9 }, { name: "scifi", count: 28 }, { name: "game-clips", count: 6 },
      { name: "films", count: 35 }, { name: "tv", count: 12 }, { name: "wow", count: 13 }
    ],
    clips: [],
    favorites: [],
    volumes: { mic: 1, master: 1, monitor: 1 },
    perClip: {},
    audio: {
      micMode: "vad", gateSensitivity: 0.15, noiseSuppression: false,
      agc: false, ducking: false, forceThrough: false
    }
  };

  // Category chip colours, keyed by category name. Matches the design comp's
  // CATS chips so the sidebar dots read identically. Unknown categories fall
  // back to the primary accent.
  var CHIP = {
    game-clips: "#e0564b", games: "#5865f2", memes: "#f5a524", movies: "#b06bf0",
    game-clips: "#1bbf9c", reactions: "#3aa0f0", game-clips: "#43c46b", scifi: "#22d3c0",
    game-clips: "#94a3b3", "films": "#f1c40f", tv: "#ee5a6f", wow: "#2f8fd6"
  };
  function chipFor(name) { return CHIP[name] || "var(--primary)"; }

  // Prettify a category key into a display label (dashes/underscores -> spaces,
  // first letter capitalised) — the same rule the backend/catalog uses.
  function prettyCategory(name) {
    var s = String(name || "").replace(/[-_]/g, " ").trim();
    return s.charAt(0).toUpperCase() + s.slice(1);
  }

  var state = FALLBACK_STATE;

  // --- render --------------------------------------------------------------
  function applyTheme(theme) {
    var root = document.getElementById("app");
    if (theme === "light") root.classList.add("light");
    else root.classList.remove("light");
    document.getElementById("theme-icon").textContent = theme === "dark" ? "☾" : "☀";
    document.getElementById("theme-label").textContent = theme === "dark" ? "Dark" : "Light";
  }

  function renderCategories(categories) {
    var host = document.getElementById("nav-categories");
    host.innerHTML = "";
    (categories || []).forEach(function (c) {
      var row = document.createElement("div");
      row.className = "jump";
      row.style.cssText =
        "display:flex;align-items:center;gap:9px;padding:7px 8px;border-radius:8px;" +
        "cursor:pointer;color:var(--dim);font-size:12.5px;font-weight:600;";
      row.innerHTML =
        '<span style="width:8px;height:8px;border-radius:3px;flex:none;background:' + chipFor(c.name) + ';"></span>' +
        '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;text-transform:capitalize;">' +
        prettyCategory(c.name) + "</span>" +
        '<span class="mono" style="font-size:10px;color:var(--faint);">' + c.count + "</span>";
      host.appendChild(row);
    });
  }

  // The routing pill in the sidebar footer reflects SetupController state:
  // engaged -> green "Routing active"; anything else -> warning "needs setup".
  function renderRouting(routing) {
    routing = routing || {};
    var engaged = routing.state === "engaged";
    var pill = document.getElementById("conn-pill");
    var dot = document.getElementById("conn-dot");
    var label = document.getElementById("conn-label");
    pill.style.background = engaged ? "var(--ok-bg)" : "var(--warn-bg)";
    dot.style.background = engaged ? "var(--success)" : "var(--warning)";
    dot.style.boxShadow = engaged ? "0 0 8px var(--success)" : "none";
    label.style.color = engaged ? "var(--success)" : "var(--warning)";
    label.textContent = engaged ? "Routing active" : "Routing needs setup";
  }

  function renderStateLine(s) {
    var clips = (s.clips && s.clips.length) || 0;
    var cats = (s.categories && s.categories.length) || 0;
    document.getElementById("ph-state").textContent =
      "GetState(): " + cats + " categories · " + clips + " clips · theme " + s.theme +
      " · routing " + (s.routing && s.routing.state);
  }

  function render(s) {
    state = s || FALLBACK_STATE;
    applyTheme(state.theme);
    renderCategories(state.categories);
    renderRouting(state.routing);
    renderStateLine(state);
  }

  // --- nav (Soundboard / Mic & Audio) -------------------------------------
  function setView(view) {
    document.getElementById("nav-sound").classList.toggle("active", view === "sound");
    document.getElementById("nav-audio").classList.toggle("active", view === "audio");
    var title = document.getElementById("ph-title");
    var sub = document.getElementById("ph-sub");
    if (view === "audio") {
      title.textContent = "Mic & Audio — Wails shell";
      sub.innerHTML = "Mic-processing panel (input mode, gate, live meter, toggles) lands in phase 2, bound to <code>SetMicMode</code> / <code>SetGateSensitivity</code> / the <code>gateLevel</code> event.";
    } else {
      title.textContent = "SoundBoard — Wails shell";
      sub.innerHTML = "Theme foundation + titlebar + sidebar are live. Soundboard grid, mixer dock, and the Audio panel land in phase 2, bound to the Go <code>App</code> methods.";
    }
  }

  // --- wiring --------------------------------------------------------------
  function wire() {
    document.getElementById("nav-sound").addEventListener("click", function () { setView("sound"); });
    document.getElementById("nav-audio").addEventListener("click", function () { setView("audio"); });

    document.getElementById("theme-toggle").addEventListener("click", function () {
      var next = state.theme === "dark" ? "light" : "dark";
      state.theme = next;
      applyTheme(next);
      var a = App();
      if (a && a.SetTheme) a.SetTheme(next);
    });

    document.getElementById("win-min").addEventListener("click", function () {
      var a = App();
      if (a && a.Minimize) a.Minimize();
    });
    var hide = function () {
      var a = App();
      if (a && a.HideToTray) a.HideToTray();
    };
    document.getElementById("win-tray").addEventListener("click", hide);
    document.getElementById("win-close").addEventListener("click", hide);
    document.getElementById("foot-tray").addEventListener("click", hide);

    document.getElementById("foot-quit").addEventListener("click", function () {
      var a = App();
      if (a && a.Quit) a.Quit();
    });
  }

  // --- live events (Go -> JS) ----------------------------------------------
  function subscribeEvents() {
    var rt = runtime();
    if (!rt || !rt.EventsOn) return;

    // gateLevel: ~15-30 Hz mic-open level [0..1]. Phase 2 drives the ring meter;
    // for now we just keep the subscription alive so the contract is exercised.
    rt.EventsOn("gateLevel", function () { /* phase 2: update meter */ });

    // routingStatus: {state, detail, canEngage} — refresh the sidebar pill live.
    rt.EventsOn("routingStatus", function (status) {
      if (status) {
        state.routing = status;
        renderRouting(status);
      }
    });

    // installProgress: {msg, done, err} — phase 2 drives the install dialog.
    rt.EventsOn("installProgress", function () { /* phase 2: dialog */ });
  }

  // --- boot ----------------------------------------------------------------
  function boot() {
    wire();
    subscribeEvents();
    var a = App();
    if (a && a.GetState) {
      a.GetState()
        .then(function (s) { render(s); })
        .catch(function () { render(FALLBACK_STATE); });
    } else {
      // No Wails runtime (plain-browser preview): render the fallback shell.
      render(FALLBACK_STATE);
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }
})();
