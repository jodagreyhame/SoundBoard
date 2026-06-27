/* SoundBoard — Wails frontend application.
 *
 * The authoritative design comp (design/source/"SoundBoard App.dc.html")
 * expresses the UI as a template DSL ({{ }}, <sc-if>, <sc-for>, onClick="{{ }}")
 * driven by support.js — a preview-only runtime that is NOT shipped. This file
 * reimplements the entire comp as real vanilla state + render + event code,
 * wired to the Wails-bound Go App.
 *
 * Architecture:
 *   - boot(): read the full snapshot from App.GetState(), hydrate `state`,
 *     render every view, wire every control to a bound method, subscribe to the
 *     three Go->JS events.
 *   - Bindings (window.go.main.App.<Method>) return Promises; every call is
 *     guarded so the page still renders (with the snapshot's data) if opened
 *     outside the Wails runtime.
 *   - Events (window.runtime.EventsOn): gateLevel -> animate the ring meter;
 *     routingStatus -> update the banner + sidebar pill; installProgress ->
 *     drive the install/engage dialog.
 *
 * Data model (the binding contract, app.go State): theme, routing{state,detail,
 * canEngage}, categories[{name,count}], clips[{id,name,category,favorite}],
 * favorites[]string, volumes{mic,master,monitor}, perClip{id:float},
 * audio{micMode,gateSensitivity,noiseSuppression,noiseSuppressionTier,
 * echoCancellation,agc,ducking,forceThrough,monitorSource,advancedVoiceActivity,
 * autoSensitivity,attenuationAmount,audioSubsystem} — the Discord Voice & Video
 * parity control set.
 *
 * The grid renders state.clips grouped by category. The wired backend's
 * GetState() returns the REAL catalog (the clip library across 12 categories), so the
 * grid is driven entirely by live data. app.js keeps a placeholder synthesizer
 * (synthClips) as a pure DEFENSIVE fallback for the degraded case where the
 * snapshot carries categories+counts but no clips[] (e.g. opened outside Wails,
 * or a catalog-walk failure leaves an empty library); whenever clips[] is
 * populated — the normal path — the synthesizer is bypassed entirely.
 */

(function () {
  "use strict";

  // --- bound-method / runtime access ---------------------------------------
  function App() {
    return (window.go && window.go.main && window.go.main.App) || null;
  }
  function rt() { return window.runtime || null; }

  // Call a bound method by name with args; swallow absence/rejection so the UI
  // never breaks when previewed outside Wails. Returns the Promise (or null).
  function call(name, ...args) {
    var a = App();
    if (a && typeof a[name] === "function") {
      try { return Promise.resolve(a[name].apply(a, args)); }
      catch (e) { return null; }
    }
    return null;
  }

  // --- category chip colours (verbatim from the comp CATS) ------------------
  var CHIP = {};
  function chipFor(name) { return CHIP[name] || "#5865f2"; }

  // The comp's per-category word lists — used ONLY to synthesize readable
  // placeholder clip names in the skeleton build (when GetState returns no
  // clips). Real clips from the backend always take precedence.
  var WORDS = {};

  function pretty(s) { return String(s || "").replace(/[-_]/g, " ").trim(); }
  function capWords(s) { return pretty(s).replace(/\b\w/g, function (c) { return c.toUpperCase(); }); }

  // --- live UI state (not all of it is persisted server-side) --------------
  var FALLBACK = {
    theme: "dark",
    routing: { state: "absent", detail: "VB-CABLE not detected — install it to route audio into Discord.", canEngage: false },
    categories: [
      { name: "game-clips", count: 6 }, { name: "games", count: 39 }, { name: "memes", count: 12 },
      { name: "movies", count: 36 }, { name: "game-clips", count: 2 }, { name: "reactions", count: 14 },
      { name: "game-clips", count: 9 }, { name: "scifi", count: 28 }, { name: "game-clips", count: 6 },
      { name: "films", count: 35 }, { name: "tv", count: 12 }, { name: "wow", count: 13 }
    ],
    clips: [], favorites: [],
    volumes: { mic: 1, master: 1, monitor: 1 }, perClip: {},
    audio: {
      micMode: "vad", gateSensitivity: 0.15, noiseSuppression: false,
      noiseSuppressionTier: "high", echoCancellation: false, agc: false,
      ducking: false, forceThrough: false, monitorSource: "clips",
      advancedVoiceActivity: true, autoSensitivity: true,
      attenuationAmount: 0.5, audioSubsystem: "standard", pttHotkey: ""
    }
  };

  var S = {
    snap: FALLBACK,           // last server snapshot
    view: "sound",            // 'sound' | 'audio'
    theme: "dark",
    search: "",
    favorites: [],            // []clipID
    clips: [],                // normalized [{id,name,category,favorite}]
    cats: [],                 // [{name,count}]
    playing: [],              // [{id,name,chip}] now-playing chips (client-side)
    selected: null,           // selected clipID (per-clip mixer row)
    vol: { mic: 100, master: 100, monitor: 100 }, // percent (0..200 in the audio panel)
    clipGain: 100,
    micMode: "vad",
    pttHotkey: "",            // bound push-to-talk combo (e.g. "ctrl+a"); "" = unbound
    pttCapturing: false,      // true while the Record-combo capture is listening for keys
    gateSens: 15,             // percent 0..100 (Input Sensitivity manual threshold)
    gateLevel: 0,             // 0..1 from the gateLevel event
    monSrc: "clips",          // 'clips' | 'transmitted' — what the monitor plays
    // Discord Voice & Video parity state.
    nsTier: "high",           // 'none' | 'standard' | 'high' | 'strong'
    autoSens: true,           // "Automatically determine input sensitivity"
    attenAmount: 50,          // percent 0..100 (Attenuation amount)
    subsystem: "standard",    // 'standard' | 'legacy' | 'experimental' (cosmetic)
    // Processing toggles: advanced voice activity, echo cancellation, AGC, and the
    // attenuation (duck) on/off. (Noise suppression is the segmented nsTier above;
    // the old inert "force through" row is retired from the panel.)
    toggles: { advVad: true, echo: false, agc: false, duck: false },
    demoOpen: false,
    dialog: null              // dialog key or null
  };

  // ---------------------------------------------------------------------------
  // Snapshot ingest. Normalizes the server State into S, synthesizing placeholder
  // clips when the snapshot has none (skeleton build).
  // ---------------------------------------------------------------------------
  function ingest(snap) {
    S.snap = snap || FALLBACK;
    var sn = S.snap;
    S.theme = sn.theme === "light" ? "light" : "dark";
    S.cats = (sn.categories || []).slice();
    S.favorites = (sn.favorites || []).slice();

    // Volumes arrive as linear gains (1.0 = unity); the UI works in percent.
    var v = sn.volumes || {};
    S.vol = { mic: pct(v.mic), master: pct(v.master), monitor: pct(v.monitor) };

    var au = sn.audio || {};
    S.micMode = au.micMode || "vad";
    S.pttHotkey = au.pttHotkey || "";
    S.gateSens = Math.round((au.gateSensitivity != null ? au.gateSensitivity : 0.15) * 100);
    S.monSrc = au.monitorSource === "transmitted" ? "transmitted" : "clips";

    // Noise-suppression tier (segmented). Prefer the explicit tier; fall back to the
    // legacy bool (true -> standard, false -> high default) for a degraded snapshot.
    var TIERS = { none: 1, standard: 1, high: 1, strong: 1 };
    S.nsTier = TIERS[au.noiseSuppressionTier] ? au.noiseSuppressionTier
      : (au.noiseSuppression ? "standard" : "high");

    // Advanced VAD + auto-sensitivity default ON (the breathing fix): treat only an
    // explicit false as off so an unset/degraded snapshot keeps the safe default.
    S.autoSens = au.autoSensitivity !== false;
    S.attenAmount = Math.round((au.attenuationAmount != null ? au.attenuationAmount : 0.5) * 100);
    var SUBS = { standard: 1, legacy: 1, experimental: 1 };
    S.subsystem = SUBS[au.audioSubsystem] ? au.audioSubsystem : "standard";

    S.toggles = {
      advVad: au.advancedVoiceActivity !== false,
      echo: !!au.echoCancellation,
      agc: !!au.agc,
      duck: !!au.ducking
    };

    // Clips: prefer the real catalog (the normal wired path); only synthesize
    // from category counts as a defensive fallback when the snapshot carries no
    // clips[] (degraded/preview path).
    var clips = (sn.clips || []).map(function (c) {
      return { id: c.id, name: c.name || capWords(c.id.split("/").pop()), category: c.category || c.id.split("/")[0], favorite: !!c.favorite };
    });
    if (!clips.length && S.cats.length) clips = synthClips(S.cats);
    // Reflect favorites onto clips.
    var favSet = {}; S.favorites.forEach(function (id) { favSet[id] = true; });
    clips.forEach(function (c) { c.favorite = !!favSet[c.id]; });
    S.clips = clips;

    // perClip gains (linear -> percent) for the currently selected clip.
    S._perClip = {};
    var pc = sn.perClip || {};
    Object.keys(pc).forEach(function (id) { S._perClip[id] = pct(pc[id]); });
  }

  function pct(gain) {
    if (gain == null) return 100;
    return Math.round(gain * 100);
  }

  // Build readable placeholder clips for the skeleton build.
  function synthClips(cats) {
    var out = [];
    cats.forEach(function (c) {
      var words = WORDS[c.name] || [pretty(c.name)];
      for (var i = 0; i < c.count; i++) {
        var base = words[i % words.length];
        var label = i < words.length ? base : base + " " + (Math.floor(i / words.length) + 1);
        out.push({ id: c.name + "/" + i, name: capWords(label), category: c.name, favorite: false });
      }
    });
    // A small default favourites set so the pinned section demonstrates (only
    // when the server provided none and we're in the synthesized skeleton path).
    if (!S.favorites.length) {
      var byId = {}; out.forEach(function (c) { byId[c.id] = true; });
      ["games/3", "movies/1", "films/5", "reactions/2", "wow/0"].forEach(function (id) {
        if (byId[id]) S.favorites.push(id);
      });
    }
    return out;
  }

  function clipById(id) {
    for (var i = 0; i < S.clips.length; i++) if (S.clips[i].id === id) return S.clips[i];
    return null;
  }

  // ---------------------------------------------------------------------------
  // Small DOM helpers.
  // ---------------------------------------------------------------------------
  function $(id) { return document.getElementById(id); }
  function el(tag, cls, html) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (html != null) e.innerHTML = html;
    return e;
  }
  function show(node, on) { node.classList.toggle("hidden", !on); }
  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"]/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c];
    });
  }
  var PLAY_SVG = '<svg width="12" height="12" viewBox="0 0 24 24" fill="currentColor"><path d="M8 5v14l11-7z"/></svg>';

  // trackFill: the comp's two-tone range track (filled blurple to the thumb).
  function trackFill(val, max) {
    var p = Math.round((val / max) * 100);
    return "linear-gradient(90deg,var(--primary) " + p + "%,var(--elev) " + p + "%)";
  }

  // ===========================================================================
  // THEME
  // ===========================================================================
  function applyTheme() {
    var root = $("app");
    root.classList.toggle("light", S.theme === "light");
    $("theme-icon").textContent = S.theme === "dark" ? "☾" : "☀";
    $("theme-label").textContent = S.theme === "dark" ? "Dark" : "Light";
  }

  // ===========================================================================
  // SIDEBAR
  // ===========================================================================
  function renderSidebar() {
    show($("side-sound"), S.view === "sound");
    show($("side-audio"), S.view === "audio");

    if (S.view === "sound") {
      var host = $("nav-jumps");
      host.innerHTML = "";
      var secs = visibleSections();

      // Favourites jump (only when there are matching favourites).
      var favCount = visibleFavClips().length;
      if (favCount) {
        var f = el("div", "jump");
        f.innerHTML =
          '<span class="star" style="color:var(--warning);font-size:13px;">★</span>' +
          '<span class="jname" style="text-transform:none;">Favourites</span>' +
          '<span class="mono jcount">' + favCount + "</span>";
        f.addEventListener("click", function () { jumpTo("sec-fav"); });
        host.appendChild(f);
      }

      secs.forEach(function (s) {
        var row = el("div", "jump");
        row.innerHTML =
          '<span class="chip" style="background:' + chipFor(s.name) + ';"></span>' +
          '<span class="jname">' + esc(capWords(s.name)) + "</span>" +
          '<span class="mono jcount">' + s.clips.length + "</span>";
        row.addEventListener("click", function () { jumpTo("sec-" + s.name); });
        host.appendChild(row);
      });
    } else {
      // Audio sidebar: live mic status.
      var open = S.gateLevel > 0.5;
      $("mic-live-dot").classList.toggle("open", open);
      $("mic-live-label").textContent = open ? "Mic open" : "Mic closed";
      $("mic-mode-hint").textContent = modeHint(S.micMode);
    }
  }

  function jumpTo(secId) {
    var scroll = $("clip-scroll");
    var node = $(secId);
    if (node && scroll) scroll.scrollTop = node.offsetTop - 6;
  }

  // ===========================================================================
  // ROUTING BANNER + sidebar pill (3 states)
  // ===========================================================================
  // routing.state: "absent" (not detected) | "present" (detected, not engaged)
  //              | "engaged" (active).
  function renderBanner() {
    var r = S.snap.routing || {};
    var b = $("banner");
    b.innerHTML = "";
    if (r.state === "engaged") {
      b.className = "banner engaged";
      b.innerHTML =
        '<span class="b-check">✓</span>' +
        '<span class="b-text">' + esc(r.detail || "Audio routing active — Discord hears the soundboard. No Discord changes needed.") + "</span>";
    } else {
      b.className = "banner warn";
      var present = r.state === "present";
      var text = r.detail || (present
        ? "VB-CABLE detected — engage it so Discord can hear the board."
        : "VB-CABLE not detected — install it to route audio into Discord.");
      var label = present ? "Engage routing" : "Install / Fix audio routing";
      b.innerHTML =
        '<span class="b-warn">⚠</span>' +
        '<span class="b-text">' + esc(text) + "</span>" +
        '<button class="ibtn b-btn" type="button">' +
        '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="#1a1208" stroke-width="2.3" stroke-linecap="round"><path d="M12 3v12m0 0 4-4m-4 4-4-4M5 21h14"/></svg>' +
        esc(label) + "</button>";
      b.querySelector(".b-btn").addEventListener("click", onInstallRouting);
    }

    // Sidebar pill.
    var engaged = r.state === "engaged";
    var pill = $("conn-pill"), dot = $("conn-dot"), lab = $("conn-label");
    pill.style.background = engaged ? "var(--ok-bg)" : "var(--warn-bg)";
    dot.style.background = engaged ? "var(--success)" : "var(--warning)";
    dot.style.boxShadow = engaged ? "0 0 8px var(--success)" : "none";
    lab.style.color = engaged ? "var(--success)" : "var(--warning)";
    lab.textContent = engaged ? "Routing active" : "Routing needs setup";
  }

  // InstallRouting (install OR engage as appropriate). Open the matching
  // progress dialog optimistically; the real outcome arrives via installProgress
  // + routingStatus events (or the demo flow advances it).
  function onInstallRouting() {
    var r = S.snap.routing || {};
    openDialog(r.state === "present" ? "progressEngage" : "progressInstall");
    call("InstallRouting");
  }

  // ===========================================================================
  // SEARCH + sections
  // ===========================================================================
  function matchClip(c) {
    var q = S.search.trim().toLowerCase();
    if (!q) return true;
    return c.name.toLowerCase().indexOf(q) !== -1 || c.category.toLowerCase().indexOf(q) !== -1;
  }

  // Sections in category order (as the catalog/snapshot provides them), each
  // with its matching clips. Empty sections are dropped.
  function visibleSections() {
    var order = S.cats.map(function (c) { return c.name; });
    var byCat = {};
    S.clips.forEach(function (c) {
      if (!matchClip(c)) return;
      (byCat[c.category] || (byCat[c.category] = [])).push(c);
    });
    // Include any category present in clips but not in the cats list (defensive).
    Object.keys(byCat).forEach(function (k) { if (order.indexOf(k) === -1) order.push(k); });
    var out = [];
    order.forEach(function (name) {
      if (byCat[name] && byCat[name].length) out.push({ name: name, clips: byCat[name] });
    });
    return out;
  }

  function visibleFavClips() {
    return S.favorites.map(clipById).filter(function (c) { return c && matchClip(c); });
  }

  // ===========================================================================
  // CLIP GRID
  // ===========================================================================
  function tileNode(c) {
    var fav = S.favorites.indexOf(c.id) !== -1;
    var t = el("div", "tile");
    t.innerHTML =
      '<span class="accent" style="background:' + chipFor(c.category) + ';"></span>' +
      '<span class="tplay">' + PLAY_SVG + "</span>" +
      '<span class="tname">' + esc(c.name) + "</span>" +
      '<span class="star" style="color:' + (fav ? "var(--warning)" : "var(--faint)") + ';">' + (fav ? "★" : "☆") + "</span>";
    t.addEventListener("click", function () { playClip(c); });
    t.querySelector(".star").addEventListener("click", function (e) {
      e.stopPropagation();
      toggleFavorite(c.id);
    });
    return t;
  }

  function renderSections() {
    var host = $("sections");
    host.innerHTML = "";
    var secs = visibleSections();
    var favClips = visibleFavClips();
    var noResults = !secs.length && !favClips.length;
    show($("no-results"), noResults);

    // Favourites section (pinned at top).
    if (favClips.length && !noResults) {
      var fh = el("div", "sec-head fav");
      fh.id = "sec-fav";
      fh.innerHTML =
        '<span class="sec-star">★</span>' +
        '<span class="sec-name fav">Favourites</span>' +
        '<span class="sec-count">' + favClips.length + "</span>";
      host.appendChild(fh);
      var fg = el("div", "grid");
      favClips.forEach(function (c) { fg.appendChild(tileNode(c)); });
      host.appendChild(fg);
    }

    secs.forEach(function (s) {
      var h = el("div", "sec-head");
      h.id = "sec-" + s.name;
      h.innerHTML =
        '<span class="sec-chip" style="background:' + chipFor(s.name) + ';"></span>' +
        '<span class="sec-name">' + esc(capWords(s.name)) + "</span>" +
        '<span class="sec-count">' + s.clips.length + "</span>" +
        '<span class="sec-rule"></span>';
      host.appendChild(h);
      var g = el("div", "grid");
      s.clips.forEach(function (c) { g.appendChild(tileNode(c)); });
      host.appendChild(g);
    });
  }

  // ===========================================================================
  // PLAY / STOP / FAVOURITE / NOW PLAYING
  // ===========================================================================
  function playClip(c) {
    call("Play", c.id);
    S.selected = c.id;
    if (S._perClip[c.id] != null) S.clipGain = S._perClip[c.id];
    else S.clipGain = 100;
    // Push to now-playing chips (most-recent-first, capped at 5).
    S.playing = [{ id: c.id, name: c.name, chip: chipFor(c.category) }]
      .concat(S.playing.filter(function (p) { return p.id !== c.id; }))
      .slice(0, 5);
    renderNowPlaying();
    renderStopBtn();
    renderMixer();
  }

  function stopOne(id) {
    S.playing = S.playing.filter(function (p) { return p.id !== id; });
    renderNowPlaying();
    renderStopBtn();
  }

  function stopAll() {
    call("StopAll");
    S.playing = [];
    renderNowPlaying();
    renderStopBtn();
  }

  function toggleFavorite(id) {
    var p = call("ToggleFavorite", id);
    var idx = S.favorites.indexOf(id);
    var willBeFav = idx === -1;
    // Optimistic local update; reconcile with the bound return if present.
    if (willBeFav) S.favorites.push(id); else S.favorites.splice(idx, 1);
    var c = clipById(id); if (c) c.favorite = willBeFav;
    var finish = function () { renderSections(); renderSidebar(); };
    if (p && p.then) {
      p.then(function (server) {
        if (typeof server === "boolean" && server !== willBeFav) {
          // Server disagreed; trust it.
          var j = S.favorites.indexOf(id);
          if (server && j === -1) S.favorites.push(id);
          if (!server && j !== -1) S.favorites.splice(j, 1);
          if (c) c.favorite = server;
        }
        finish();
      }).catch(finish);
    } else { finish(); }
  }

  function renderNowPlaying() {
    show($("nowplaying"), S.playing.length > 0);
    var host = $("np-chips");
    host.innerHTML = "";
    S.playing.forEach(function (p) {
      var chip = el("div", "chiprow");
      chip.innerHTML =
        '<span class="eq"><span style="height:50%;"></span><span style="height:80%;"></span><span style="height:40%;"></span></span>' +
        '<span class="np-name">' + esc(p.name) + "</span>" +
        '<span class="chipx">✕</span>';
      chip.querySelector(".chipx").addEventListener("click", function () { stopOne(p.id); });
      host.appendChild(chip);
    });
  }

  function renderStopBtn() {
    var n = S.playing.length;
    var btn = $("stop-all");
    btn.classList.toggle("armed", n > 0);
    $("stop-label").textContent = n > 0 ? "Stop · " + n : "Stop";
  }

  // ===========================================================================
  // MIXER DOCK
  // ===========================================================================
  function micIcon(p) {
    return '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="' + p + '"/></svg>';
  }
  var MIX_ICONS = {
    mic: "M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3zM19 10v2a7 7 0 0 1-14 0v-2M12 19v3",
    master: "M3 11v2a1 1 0 0 0 1 1h3l4 4V6L7 10H4a1 1 0 0 0-1 1zM16 8a5 5 0 0 1 0 8M19 5a9 9 0 0 1 0 14",
    monitor: "M3 14v-2a9 9 0 0 1 18 0v2M21 14v3a2 2 0 0 1-2 2h-1v-6h1a2 2 0 0 1 2 1zM3 14a2 2 0 0 1 2-1h1v6H5a2 2 0 0 1-2-2z",
    clip: "M9 18V5l11-2v13M9 9l11-2"
  };

  function volRowNode(opts) {
    // opts: { key, kind('mic'|'master'|'monitor'|'clip'), label, value, disabled,
    //         muted, onInput }
    var wrap = el("div", "vol" + (opts.disabled ? " disabled" : ""));
    var hot = opts.value > 100 && !opts.disabled;
    var pctTxt = opts.disabled ? "—" : opts.value + "%";
    wrap.innerHTML =
      '<div class="v-head">' +
      '<span class="v-icon' + (opts.disabled ? " off" : "") + '">' + micIcon(MIX_ICONS[opts.kind]) + "</span>" +
      '<span class="v-label' + (opts.muted ? " muted" : "") + '">' + esc(opts.label) + "</span>" +
      '<span class="v-pct' + (hot ? " hot" : (opts.disabled ? " off" : "")) + '">' + pctTxt + "</span>" +
      "</div>" +
      '<input type="range" class="rng" min="0" max="150" step="1" value="' + opts.value + '"' +
      (opts.disabled ? " disabled" : "") + ' style="width:100%;background:' +
      (opts.disabled ? "var(--elev)" : trackFill(opts.value, 150)) + ';">';
    var input = wrap.querySelector("input");
    if (!opts.disabled) {
      input.addEventListener("input", function () {
        var v = +input.value;
        opts.onInput(v);
        // Live track + percent without a full re-render (smooth dragging).
        input.style.background = trackFill(v, 150);
        var pctEl = wrap.querySelector(".v-pct");
        pctEl.textContent = v + "%";
        pctEl.classList.toggle("hot", v > 100);
      });
    }
    return wrap;
  }

  function renderMixer() {
    var host = $("mixer-grid");
    host.innerHTML = "";

    host.appendChild(volRowNode({
      kind: "mic", label: "Mic — your voice", value: S.vol.mic,
      onInput: function (v) { S.vol.mic = v; call("SetVolume", "mic", v / 100); }
    }));
    host.appendChild(volRowNode({
      kind: "master", label: "Others hear", value: S.vol.master,
      onInput: function (v) { S.vol.master = v; call("SetVolume", "master", v / 100); }
    }));
    host.appendChild(volRowNode({
      kind: "monitor", label: "You hear", value: S.vol.monitor,
      onInput: function (v) { S.vol.monitor = v; call("SetVolume", "monitor", v / 100); }
    }));

    var sel = S.selected ? clipById(S.selected) : null;
    host.appendChild(volRowNode({
      kind: "clip",
      label: sel ? "Clip: " + sel.name : "No clip selected",
      value: S.clipGain, disabled: !sel, muted: !sel,
      onInput: function (v) {
        S.clipGain = v;
        if (sel) { S._perClip[sel.id] = v; call("SetClipVolume", sel.id, v / 100); }
      }
    }));
  }

  // ===========================================================================
  // CONFIDENCE MONITOR — "What you hear" segmented control
  // ===========================================================================
  // Two captions explain the trade-off. "Clips only" is the default and what the
  // app has always done; "Exactly what Discord hears" routes the EXACT cable-bound
  // mix (your processed mic + clips) to your headphones so you can audit the
  // transmitted quality. The CAVEAT (slight delay; echo-y over open speakers
  // because you also hear your own voice acoustically; best with headphones) is in
  // the caption — it is EXPECTED and it IS what others hear.
  var MONSRC_CAP = {
    clips: "Your headphones play the clips only — you hear your own voice naturally. " +
      "Nothing extra is sent to Discord.",
    transmitted: "Your headphones play the EXACT signal sent to Discord — your processed " +
      "mic plus clips — with a small delay, so you can audition the transmitted quality. " +
      "Expect a slight echo of your own voice (it’s delayed against your natural voice); " +
      "best with headphones. This does not change what Discord receives."
  };

  function renderMonSource() {
    var seg = $("monsrc-seg");
    if (!seg) return;
    var btns = seg.querySelectorAll(".seg-btn");
    for (var i = 0; i < btns.length; i++) {
      var on = btns[i].getAttribute("data-src") === S.monSrc;
      btns[i].classList.toggle("on", on);
      btns[i].setAttribute("aria-pressed", on ? "true" : "false");
    }
    var cap = $("monsrc-cap");
    if (cap) cap.textContent = MONSRC_CAP[S.monSrc] || MONSRC_CAP.clips;
  }

  function setMonSource(src) {
    src = src === "transmitted" ? "transmitted" : "clips";
    if (src === S.monSrc) return;
    S.monSrc = src;
    call("SetMonitorSource", src);
    renderMonSource();
  }

  // ===========================================================================
  // AUDIO VIEW — modes, gate, ring meter, toggles, checklist
  // ===========================================================================
  var MODE_META = {
    vad: ["Voice-activated", "opens when you speak"],
    ptt: ["Push-to-talk", "hold a key to talk"],
    always: ["Always-on", "mic always live"],
    mute: ["Mute", "mic off"]
  };
  function modeHint(m) {
    var sub = (MODE_META[m] || ["", ""])[1];
    return sub.charAt(0).toUpperCase() + sub.slice(1) + ".";
  }

  function renderModes() {
    var host = $("mode-grid");
    host.innerHTML = "";
    ["vad", "ptt", "always", "mute"].forEach(function (v) {
      var on = S.micMode === v;
      var m = MODE_META[v];
      var b = el("button", "mode" + (on ? " on" : ""));
      b.type = "button";
      b.innerHTML =
        '<span class="m-dot"></span>' +
        '<span class="m-txt"><span class="m-name">' + m[0] + '</span><span class="m-sub">' + m[1] + "</span></span>";
      b.addEventListener("click", function () {
        S.micMode = v;
        call("SetMicMode", v);
        renderModes();
        show($("ptt-block"), v === "ptt");
        renderSidebar();
      });
      host.appendChild(b);
    });
    show($("ptt-block"), S.micMode === "ptt");
    renderPTT();
  }

  // PUSH-TO-TALK key binding. The label shows the live combo bound on the backend
  // (App.SetPTTHotkey re-registers it instantly); the Record button captures the
  // next modifier+key chord and binds it. Only keys the backend's parser accepts
  // (letters, digits, F1–F12, plus Ctrl/Alt/Shift/Win modifiers) are captured.
  var PTT_HINT = "Hold this combo to talk in push-to-talk mode. Applies instantly · saved automatically.";

  // prettyCombo turns a stored combo ("ctrl+shift+a") into a display label
  // ("Ctrl + Shift + A"); an empty combo shows a dash.
  function prettyCombo(combo) {
    if (!combo) return "—";
    return combo.split("+").map(function (t) {
      t = t.trim().toLowerCase();
      if (t === "ctrl" || t === "control") return "Ctrl";
      if (t === "alt") return "Alt";
      if (t === "shift") return "Shift";
      if (t === "win" || t === "super" || t === "meta" || t === "cmd") return "Win";
      return t.toUpperCase();
    }).join(" + ");
  }

  function renderPTT() {
    if (S.pttCapturing) return; // leave the live "Press a combo…" prompt alone
    var key = $("ptt-key");
    if (key) key.textContent = prettyCombo(S.pttHotkey);
  }

  // pttKeyToken maps a KeyboardEvent to the non-modifier token the backend parser
  // understands, using e.code so it is keyboard-layout robust. Returns null for
  // modifier-only presses and for keys the parser does not accept (so capture keeps
  // listening until a usable key arrives).
  function pttKeyToken(e) {
    var code = e.code || "";
    var m;
    if ((m = /^Key([A-Z])$/.exec(code))) return m[1].toLowerCase();
    if ((m = /^Digit([0-9])$/.exec(code))) return m[1];
    if (/^F([1-9]|1[0-2])$/.test(code)) return code.toLowerCase();
    return null;
  }

  // startPTTCapture listens for the next chord and binds it via SetPTTHotkey. Esc
  // cancels; Backspace/Delete clears the binding. Capture is a one-shot: the handler
  // detaches as soon as a usable chord (or cancel/clear) arrives.
  function startPTTCapture() {
    if (S.pttCapturing) return;
    S.pttCapturing = true;
    var btn = $("ptt-rec"), key = $("ptt-key"), hint = $("ptt-hint");
    if (btn) btn.textContent = "Press keys…";
    if (key) key.textContent = "Press a combo…";
    if (hint) hint.textContent = "Press a modifier + key. Esc cancels · Backspace clears.";

    function finish() {
      S.pttCapturing = false;
      document.removeEventListener("keydown", onKey, true);
      if (btn) btn.textContent = "Record combo…";
      if (hint) hint.textContent = PTT_HINT;
      renderPTT();
    }

    function onKey(e) {
      e.preventDefault();
      e.stopPropagation();
      if (e.key === "Escape") { finish(); return; }
      if (e.key === "Backspace" || e.key === "Delete") {
        S.pttHotkey = "";
        call("SetPTTHotkey", "");
        finish();
        return;
      }
      var tok = pttKeyToken(e);
      if (!tok) return; // modifier-only or unsupported key — keep listening
      var mods = [];
      if (e.ctrlKey) mods.push("ctrl");
      if (e.altKey) mods.push("alt");
      if (e.shiftKey) mods.push("shift");
      if (e.metaKey) mods.push("win");
      S.pttHotkey = mods.concat(tok).join("+");
      call("SetPTTHotkey", S.pttHotkey);
      finish();
    }

    document.addEventListener("keydown", onKey, true);
  }

  // INPUT SENSITIVITY — Discord's "Input Sensitivity": an Automatic toggle plus a
  // manual threshold slider. When Automatic is on, the manual slider is dimmed and
  // the threshold tracks the noise floor (engine-side); when off, the slider sets it.
  // The slider value is reused as the VAD open-probability bias when Advanced Voice
  // Activity is on, and as the energy threshold otherwise.
  function renderInputSensitivity() {
    // Automatic toggle.
    var at = $("autosens-toggle");
    if (at) {
      at.classList.toggle("on", S.autoSens);
      at.setAttribute("aria-checked", S.autoSens ? "true" : "false");
      var box = at.querySelector(".box");
      if (box) box.textContent = S.autoSens ? "✓" : "";
    }
    // Manual threshold slider (dimmed + disabled while Automatic is on).
    var g = $("gate");
    if (g) {
      g.value = S.gateSens;
      g.disabled = S.autoSens;
      g.style.background = S.autoSens ? "var(--elev)" : trackFill(S.gateSens, 100);
    }
    var gv = $("gate-val");
    if (gv) {
      gv.textContent = S.autoSens ? "Auto" : S.gateSens + "%";
      gv.classList.toggle("off", S.autoSens);
    }
  }

  // NOISE SUPPRESSION — segmented None / Standard / High / Strong (one tier at a
  // time; never stacked). Mirrors Discord's None / Standard / Krisp selector mapped
  // onto our engine: Standard/High = the WebRTC APM noise suppressor at two
  // strengths, Strong = RNNoise (our closest non-proprietary, Krisp-like option).
  var NSTIER_CAP = {
    none: "No noise suppression. Discord hears your raw mic — background noise and breathing included.",
    standard: "WebRTC noise suppression at a moderate level — Discord’s “Standard”. Trims steady background noise while staying transparent.",
    high: "WebRTC noise suppression at a high level (the default). Aggressive enough to shave breathing that rides under your voice.",
    strong: "RNNoise — our closest non-proprietary, Krisp-like denoiser. The most aggressive removal of breathing, keyboard and fan noise; the APM suppressor is bypassed so the two never stack."
  };
  function renderNSTier() {
    var seg = $("nstier-seg");
    if (!seg) return;
    var btns = seg.querySelectorAll(".seg-btn");
    for (var i = 0; i < btns.length; i++) {
      var on = btns[i].getAttribute("data-tier") === S.nsTier;
      btns[i].classList.toggle("on", on);
      btns[i].setAttribute("aria-pressed", on ? "true" : "false");
    }
    var cap = $("nstier-cap");
    if (cap) cap.textContent = NSTIER_CAP[S.nsTier] || NSTIER_CAP.high;
  }
  function setNSTier(tier) {
    if (!NSTIER_CAP[tier] || tier === S.nsTier) return;
    S.nsTier = tier;
    call("SetNoiseSuppressionTier", tier);
    renderNSTier();
  }

  // Processing toggles: Advanced voice activity (the breathing-fix VAD), Echo
  // cancellation, Automatic gain control. (Noise suppression is the segmented tier
  // above; Attenuation has its own card; the inert "Force through" row is retired.)
  var TOGGLE_META = [
    ["advVad", "Advanced voice activity", "Opens the gate on real speech, not just loud sound — keeps your breathing OFF the call. Discord’s Krisp analog. Recommended ON.", "SetAdvancedVoiceActivity"],
    ["echo", "Echo cancellation", "Cancels speaker bleed picked up by your mic.", "SetEchoCancellation"],
    ["agc", "Automatic gain control", "Evens out your volume automatically.", "SetAGC"]
  ];

  function renderToggles() {
    var host = $("toggle-grid");
    host.innerHTML = "";
    TOGGLE_META.forEach(function (t) {
      var key = t[0], on = !!S.toggles[key];
      var row = el("div", "toggle" + (on ? " on" : ""));
      row.innerHTML =
        '<span class="box">' + (on ? "✓" : "") + "</span>" +
        '<div class="t-body"><div class="t-name">' + t[1] + '</div><div class="t-desc">' + esc(t[2]) + "</div></div>";
      row.addEventListener("click", function () {
        var next = !S.toggles[key];
        S.toggles[key] = next;
        call(t[3], next);
        renderToggles();
      });
      host.appendChild(row);
    });
  }

  // ATTENUATION — Discord's "lower other sounds while talking": a toggle (Ducking)
  // plus an amount slider (the duck depth). The amount block dims when off.
  function renderAttenuation() {
    var t = $("atten-toggle");
    if (t) {
      t.classList.toggle("on", S.toggles.duck);
      t.setAttribute("aria-checked", S.toggles.duck ? "true" : "false");
      var box = t.querySelector(".box");
      if (box) box.textContent = S.toggles.duck ? "✓" : "";
    }
    var block = $("atten-amount-block");
    if (block) block.classList.toggle("disabled", !S.toggles.duck);
    var a = $("atten-amount");
    if (a) {
      a.value = S.attenAmount;
      a.disabled = !S.toggles.duck;
      a.style.background = S.toggles.duck ? trackFill(S.attenAmount, 100) : "var(--elev)";
    }
    var av = $("atten-val");
    if (av) av.textContent = S.attenAmount + "%";
  }

  // INPUT / OUTPUT VOLUME — Discord parity sliders (0–200%). Input drives the mic
  // gain into Discord (the same channel as the mixer "Mic" slider); output drives
  // the local monitor gain (what YOU hear). They share S.vol so the mixer dock and
  // this panel stay in sync.
  function renderAudioVolumes() {
    var pairs = [
      ["vol-input", "vol-input-val", "mic", "SetInputVolume"],
      ["vol-output", "vol-output-val", "monitor", "SetOutputVolume"]
    ];
    pairs.forEach(function (p) {
      var slider = $(p[0]);
      if (!slider) return;
      var val = S.vol[p[2]];
      slider.value = val;
      slider.style.background = trackFill(val, 200);
      var lab = $(p[1]);
      if (lab) { lab.textContent = val + "%"; lab.classList.toggle("hot", val > 100); }
    });
  }

  // AUDIO SUBSYSTEM — cosmetic Standard/Legacy/Experimental dropdown (parity).
  function renderSubsystem() {
    var sel = $("subsystem-select");
    if (sel) sel.value = S.subsystem;
  }

  var CHECKLIST = [
    "Set Discord input to “CABLE Output”",
    "Turn OFF Krisp noise suppression",
    "Turn OFF echo cancellation & AGC",
    "Turn OFF automatic input sensitivity"
  ];
  function renderChecklist() {
    var host = $("check-grid");
    if (host.childElementCount) return; // static — render once
    CHECKLIST.forEach(function (text) {
      var item = el("div", "check-item");
      item.innerHTML =
        '<span class="tick"><svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="var(--success)" stroke-width="3" stroke-linecap="round" stroke-linejoin="round"><path d="M20 6 9 17l-5-5"/></svg></span>' +
        esc(text);
      host.appendChild(item);
    });
  }

  // Ring meter — driven by S.gateLevel (0..1) from the gateLevel event.
  function renderMeter() {
    var lvl = S.gateLevel;
    var open = lvl > 0.5;
    var deg = Math.round(lvl * 360);
    var ringCol = open ? "var(--success-glow)" : "var(--primary)";
    var ring = $("ring");
    ring.style.background = "conic-gradient(from -90deg, " + ringCol + " " + deg + "deg, var(--elev) " + deg + "deg)";
    ring.style.boxShadow = open ? "0 0 26px rgba(98,239,147,.45)" : "none";

    $("ring-mic").classList.toggle("open", open);
    var rs = $("ring-state");
    rs.textContent = open ? "OPEN" : "closed";
    rs.classList.toggle("open", open);

    var fill = $("meter-fill");
    fill.style.width = Math.round(lvl * 100) + "%";
    fill.classList.toggle("open", open);

    // The Input-sensitivity card carries a second live meter so the user can set the
    // threshold against the live signal (Discord shows the same bar under the slider).
    var sfill = $("sens-fill");
    if (sfill) {
      sfill.style.width = Math.round(lvl * 100) + "%";
      sfill.classList.toggle("open", open);
    }
  }

  // ===========================================================================
  // DIALOG (install / engage progress + outcomes)
  // ===========================================================================
  // Mirrors the comp's dialog flows. installProgress events advance/replace these.
  function openDialog(key) {
    S.dialog = key;
    renderDialog();
  }
  function closeDialog() { S.dialog = null; renderDialog(); }

  // restartApp re-launches SoundBoard via the bound App.Restart, which re-execs
  // the binary and quits this process. The freshly installed VB-CABLE is only
  // picked up by a new process (the running one initialized its audio context
  // before the cable existed), so this is the one-click path to finish routing
  // after an install. We close the dialog first for immediate feedback; the
  // process exits a moment later when the backend teardown completes.
  function restartApp() {
    closeDialog();
    call("Restart");
  }

  function renderDialog() {
    var overlay = $("dialog-overlay");
    if (!S.dialog) { show(overlay, false); return; }
    show(overlay, true);

    var spinner = false, title = "", body = "", err = false, btns = [];
    var d = S.dialog;
    if (d === "progressInstall") {
      spinner = true; title = "Installing VB-CABLE";
      body = "Approve the Windows UAC elevation prompt to continue. This installs the virtual audio device.";
      btns = []; // progress is event-driven; no manual button
    } else if (d === "progressEngage") {
      spinner = true; title = "Engaging routing";
      body = "Routing the soundboard into your microphone path…";
      btns = [];
    } else if (d === "error") {
      err = true; title = "Install failed";
      body = "Could not locate the CABLE Output device. Try reinstalling VB-CABLE, then retry.";
      btns = [{ label: "OK", kind: "primary", on: closeDialog }];
    } else if (d === "engageSuccess") {
      title = "Routing engaged";
      body = "The soundboard is now mixed into your microphone — others in your call can hear it.";
      btns = [{ label: "Nice", kind: "primary", on: closeDialog }];
    } else if (d === "installSuccess") {
      title = "VB-CABLE installed";
      body = "Installation complete. SoundBoard needs to restart to finish wiring the audio routing.";
      btns = [
        { label: "Later", kind: "secondary", on: closeDialog },
        { label: "Restart app", kind: "primary", on: restartApp }
      ];
    }

    show($("dialog-spinner"), spinner);
    var t = $("dialog-title");
    t.textContent = title;
    t.classList.toggle("err", err);
    $("dialog-body").textContent = body;

    var host = $("dialog-btns");
    host.innerHTML = "";
    btns.forEach(function (b) {
      var btn = el("button", "ibtn d-btn " + b.kind, esc(b.label));
      btn.type = "button";
      btn.addEventListener("click", b.on);
      host.appendChild(btn);
    });
  }

  // ===========================================================================
  // SETTINGS (⚙) PREVIEW POPOVER — drive banner state + dialogs (QA aid)
  // ===========================================================================
  function renderDemoPop() {
    show($("demo-pop"), S.demoOpen);
    if (!S.demoOpen) return;

    var bhost = $("demo-banner");
    bhost.innerHTML = "";
    [["absent", "Cable absent"], ["present", "Cable detected"], ["engaged", "Engaged / ready"]].forEach(function (pair) {
      var cur = (S.snap.routing || {}).state === pair[0];
      var row = el("div", "row" + (cur ? " active" : ""), esc(pair[1]));
      row.addEventListener("click", function () {
        S.snap.routing = { state: pair[0], detail: "", canEngage: pair[0] === "present" };
        S.demoOpen = false;
        renderBanner();
        renderDemoPop();
      });
      bhost.appendChild(row);
    });

    var dhost = $("demo-dialogs");
    dhost.innerHTML = "";
    [["progressInstall", "Install progress"], ["error", "Error"],
     ["engageSuccess", "Engage success"], ["installSuccess", "Install success"]].forEach(function (pair) {
      var row = el("div", "row", esc(pair[1]));
      row.addEventListener("click", function () { S.demoOpen = false; renderDemoPop(); openDialog(pair[0]); });
      dhost.appendChild(row);
    });
  }

  // ===========================================================================
  // VIEW SWITCH
  // ===========================================================================
  function setView(view) {
    S.view = view;
    $("nav-sound").classList.toggle("active", view === "sound");
    $("nav-audio").classList.toggle("active", view === "audio");
    show($("view-sound"), view === "sound");
    show($("view-audio"), view === "audio");
    renderSidebar();
  }

  // ===========================================================================
  // FULL RENDER
  // ===========================================================================
  function renderAll() {
    applyTheme();
    renderBanner();
    renderSections();
    renderSidebar();
    renderNowPlaying();
    renderStopBtn();
    renderMixer();
    renderMonSource();
    renderModes();
    renderPTT();
    renderInputSensitivity();
    renderNSTier();
    renderToggles();
    renderAttenuation();
    renderAudioVolumes();
    renderSubsystem();
    renderChecklist();
    renderMeter();
    renderDialog();
    // search placeholder count from the snapshot total
    var total = S.clips.length || 212;
    $("search").placeholder = "Search " + total + " clips by name or category…";
  }

  // ===========================================================================
  // WIRING
  // ===========================================================================
  function wire() {
    $("nav-sound").addEventListener("click", function () { setView("sound"); });
    $("nav-audio").addEventListener("click", function () { setView("audio"); });

    $("theme-toggle").addEventListener("click", function () {
      S.theme = S.theme === "dark" ? "light" : "dark";
      applyTheme();
      call("SetTheme", S.theme);
    });

    // Settings (⚙) preview popover.
    $("demo-toggle").addEventListener("click", function (e) {
      e.stopPropagation();
      S.demoOpen = !S.demoOpen;
      renderDemoPop();
    });
    document.addEventListener("click", function () {
      if (S.demoOpen) { S.demoOpen = false; renderDemoPop(); }
    });
    $("demo-pop").addEventListener("click", function (e) { e.stopPropagation(); });

    // Window controls.
    $("win-min").addEventListener("click", function () { call("Minimize"); });
    var hide = function () { call("HideToTray"); };
    $("win-tray").addEventListener("click", hide);
    $("win-close").addEventListener("click", hide);
    $("foot-tray").addEventListener("click", hide);
    $("foot-quit").addEventListener("click", function () { call("Quit"); });

    // Search.
    var search = $("search");
    search.addEventListener("input", function () {
      S.search = search.value;
      show($("search-clear"), !!S.search);
      renderSections();
      renderSidebar();
    });
    $("search-clear").addEventListener("click", function () {
      S.search = ""; search.value = "";
      show($("search-clear"), false);
      renderSections(); renderSidebar();
    });

    // Stop-all.
    $("stop-all").addEventListener("click", stopAll);

    // Confidence monitor — "What you hear" segmented control.
    var monSeg = $("monsrc-seg");
    if (monSeg) {
      monSeg.addEventListener("click", function (e) {
        var btn = e.target.closest ? e.target.closest(".seg-btn") : null;
        if (btn) setMonSource(btn.getAttribute("data-src"));
      });
    }

    // Input-sensitivity manual threshold slider (the gate). Ignored while Automatic
    // is on (the slider is disabled, but guard anyway). Reused as the VAD open-prob
    // bias when Advanced Voice Activity is on.
    var gate = $("gate");
    gate.addEventListener("input", function () {
      if (S.autoSens) return;
      S.gateSens = +gate.value;
      gate.style.background = trackFill(S.gateSens, 100);
      $("gate-val").textContent = S.gateSens + "%";
      call("SetGateSensitivity", S.gateSens / 100);
    });

    // Automatically determine input sensitivity (Discord parity) toggle.
    var autoSens = $("autosens-toggle");
    if (autoSens) {
      autoSens.addEventListener("click", function () {
        S.autoSens = !S.autoSens;
        call("SetAutoSensitivity", S.autoSens);
        renderInputSensitivity();
      });
    }

    // Push-to-talk "Record combo…" button — capture the next chord and bind it live.
    var pttRec = $("ptt-rec");
    if (pttRec) {
      pttRec.addEventListener("click", function (e) {
        e.stopPropagation();
        startPTTCapture();
      });
    }

    // Noise-suppression segmented selector (None / Standard / High / Strong).
    var nsSeg = $("nstier-seg");
    if (nsSeg) {
      nsSeg.addEventListener("click", function (e) {
        var btn = e.target.closest ? e.target.closest(".seg-btn") : null;
        if (btn) setNSTier(btn.getAttribute("data-tier"));
      });
    }

    // Attenuation toggle (Ducking on/off).
    var attenToggle = $("atten-toggle");
    if (attenToggle) {
      attenToggle.addEventListener("click", function () {
        S.toggles.duck = !S.toggles.duck;
        call("SetDucking", S.toggles.duck);
        renderAttenuation();
      });
    }

    // Attenuation amount slider (duck depth).
    var atten = $("atten-amount");
    if (atten) {
      atten.addEventListener("input", function () {
        if (!S.toggles.duck) return;
        S.attenAmount = +atten.value;
        atten.style.background = trackFill(S.attenAmount, 100);
        $("atten-val").textContent = S.attenAmount + "%";
        call("SetAttenuationAmount", S.attenAmount / 100);
      });
    }

    // Input / Output volume sliders (0–200%).
    var volInput = $("vol-input");
    if (volInput) {
      volInput.addEventListener("input", function () {
        var v = +volInput.value;
        S.vol.mic = v;
        volInput.style.background = trackFill(v, 200);
        var lab = $("vol-input-val");
        lab.textContent = v + "%"; lab.classList.toggle("hot", v > 100);
        call("SetInputVolume", v / 100);
      });
    }
    var volOutput = $("vol-output");
    if (volOutput) {
      volOutput.addEventListener("input", function () {
        var v = +volOutput.value;
        S.vol.monitor = v;
        volOutput.style.background = trackFill(v, 200);
        var lab = $("vol-output-val");
        lab.textContent = v + "%"; lab.classList.toggle("hot", v > 100);
        call("SetOutputVolume", v / 100);
      });
    }

    // Audio subsystem dropdown (cosmetic parity).
    var subsystem = $("subsystem-select");
    if (subsystem) {
      subsystem.addEventListener("change", function () {
        S.subsystem = subsystem.value;
        call("SetAudioSubsystem", S.subsystem);
      });
    }
  }

  // ===========================================================================
  // LIVE EVENTS (Go -> JS)
  // ===========================================================================
  function subscribeEvents() {
    var r = rt();
    if (!r || !r.EventsOn) return;

    // gateLevel: ~15-30 Hz mic-open level [0..1]. Drives the ring meter and the
    // sidebar mic-status dot. Throttled to animation frames so a 30 Hz feed
    // never thrashes layout.
    var pendingLevel = null, rafQueued = false;
    r.EventsOn("gateLevel", function (p) {
      var lvl = p && typeof p.level === "number" ? p.level : (typeof p === "number" ? p : 0);
      pendingLevel = Math.max(0, Math.min(1, lvl));
      if (!rafQueued) {
        rafQueued = true;
        requestAnimationFrame(function () {
          rafQueued = false;
          S.gateLevel = pendingLevel;
          if (S.view === "audio") renderMeter();
          // keep the sidebar mic dot honest even off the audio view
          var open = S.gateLevel > 0.5;
          var dot = $("mic-live-dot");
          if (dot) {
            dot.classList.toggle("open", open);
            $("mic-live-label").textContent = open ? "Mic open" : "Mic closed";
          }
        });
      }
    });

    // routingStatus: {state, detail, canEngage} — refresh banner + pill live, and
    // advance any open progress dialog to its success state.
    r.EventsOn("routingStatus", function (status) {
      if (!status) return;
      S.snap.routing = status;
      renderBanner();
      if (status.state === "engaged") {
        if (S.dialog === "progressInstall") openDialog("installSuccess");
        else if (S.dialog === "progressEngage") openDialog("engageSuccess");
      }
    });

    // installProgress: {msg, done, err} — drive the dialog body/outcome.
    r.EventsOn("installProgress", function (p) {
      p = p || {};
      if (p.err) { openDialog("error"); $("dialog-body").textContent = p.err; return; }
      if (S.dialog === "progressInstall" || S.dialog === "progressEngage") {
        if (p.msg) $("dialog-body").textContent = p.msg;
        if (p.done) {
          // Outcome is finalized by the routingStatus event; if none arrives,
          // fall back to the install-success card after a done install.
          if (S.dialog === "progressInstall") openDialog("installSuccess");
          else openDialog("engageSuccess");
        }
      }
    });
  }

  // ===========================================================================
  // BOOT
  // ===========================================================================
  function boot() {
    wire();
    subscribeEvents();
    var p = call("GetState");
    if (p && p.then) {
      p.then(function (snap) { ingest(snap); renderAll(); })
       .catch(function () { ingest(FALLBACK); renderAll(); });
    } else {
      ingest(FALLBACK);
      renderAll();
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }
})();
