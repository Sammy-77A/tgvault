"use strict";

// tgmoviebox: the library app's screens. A Home tab to browse movies and
// shows, a Downloads tab for what's on this phone and what was asked for, a
// details page for each title. Nothing streams: a video plays only after it
// has been downloaded to the phone. Uses the helpers in app.js (api, el, dialog…).

const MB = {
  stack: [],       // screens, newest last: {name, params}
  tab: "home",
  info: null,
  lastTasks: "",   // which downloads were running at the last look
  loaded: false,
};

const ICONS = {
  search: '<path d="M10 4a6 6 0 1 0 3.7 10.7l4.8 4.8 1.4-1.4-4.8-4.8A6 6 0 0 0 10 4zm0 2a4 4 0 1 1 0 8 4 4 0 0 1 0-8z"/>',
  gear: '<circle cx="12" cy="5" r="2"/><circle cx="12" cy="12" r="2"/><circle cx="12" cy="19" r="2"/>',
  back: '<path d="M15.5 5 8.500 12l7 7 1.400-1.400L11.300 12l5.600-5.600z"/>',
  play: '<path d="M8 5v14l11-7z"/>',
  down: '<path d="M11 4h2v8.200l3.100-3.100 1.400 1.400-5.500 5.500-5.500-5.500 1.400-1.400L11 12.200zM5 18h14v2H5z"/>',
  check: '<path d="m9.500 16.200-3.700-3.700L4.400 14l5.100 5.100L20.600 8l-1.400-1.400z"/>',
  clock: '<path d="M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zm1 4h-2v6l4.700 2.800 1-1.600L13 12.500z"/>',
  trash: '<path d="M7 5V3h10v2h4v2H3V5zm1 4h8l-.6 11H8.600zM6 9h2l.5 11h7L16 9h2l-.7 13H6.700z"/>',
  star: '<path d="m12 3 2.700 5.600 6.100.9-4.400 4.300 1 6.100L12 17l-5.400 2.900 1-6.100L3.200 9.500l6.100-.9z"/>',
  close: '<path d="m6.400 5 5.600 5.600L17.600 5 19 6.400 13.400 12l5.600 5.600-1.400 1.400-5.600-5.600L6.400 19 5 17.600l5.600-5.600L5 6.400z"/>',
  film: '<path fill-rule="evenodd" d="M4 5h16v14H4zm2 2v2h2V7zm0 4v2h2v-2zm0 4v2h2v-2zm10-8v2h2V7zm0 4v2h2v-2zm0 4v2h2v-2zM10 7v10h4V7z"/>',
};

function icon(name, cls = "") {
  const t = document.createElement("template");
  t.innerHTML = `<svg viewBox="0 0 24 24" class="mb-icon ${cls}" aria-hidden="true" fill="currentColor" fill-rule="evenodd">${ICONS[name] || ""}</svg>`;
  return t.content.firstChild;
}

const mbLang = () => { try { return localStorage.getItem("mb.lang") || "en"; } catch { return "en"; } };

function mbImg(path, size, alt = "") {
  if (!path) return el("div", { class: "mb-noimg" }, icon("film"));
  return el("img", { class: "mb-img", loading: "lazy", alt, src: `/api/mb/img?p=${encodeURIComponent(path)}&s=${size}` });
}

const mins = (m) => (m ? (m >= 60 ? `${Math.floor(m / 60)}h ${m % 60}m` : `${m}m`) : "");
const pad2 = (n) => String(n).padStart(2, "0");

// ---------- shell ----------

function mbBuild() {
  if (MB.built) return;
  MB.built = true;
  $("mbSearchBtn").addEventListener("click", () => mbGo("search"));
  $("mbGear").addEventListener("click", () => mbSettings().catch((e) => toast(e.message)));
  for (const b of document.querySelectorAll(".mb-tabs button")) {
    b.addEventListener("click", () => mbTab(b.dataset.mb));
  }
  // Android's back button asks the page first.
  const prev = window.tgvaultBack;
  window.tgvaultBack = () => {
    const dlg = $("dlg");
    if (dlg && dlg.open) { dlg.close(); return true; }
    if (MB.stack.length > 1) { mbBack(); return true; }
    return prev ? prev() : false;
  };
  window.tgvaultRefresh = () => { mbRefreshCurrent(); mbPollDownloads(true); };
  window.mbOpen = (where) => { if (where === "downloads" || where === "library" || where === "home") mbTab(where); };
  window.addEventListener("scroll", () => document.querySelector(".mb-top").classList.toggle("solid", window.scrollY > 120), { passive: true });
}

// mbShow is called by app.js whenever the state arrives in the library app.
function mbShow() {
  mbBuild();
  document.body.classList.add("mb-on");
  $("main").hidden = true;
  $("mb").hidden = false;
  if (!MB.loaded) {
    MB.loaded = true;
    // Offline, open on what's downloaded: it never waits for Telegram.
    mbTab(navigator.onLine === false ? "downloads" : "home");
    window.addEventListener("online", () => { mbRefreshCurrent(); });
    window.addEventListener("offline", () => mbOnState());
  }
  mbOnState();
}

// Each tab keeps its own stack of screens and where each was scrolled to, so
// going to another tab and back lands exactly where you were.
function mbTab(name) {
  const top = MB.stack[MB.stack.length - 1];
  if (top) top.scroll = window.scrollY;
  MB.tabStacks = MB.tabStacks || {};
  if (MB.stack.length) MB.tabStacks[MB.tab] = MB.stack;
  const same = name === MB.tab;
  MB.tab = name;
  const kept = !same && MB.tabStacks[name];
  MB.stack = kept || [{ name, params: {} }];
  for (const b of document.querySelectorAll(".mb-tabs button")) b.classList.toggle("active", b.dataset.mb === name);
  mbRender(!!kept);
}

function mbGo(name, params = {}) {
  const cur = MB.stack[MB.stack.length - 1];
  if (cur) cur.scroll = window.scrollY;
  MB.stack.push({ name, params });
  mbRender();
}

function mbBack() {
  if (MB.stack.length > 1) MB.stack.pop();
  mbRender(true);
}

function mbRefreshCurrent() {
  if (!$("mb").hidden) mbRender(true);
}

// Grey placeholder shapes shown while a screen's data arrives, instead of a blank wait.
function skeleton(name) {
  const card = () => el("div", { class: "mb-sk-card" }, el("div", { class: "mb-sk mb-sk-poster" }), el("div", { class: "mb-sk mb-sk-line" }), el("div", { class: "mb-sk mb-sk-line short" }));
  if (name === "details") {
    return el("div", { class: "mb-skeleton" }, el("div", { class: "mb-sk mb-sk-hero" }), el("div", { class: "mb-sk mb-sk-line wide" }), el("div", { class: "mb-sk mb-sk-line" }), el("div", { class: "mb-sk mb-sk-line" }), el("div", { class: "mb-sk mb-sk-line short" }));
  }
  if (name === "home") {
    return el("div", { class: "mb-skeleton" }, el("div", { class: "mb-sk mb-sk-hero" }),
      el("div", { class: "mb-sk-row" }, [1, 2, 3, 4].map(card)), el("div", { class: "mb-sk-row" }, [1, 2, 3, 4].map(card)));
  }
  return el("div", { class: "mb-skeleton" }, el("div", { class: "mb-sk-grid" }, [1, 2, 3, 4, 5, 6].map(card)));
}

const screens = {};

async function mbRender(restore = false) {
  const view = MB.stack[MB.stack.length - 1];
  const host = $("mbBody");
  const seq = (MB.seq = (MB.seq || 0) + 1);
  $("mbBackBtn").hidden = MB.stack.length < 2;
  $("mbTitle").hidden = MB.stack.length >= 2 && view.name === "details";
  document.querySelector(".mb-top").classList.toggle("over", view.name === "details");
  // What was shown last time appears at once; fresh data replaces it when it arrives.
  if (view.node) {
    host.replaceChildren(view.node);
    if (view.scroll) window.scrollTo(0, view.scroll);
  } else {
    host.replaceChildren(skeleton(view.name));
    window.scrollTo(0, 0);
  }
  try {
    const node = await screens[view.name](view.params);
    if (seq !== MB.seq) return; // another screen was opened meanwhile
    const y = window.scrollY;
    view.node = node;
    host.replaceChildren(node);
    window.scrollTo(0, view.node && (restore || y) ? (restore ? (view.scroll || y) : y) : 0);
  } catch (e) {
    if (seq !== MB.seq) return;
    if (view.node) return; // keep showing what we had; the status chip says we are offline
    host.replaceChildren(el("div", { class: "mb-error" }, el("p", {}, e.message || String(e)),
      el("button", { type: "button", onclick: () => mbRender() }, "Try again")));
  }
}

// ---------- cards and rows ----------

function pill(text, kind) { return el("span", { class: `mb-pill ${kind || ""}` }, text); }

function itemCard(it, onOpen) {
  const open = onOpen || (() => mbGo("details", { kind: it.kind, id: it.id }));
  const mark = it.inLibrary ? pill("In library", "ok")
    : it.request === "queued" ? pill("Will send", "wait") : it.request === "requested" ? pill("Requested", "wait")
    : it.request === "declined" ? pill("Not available", "bad") : null;
  return el("button", { type: "button", class: "mb-card", onclick: open, "aria-label": it.title,
    onpointerdown: () => { if (it.id && !it.prefetched) { it.prefetched = true; api(`/api/mb/title?kind=${it.kind}&id=${it.id}`).catch(() => {}); } } },
    el("div", { class: "mb-poster" }, mbImg(it.poster, "w342", it.title),
      it.rating ? el("span", { class: "mb-rating" }, icon("star"), it.rating.toFixed(1)) : null,
      mark),
    el("div", { class: "mb-card-title" }, it.title),
    el("div", { class: "mb-card-sub" }, it.year || "", it.kind === "show" ? " · TV" : ""));
}

function row(title, items, more) {
  if (!items.length) return null;
  return el("section", { class: "mb-row" },
    el("div", { class: "mb-row-head" }, el("h2", {}, title), more ? el("button", { type: "button", class: "mb-link", onclick: more }, "See all") : null),
    el("div", { class: "mb-scroller" }, items.map((it) => itemCard(it))));
}

function grid(items) {
  return el("div", { class: "mb-grid" }, items.map((it) => itemCard(it)));
}

// ---------- Home ----------

screens.home = async () => {
  const info = await mbInfo();
  if (!info.hasTMDB) {
    MB.info = null;
    return el("div", { class: "mb-error" }, el("p", {}, "Getting set up… the app picks up its keys from the owner's PC (tgvault must be running there with the TMDB key entered)."),
      el("button", { type: "button", onclick: () => mbRender() }, "Check again"));
  }
  let h;
  try {
    h = await api("/api/mb/home");
  } catch (e) {
    // Offline: what's been downloaded still plays.
    return el("div", { class: "mb-error" }, el("p", {}, "Can't reach the movie database right now."),
      el("p", { class: "mb-hint" }, e.message),
      el("button", { type: "button", class: "mb-primary", onclick: () => mbTab("downloads") }, "Open my downloads"),
      el("button", { type: "button", onclick: () => mbRender() }, "Try again"));
  }
  const hero = (h.rows[0] ? h.rows[0].items : []).find((i) => i.backdrop);
  let added = null;
  try {
    const l = await api("/api/mb/library");
    const recent = [...l.titles].sort((a, b) => (b.newest || 0) - (a.newest || 0)).slice(0, 14);
    if (recent.length) {
      added = el("section", { class: "mb-row" },
        el("div", { class: "mb-row-head" }, el("h2", {}, "In the library"), el("button", { type: "button", class: "mb-link", onclick: () => mbTab("library") }, "See all")),
        el("div", { class: "mb-scroller" }, recent.map(libCard)));
    }
  } catch { /* offline or empty: the TMDB rows still show */ }
  const chips = el("div", { class: "mb-chips" },
    el("button", { type: "button", class: "mb-chip active" }, "All"),
    el("button", { type: "button", class: "mb-chip", onclick: () => mbGo("browse", { kind: "movie" }) }, "Movies"),
    el("button", { type: "button", class: "mb-chip", onclick: () => mbGo("browse", { kind: "show" }) }, "TV shows"));
  return el("div", { class: "mb-home" }, chips,
    hero ? heroCard(hero) : null, added,
    h.rows.map((r) => row(r.title, r.items, r.key === "trending" ? null : () => mbGo("browse", { kind: r.key.includes("shows") ? "show" : "movie" }))));
};

function heroCard(it) {
  const mark = it.inLibrary ? pill("In library", "ok") : it.request === "queued" ? pill("Will send", "wait") : it.request === "requested" ? pill("Requested", "wait") : null;
  return el("div", { class: "mb-hero" },
    el("img", { class: "mb-hero-img", alt: "", src: `/api/mb/img?p=${encodeURIComponent(it.backdrop)}&s=w780` }),
    el("div", { class: "mb-hero-shade" }),
    el("div", { class: "mb-hero-text" },
      el("div", { class: "mb-hero-kind" }, it.kind === "show" ? "TV show" : "Movie", it.year ? ` · ${it.year}` : ""),
      el("h1", {}, it.title), mark,
      el("button", { type: "button", class: "mb-primary", onclick: () => mbGo("details", { kind: it.kind, id: it.id }) }, "Details")));
}

// ---------- Browse and search ----------

screens.browse = async ({ kind, genre = "", page = 1 }) => {
  const h = await api("/api/mb/home");
  const genres = (h.genres && h.genres[kind]) || [];
  const wrap = el("div", { class: "mb-browse" });
  const select = el("select", { class: "mb-select", "aria-label": "Genre", onchange: (ev) => { const v = MB.stack[MB.stack.length - 1]; v.params = { kind, genre: ev.target.value, page: 1 }; mbRender(); } },
    el("option", { value: "" }, "All genres"), genres.map((g) => el("option", { value: g.id }, g.name)));
  select.value = genre;
  const tabs = el("div", { class: "mb-chips" },
    ["movie", "show"].map((k) => el("button", { type: "button", class: "mb-chip" + (k === kind ? " active" : ""), onclick: () => { MB.stack[MB.stack.length - 1].params = { kind: k, genre: "", page: 1 }; mbRender(); } }, k === "movie" ? "Movies" : "TV shows")), select);
  const list = el("div", { class: "mb-grid" });
  const more = el("button", { type: "button", class: "mb-more" }, "Show more");
  let p = page;
  async function load() {
    more.disabled = true;
    const r = await api(`/api/mb/browse?kind=${kind}&genre=${genre}&page=${p}`);
    list.append(...r.items.map((it) => itemCard(it)));
    more.hidden = p >= Math.min(r.pages || 1, 20) || !r.items.length;
    more.disabled = false;
    p++;
  }
  more.addEventListener("click", () => load().catch((e) => toast(e.message)));
  wrap.append(tabs, list, more);
  await load();
  return wrap;
};

screens.search = async ({ q = "" }) => {
  const out = el("div", { class: "mb-grid" });
  const status = el("p", { class: "mb-hint" }, "Search movies and TV shows.");
  let timer, token = 0;
  const input = el("input", { type: "search", class: "mb-search", placeholder: "Search titles", value: q, autocomplete: "off", enterkeyhint: "search" });
  async function run() {
    const my = ++token;
    const text = input.value.trim();
    MB.stack[MB.stack.length - 1].params = { q: input.value };
    if (text.length < 2) { out.replaceChildren(); status.textContent = "Search movies and TV shows."; return; }
    status.textContent = "Searching…";
    try {
      const r = await api("/api/mb/search?q=" + encodeURIComponent(text));
      if (my !== token) return;
      out.replaceChildren(...r.items.map((it) => itemCard(it)));
      status.textContent = r.items.length ? "" : `Nothing found for “${text}”.`;
    } catch (e) { if (my === token) status.textContent = e.message; }
  }
  input.addEventListener("input", () => { clearTimeout(timer); timer = setTimeout(run, 350); });
  const node = el("div", { class: "mb-searchpage" }, input, status, out);
  setTimeout(() => { input.focus(); if (input.value) run(); }, 50);
  return node;
};

// ---------- Details ----------

async function libraryFor(kind, title) {
  const l = await api("/api/mb/library");
  return l.titles.find((t) => t.kind === kind && t.norm === norm(title)) || null;
}

const norm = (s) => s.toLowerCase().replace(/['’]/g, "").replace(/[^a-z0-9]+/g, " ").trim();

function reqState(it) { return it.request || ""; }

screens.details = async ({ kind, id, offline }) => {
  let d = null;
  if (!offline && id) {
    try { d = await api(`/api/mb/title?kind=${kind}&id=${id}`); } catch (e) { if (!MB.stack[MB.stack.length - 1].params.lib) throw e; }
  }
  if (!d) {
    // Offline (or no TMDB match): the page is made from what's in the library.
    const lib = MB.stack[MB.stack.length - 1].params.lib;
    d = { item: { id: 0, kind: lib.kind, title: lib.title, year: lib.year, poster: lib.poster, overview: "" }, library: lib, seasons: [], cast: [], trailers: [], genres: [], offline: true };
    if (lib.kind === "show") d.seasons = lib.seasons.map((s) => ({ number: s.number, name: `Season ${s.number}`, episodes: s.episodes.length, have: s.episodes.length, local: s.episodes.filter((e) => e.local).length }));
  }
  const it = d.item;
  const lib = d.library;
  const isShow = it.kind === "show";
  const files = lib ? (isShow ? lib.seasons.flatMap((s) => s.episodes) : lib.files) : [];
  const local = files.filter((f) => f.local);

  const head = el("div", { class: "mb-d-head" },
    it.backdrop ? el("img", { class: "mb-d-back", alt: "", src: `/api/mb/img?p=${encodeURIComponent(it.backdrop)}&s=w780` }) : null,
    el("div", { class: "mb-d-shade" }),
    el("div", { class: "mb-d-top" },
      el("div", { class: "mb-d-poster" }, mbImg(it.poster, "w342", it.title)),
      el("div", { class: "mb-d-info" },
        el("h1", {}, it.title),
        el("div", { class: "mb-meta" },
          it.year ? el("span", {}, it.year) : null,
          d.runtime ? el("span", {}, mins(d.runtime)) : null,
          it.rating ? el("span", { class: "mb-rating inline" }, icon("star"), it.rating.toFixed(1)) : null,
          el("span", {}, isShow ? "TV show" : "Movie")),
        d.genres && d.genres.length ? el("div", { class: "mb-genres" }, d.genres.map((g) => el("span", {}, g))) : null)));

  const actions = el("div", { class: "mb-actions" });
  const reqBtn = () => {
    const st = reqState(it);
    if (isShow) return el("button", { type: "button", class: "mb-secondary", onclick: () => requestDialog(d) }, icon("clock"), (st === "requested" || st === "queued") ? "Request more" : "Request seasons");
    if (lib && files.length) return null;
    if (st === "queued") return el("button", { type: "button", class: "mb-secondary", disabled: true }, icon("clock"), "Will send when online");
    if (st === "requested") return el("button", { type: "button", class: "mb-secondary", disabled: true }, icon("check"), "Requested; waiting");
    return el("button", { type: "button", class: "mb-primary", onclick: () => requestDialog(d) }, icon("clock"), st === "declined" ? "Ask again" : "Request");
  };
  if (!isShow && lib && files.length) {
    const f = files[0];
    if (f.local) {
      actions.append(el("button", { type: "button", class: "mb-primary", onclick: () => playFiles([f.path], f.path, d) }, icon("play"), f.pos ? "Resume" : "Play"),
        el("button", { type: "button", class: "mb-secondary", onclick: () => removeFiles([f.path], it.title) }, icon("trash"), "Remove from phone"));
    } else if (dlFor(f.path)) {
      actions.append(dlBlock(dlFor(f.path), d));
    } else {
      actions.append(el("button", { type: "button", class: "mb-primary", onclick: () => downloadFiles([f.path], it.title) }, icon("down"), "Download", f.size ? ` · ${bytes(f.size)}` : ""));
    }
  }
  if (isShow && local.length) {
    const next = files.find((f) => f.local && f.pos > 0) || local[0];
    actions.append(el("button", { type: "button", class: "mb-primary", onclick: () => playFiles(local.map((f) => f.path), next.path, d) }, icon("play"), next.pos ? "Resume" : "Play", ` S${pad2(seasonOf(lib, next))}E${pad2(next.episodes[0])}`));
  }
  const rb = reqBtn();
  if (rb) actions.append(rb);
  if (d.trailers && d.trailers.length) actions.append(el("button", { type: "button", class: "mb-secondary", onclick: () => showTrailer(d.trailers[0], trailerBox) }, icon("play"), "Trailer"));
  const trailerBox = el("div", { class: "mb-trailer", hidden: true });

  const state = reqState(it) === "queued" ? el("p", { class: "mb-note" }, icon("clock"), "You're offline. The request is saved and goes out as soon as you're back online.")
    : reqState(it) === "requested" ? el("p", { class: "mb-note" }, icon("clock"), "You've asked for this; you'll see it here when it's ready.")
    : reqState(it) === "declined" ? el("p", { class: "mb-note bad" }, "It wasn't added last time. You can ask again.") : null;

  const body = [head, el("div", { class: "mb-d-body" }, actions, state, trailerBox,
    d.tagline ? el("p", { class: "mb-tagline" }, d.tagline) : null,
    it.overview ? overview(it.overview) : null,
    d.cast && d.cast.length ? el("section", {}, el("h2", {}, "Cast"),
      el("div", { class: "mb-scroller cast" }, d.cast.map((c) => el("div", { class: "mb-cast" },
        el("div", { class: "mb-cast-photo" }, mbImg(c.photo, "w185", c.name)), el("div", { class: "mb-cast-name" }, c.name), el("div", { class: "mb-cast-role" }, c.character || ""))))) : null,
    isShow ? seasonsSection(d) : null,
    d.offline ? el("p", { class: "mb-hint" }, "Showing what's in your library; the full details need a connection.") : null)];
  return el("div", { class: "mb-details" }, body);
};

const seasonOf = (lib, f) => (lib.seasons.find((s) => s.episodes.includes(f)) || {}).number || f.season || 1;

function overview(text) {
  const p = el("p", { class: "mb-overview clamp" }, text);
  const more = text.length > 220 ? el("button", { type: "button", class: "mb-link", onclick: (ev) => { p.classList.toggle("clamp"); ev.target.textContent = p.classList.contains("clamp") ? "More" : "Less"; } }, "More") : null;
  return el("div", {}, p, more);
}

async function showTrailer(t, box) {
  box.hidden = !box.hidden;
  if (box.hidden) { box.replaceChildren(); return; }
  box.replaceChildren(
    el("iframe", { src: `https://www.youtube-nocookie.com/embed/${encodeURIComponent(t.key)}?rel=0&playsinline=1`, title: t.name || "Trailer", allow: "fullscreen; picture-in-picture", allowfullscreen: true, referrerpolicy: "strict-origin-when-cross-origin" }),
    el("a", { class: "mb-link", href: `https://www.youtube.com/watch?v=${encodeURIComponent(t.key)}` }, "Not playing? Open it on YouTube"));
}

// ---------- seasons and episodes ----------

function seasonsSection(d) {
  const it = d.item;
  const wrap = el("section", { class: "mb-seasons" }, el("h2", {}, "Seasons"));
  for (const s of d.seasons) {
    const body = el("div", { class: "mb-eps", hidden: true });
    const head = el("button", { type: "button", class: "mb-season-head", "aria-expanded": "false" },
      el("div", {}, el("strong", {}, s.name || `Season ${s.number}`),
        el("div", { class: "mb-card-sub" }, `${s.episodes} episodes`, s.have ? ` · ${s.have} in library` : "", s.local ? ` · ${s.local} on this phone` : "")),
      s.local ? pill(`${s.local} on phone`, "ok") : s.have ? pill("Available", "info") : null);
    const key = `${it.kind}:${it.title}:${s.number}`;
    const toggle = async () => {
      body.hidden = !body.hidden;
      head.setAttribute("aria-expanded", String(!body.hidden));
      MB.open = MB.open || new Set();
      if (body.hidden) MB.open.delete(key); else MB.open.add(key);
      if (!body.hidden && !body.dataset.loaded) {
        body.dataset.loaded = "1";
        body.replaceChildren(el("div", { class: "mb-loading" }, "Loading…"));
        try { await fillSeason(body, d, s); } catch (e) { body.replaceChildren(el("p", { class: "mb-hint" }, e.message)); body.dataset.loaded = ""; }
      }
    };
    head.addEventListener("click", toggle);
    wrap.append(el("div", { class: "mb-season" }, head, body));
    if (MB.open && MB.open.has(key)) toggle().then(() => { if (body.hidden) MB.open.add(key); });
  }
  return wrap;
}

async function fillSeason(body, d, s) {
  const it = d.item;
  let eps;
  if (d.offline || !it.id) {
    const ls = d.library.seasons.find((x) => x.number === s.number);
    eps = ls.episodes.map((f) => ({ number: f.episodes[0], name: "", file: f }));
  } else {
    eps = (await api(`/api/mb/season?id=${it.id}&n=${s.number}&title=${encodeURIComponent(it.title)}&year=${it.year || 0}`)).episodes;
  }
  const all = eps.filter((e) => e.file);
  const toGet = all.filter((e) => !e.file.local);
  const onPhone = all.filter((e) => e.file.local);
  const bar = el("div", { class: "mb-season-actions" },
    toGet.length ? el("button", { type: "button", class: "mb-secondary", onclick: () => downloadFiles(toGet.map((e) => e.file.path), `${it.title} season ${s.number}`) }, icon("down"), `Download ${toGet.length === all.length ? "season" : toGet.length + " missing"}`, ` · ${bytes(toGet.reduce((n, e) => n + e.file.size, 0))}`) : null,
    onPhone.length ? el("button", { type: "button", class: "mb-secondary", onclick: () => removeFiles(onPhone.map((e) => e.file.path), `${it.title} season ${s.number}`) }, icon("trash"), "Remove from phone") : null);
  const playable = d.library ? d.library.seasons.flatMap((x) => x.episodes).filter((f) => f.local).map((f) => f.path) : [];
  const rows = eps.map((e) => episodeRow(d, e, playable));
  body.replaceChildren(bar, ...rows);
}

function episodeRow(d, e, playable) {
  const f = e.file;
  const pct = f && f.dur ? Math.min(100, Math.round((f.pos / f.dur) * 100)) : 0;
  let action;
  if (!f) action = el("span", { class: "mb-ep-none" }, "Not in library");
  else if (f.local) action = el("button", { type: "button", class: "mb-round", "aria-label": "Play", onclick: () => playFiles(playable, f.path, d) }, icon("play"));
  else if (dlFor(f.path)) action = el("span", { class: "mb-ep-dl", "data-dltext": f.path }, pctOf(dlFor(f.path)) + "%");
  else action = el("button", { type: "button", class: "mb-round ghost", "aria-label": "Download", onclick: () => downloadFiles([f.path], `${d.item.title} episode ${e.number}`) }, icon("down"));
  return el("div", { class: "mb-ep" + (f ? "" : " off") },
    el("div", { class: "mb-ep-n" }, e.number),
    el("div", { class: "mb-ep-main" },
      el("div", { class: "mb-ep-name" }, e.name || `Episode ${e.number}`),
      el("div", { class: "mb-card-sub" }, e.runtime ? mins(e.runtime) : "", f && !f.local ? ` · ${bytes(f.size)}` : "", f && f.local ? " · on this phone" : ""),
      f && dlFor(f.path) ? dlBlock(dlFor(f.path), d) : null,
      pct ? el("div", { class: "mb-progress" }, el("span", { style: `width:${pct}%` })) : null),
    action);
}

// ---------- requests ----------

async function requestDialog(d) {
  try {
    await requestDialogInner(d);
  } catch (e) {
    // never fail silently: the person tapped a button and expects something
    toast("Couldn't open the request window: " + (e && e.message ? e.message : e));
  }
}

async function requestDialogInner(d) {
  const it = d.item;
  if (it.kind === "movie") {
    const ok = await dialog({
      title: `Request ${it.title}?`,
      body: el("p", {}, "The owner gets a notice, adds it to the library, and it will show up under Downloads when it's ready."),
      buttons: [{ label: "Not now", value: null }, { label: "Request", value: "go", primary: true }],
      run: async () => { MB.lastReq = await api("/api/mb/request", { kind: "movie", tmdb: it.id, title: it.title, year: it.year || 0, poster: it.poster || "" }); },
    });
    if (ok) { toast(requestedToast()); mbRender(true); }
    return;
  }
  const seasonList = d.seasons || [];
  const rows = seasonList.map((s) => {
    const complete = s.episodes > 0 && s.have >= s.episodes;
    const box = el("input", { type: "checkbox", id: `rq${s.number}`, disabled: complete });
    const epBox = el("div", { class: "mb-rq-eps", hidden: true });
    const more = el("button", { type: "button", class: "mb-link", onclick: async () => {
      epBox.hidden = !epBox.hidden;
      if (!epBox.hidden && !epBox.dataset.loaded) {
        epBox.dataset.loaded = "1";
        try {
          const r = await api(`/api/mb/season?id=${it.id}&n=${s.number}&title=${encodeURIComponent(it.title)}&year=${it.year || 0}`);
          epBox.replaceChildren(...r.episodes.map((e) => {
            const c = el("input", { type: "checkbox", disabled: !!e.file, "data-ep": e.number });
            c.addEventListener("change", () => { box.checked = false; });
            return el("label", { class: "mb-rq-ep" }, c, ` ${e.number}. ${e.name}`, e.file ? " (in library)" : "");
          }));
        } catch (er) { epBox.textContent = er.message; epBox.dataset.loaded = ""; }
      }
    } }, "Pick episodes ▾");
    return el("div", { class: "mb-rq" },
      el("label", { class: "mb-rq-season" }, box, ` ${s.name || "Season " + s.number}`, el("span", { class: "mb-card-sub" }, ` ${s.episodes} episodes`, s.have ? `, ${s.have} already here` : "")), complete ? null : more, epBox);
  });
  const all = el("input", { type: "checkbox", id: "rqAll" });
  all.addEventListener("change", () => {
    for (const s of seasonList) {
      const b = document.getElementById(`rq${s.number}`);
      if (b && !b.disabled) b.checked = all.checked;
    }
  });
  const allRow = seasonList.length > 1 ? el("label", { class: "mb-rq-season mb-rq-all" }, all, " Select all seasons") : null;
  const ok = await dialog({
    title: `Request ${it.title}`,
    body: el("div", { class: "mb-rq-list" }, el("p", { class: "mb-hint" }, seasonList.length ? "Choose whole seasons, or pick single episodes." : "No season list is available right now. Check your connection and try again."), allRow, rows),
    buttons: [{ label: "Cancel", value: null }, { label: "Request", value: "go", primary: true }],
    run: async () => {
      const seasons = [], episodes = {};
      for (const s of seasonList) {
        const box = document.getElementById(`rq${s.number}`);
        if (box && box.checked) { seasons.push(s.number); continue; }
        const picked = [...document.querySelectorAll(`#dlgBody .mb-rq`)].find((x) => x.querySelector(`#rq${s.number}`));
        const eps = picked ? [...picked.querySelectorAll("input[data-ep]:checked")].map((c) => Number(c.dataset.ep)) : [];
        if (eps.length) episodes[s.number] = eps;
      }
      if (!seasons.length && !Object.keys(episodes).length) throw new Error("Choose at least one season or episode.");
      const payload = { kind: "show", tmdb: it.id, title: it.title, year: it.year || 0, poster: it.poster || "", seasons };
      if (Object.keys(episodes).length) payload.episodes = episodes;
      MB.lastReq = await api("/api/mb/request", payload);
    },
  });
  if (ok) { toast(requestedToast()); mbRender(true); }
}

const requestedToast = () => (MB.lastReq && MB.lastReq.request && MB.lastReq.request.state === "queued")
  ? "You're offline: saved, and it will be sent when you're back online."
  : "Requested. You'll see it under Downloads.";

// ---------- downloading, removing, playing ----------

async function downloadFiles(paths, label) {
  try {
    const r = await api("/api/mb/download", { paths });
    toast(`Downloading ${label}… ${r.queued > 1 ? r.queued + " files" : ""}`.trim());
    location.href = "tgvault://dlservice"; // keeps the download going with the app closed
    await mbPollDownloads(true);
    mbRefreshCurrent();
  } catch (e) { toast(e.message); }
}

const dlControl = (action, path) => api("/api/mb/download-action", { action, path }).then(() => mbPollDownloads(true)).then(() => mbRefreshCurrent()).catch((e) => toast(e.message));

// the queue entry for a file, if it has one that isn't finished
const dlFor = (path) => (MB.dl && MB.dl.items.find((i) => i.path === path && i.state !== "done")) || null;

const fmtEta = (sec) => !sec ? "" : sec < 60 ? `${sec}s left` : sec < 3600 ? `${Math.round(sec / 60)} min left` : `${Math.floor(sec / 3600)}h ${Math.round((sec % 3600) / 60)}m left`;
const pctOf = (it) => (it.size ? Math.min(100, Math.floor((it.done / it.size) * 100)) : 0);

function dlDetail(it) {
  if (it.state === "failed") return it.error || "Failed";
  if (it.state === "paused") return `Paused · ${pctOf(it)}%`;
  if (it.state === "queued") return it.waiting || (it.error ? `Retrying… (${it.error})` : "Waiting in line");
  return [`${pctOf(it)}%`, it.speed > 1 ? `${bytes(it.speed)}/s` : "", fmtEta(it.eta), `${bytes(it.done)} of ${bytes(it.size)}`].filter(Boolean).join(" · ");
}

// Can what has arrived of this download be watched now?
const playable = (it) => !!it.canPlay && (it.state === "running" || it.state === "paused" || it.state === "queued");

function readyText(it) {
  if (!playable(it)) return "";
  if (it.readyIn === 0) return "Ready to watch";
  if (it.readyIn > 0) {
    const m = Math.max(1, Math.round(it.readyIn / 60));
    return `Watching now will pause when it catches up with the download. Ready to watch without stopping in about ${m >= 90 ? Math.round(m / 60) + " h" : m + " min"}.`;
  }
  return "You can start watching now. It may pause while the rest downloads.";
}

// A progress block for one queued download: bar, numbers and the buttons that fit its state.
// d is the title the download belongs to, when the page knows it (for subtitles and the player's title).
function dlBlock(it, d) {
  const bar = el("div", { class: "mb-progress thick" + (it.state === "failed" ? " bad" : "") }, el("span", { style: `width:${pctOf(it)}%`, "data-dlbar": it.path }));
  const btns = el("div", { class: "mb-dl-btns" },
    (it.state === "running" || it.state === "queued") ? el("button", { type: "button", class: "mb-link", onclick: () => dlControl("pause", it.path) }, "Pause") : null,
    it.state === "paused" ? el("button", { type: "button", class: "mb-link", onclick: () => dlControl("resume", it.path) }, "Resume") : null,
    it.state === "failed" ? el("button", { type: "button", class: "mb-link", onclick: () => dlControl("retry", it.path) }, "Retry") : null,
    el("button", { type: "button", class: "mb-link bad", onclick: () => dlControl("cancel", it.path) }, "Cancel"));
  const ctx = d || { item: { id: 0, kind: it.path.includes("/Shows/") ? "show" : "movie", title: it.title } };
  const play = el("div", { class: "mb-dl-play", "data-dlplay": it.path, hidden: !playable(it) },
    el("button", { type: "button", class: "mb-secondary", "data-dlplaybtn": it.path, onclick: () => playFiles([it.path], it.path, ctx) }, icon("play"), it.readyIn === 0 ? "Play now" : "Play while downloading"),
    el("p", { class: "mb-hint", "data-dlready": it.path }, readyText(it)));
  return el("div", { class: "mb-dl" }, el("div", { class: "mb-dl-text", "data-dltext": it.path }, dlDetail(it)), bar, play, btns);
}

async function removeFiles(paths, label) {
  const ok = await dialog({
    title: "Remove from this phone?",
    body: el("p", {}, `${label} will be removed from this phone to free space. It stays in the library; you can download it again.`),
    buttons: [{ label: "Keep it", value: null }, { label: "Remove", value: "rm", danger: true }],
    run: async () => { await api("/api/mb/remove", { paths }); },
  });
  if (ok) { toast("Removed."); mbRender(true); }
}

async function playFiles(paths, start, d) {
  try {
    const r = await api("/api/mb/playlist", { paths, start, tmdb: d.item.id || 0, show: d.item.kind === "show", title: d.item.title, lang: mbLang() });
    window.__mbLastPlay = r; // the player reads the rest from this link
    location.href = "tgvault://mbplay?u=" + encodeURIComponent(r.url);
  } catch (e) { toast(e.message); }
}

// ---------- Downloads ----------

const STATE_TEXT = { requested: "Waiting", fulfilled: "Added", declined: "Not available" };

screens.downloads = async () => {
  const [lib, rq] = await Promise.all([api("/api/mb/library"), api("/api/mb/requests").catch(() => ({ requests: [] }))]);
  const titles = lib.titles;
  const fileOf = (t) => (t.kind === "movie" ? t.files : t.seasons.flatMap((s) => s.episodes));
  // continue watching: started and not finished
  const started = [];
  for (const t of titles) for (const f of fileOf(t)) if (f.local && f.pos > 0) started.push({ t, f });
  started.sort((a, b) => (b.f.added || 0) - (a.f.added || 0));
  const cont = started.length ? el("section", { class: "mb-row" }, el("div", { class: "mb-row-head" }, el("h2", {}, "Continue watching")),
    el("div", { class: "mb-scroller" }, started.slice(0, 10).map(({ t, f }) => {
      const pct = f.dur ? Math.min(100, Math.round((f.pos / f.dur) * 100)) : 0;
      const label = t.kind === "show" ? `S${pad2(seasonFromPath(f.path))}E${pad2(f.episodes[0])}` : "";
      return el("button", { type: "button", class: "mb-card wide", onclick: () => playFiles(fileOf(t).filter((x) => x.local).map((x) => x.path), f.path, { item: { id: t.tmdb || 0, kind: t.kind, title: t.title } }) },
        el("div", { class: "mb-poster" }, mbImg(t.poster, "w342", t.title), el("div", { class: "mb-play-over" }, icon("play")), el("div", { class: "mb-progress" }, el("span", { style: `width:${pct}%` }))),
        el("div", { class: "mb-card-title" }, t.title), el("div", { class: "mb-card-sub" }, label));
    }))) : null;

  // requests
  const mine = rq.requests.filter((r) => r.title).slice(0, 12);
  const answered = rq.requests.some((r) => r.state !== "requested" && r.state !== "queued");
  const reqs = mine.length ? el("section", { class: "mb-reqs" }, el("div", { class: "mb-row-head" }, el("h2", {}, "Requests"),
    answered ? el("button", { type: "button", class: "mb-link mb-clear", onclick: () => api("/api/mb/requests/clear", {}).then(() => mbRender(true)).catch((e) => toast(e.message)) }, "Clear answered") : null),
    mine.map((r) => {
      const status = r.state === "declined" ? pill("Not available", "bad")
        : r.available ? pill("Ready to download", "ok")
        : r.state === "fulfilled" ? pill("Being added…", "info") : r.state === "queued" ? pill("Waiting to send (offline)", "wait") : pill("Requested", "wait");
      const what = r.kind === "movie" ? "Movie" : !r.seasons || !r.seasons.length ? (r.episodes ? "Chosen episodes" : "All seasons") : r.seasons.length === 1 ? `Season ${r.seasons[0]}` : `Seasons ${r.seasons.join(", ")}`;
      const lt = titles.find((t) => t.kind === (r.kind === "movie" ? "movie" : "show") && t.norm === norm(r.title) && (!t.year || !r.year || Math.abs(t.year - r.year) <= 1));
      return el("button", { type: "button", class: "mb-req", onclick: () => mbGo("details", { kind: r.kind === "movie" ? "movie" : "show", id: r.tmdb, lib: lt || undefined }) },
        el("div", { class: "mb-req-poster" }, mbImg(r.poster, "w154", r.title)),
        el("div", { class: "mb-req-main" }, el("strong", {}, r.title, r.year ? ` (${r.year})` : ""), el("div", { class: "mb-card-sub" }, what, " · asked ", when(r.at)), status));
    })) : null;

  const onPhone = titles.filter((t) => t.local > 0);
  const movies = onPhone.filter((t) => t.kind === "movie");
  const shows = onPhone.filter((t) => t.kind === "show");
  const empty = !onPhone.length && !started.length && !mine.length ? el("div", { class: "mb-empty" }, icon("film"), el("p", {}, "Nothing on this phone yet."),
    el("p", { class: "mb-hint" }, "Everything the owner has added is in the Library tab. Download something there and it shows up here, ready to watch offline."),
    el("button", { type: "button", class: "mb-primary", onclick: () => mbTab("library") }, "Open the library")) : null;
  const queue = (MB.dl ? MB.dl.items : []).filter((i) => i.state !== "done");
  const queueSection = queue.length ? el("section", { class: "mb-queue" },
    el("div", { class: "mb-row-head" }, el("h2", {}, "Downloading"), connChip(),
      queue.some((i) => i.state === "failed") ? el("button", { type: "button", class: "mb-link", onclick: () => dlControl("clear", "") }, "Clear failed") : null),
    queue.map((it) => el("div", { class: "mb-queue-item" }, el("strong", {}, it.title), dlBlock(it)))) : null;
  return el("div", { class: "mb-downloads" }, queueSection, cont, reqs,
    movies.length ? el("section", {}, el("h2", {}, "Movies on this phone"), el("div", { class: "mb-grid" }, movies.map(libCard))) : null,
    shows.length ? el("section", {}, el("h2", {}, "TV shows on this phone"), el("div", { class: "mb-grid" }, shows.map(libCard))) : null,
    onPhone.length ? el("p", { class: "mb-hint mb-more-hint" }, "Looking for something else? ", el("button", { type: "button", class: "mb-link", onclick: () => mbTab("library") }, "Open the library")) : null,
    empty);
};

// A library title as a card: what's on the phone, or that it can be downloaded.
function libCard(t) {
  const mark = t.local === t.total ? pill("On phone", "ok") : t.local ? pill(`${t.local}/${t.total} on phone`, "ok") : pill("Download", "info");
  return el("button", { type: "button", class: "mb-card", onclick: () => mbGo("details", { kind: t.kind, id: t.tmdb || 0, lib: t }) },
    el("div", { class: "mb-poster" }, mbImg(t.poster, "w342", t.title), mark),
    el("div", { class: "mb-card-title" }, t.title),
    el("div", { class: "mb-card-sub" }, [t.year || "", t.kind === "show" ? `${t.seasons.length} season${t.seasons.length === 1 ? "" : "s"}` : bytes(t.size)].filter(Boolean).join(" · ")));
}

// ---------- Library: everything the owner has filed, requested or not ----------

screens.library = async ({ q = "", kind = "all" }) => {
  const lib = await api("/api/mb/library");
  const titles = lib.titles;
  // posters still being looked up: ask again shortly, a few times
  if (lib.pending && (MB.libTries = (MB.libTries || 0) + 1) <= 10) setTimeout(() => { if (MB.tab === "library") mbRefreshCurrent(); }, 2500);
  if (!lib.pending) MB.libTries = 0;
  const out = el("div", { class: "mb-libgrid" });
  const status = el("p", { class: "mb-hint" });
  const input = el("input", { type: "search", class: "mb-search", placeholder: `Search the library (${titles.length})`, value: q, autocomplete: "off" });
  const chips = el("div", { class: "mb-chips" });
  let current = kind;
  function draw() {
    const text = norm(input.value);
    const view = MB.stack[MB.stack.length - 1];
    view.params = { q: input.value, kind: current };
    for (const c of chips.children) c.classList.toggle("active", c.dataset.k === current);
    const list = titles.filter((t) => (current === "all" || t.kind === current) && (!text || norm(t.title).includes(text)));
    const movies = list.filter((t) => t.kind === "movie"), shows = list.filter((t) => t.kind === "show");
    out.replaceChildren(
      movies.length ? el("section", {}, el("h2", {}, `Movies (${movies.length})`), el("div", { class: "mb-grid" }, movies.map(libCard))) : null,
      shows.length ? el("section", {}, el("h2", {}, `TV shows (${shows.length})`), el("div", { class: "mb-grid" }, shows.map(libCard))) : null);
    status.textContent = list.length ? "" : titles.length ? "Nothing matches." : "";
  }
  for (const [k, label] of [["all", "All"], ["movie", "Movies"], ["show", "TV shows"]]) {
    chips.append(el("button", { type: "button", "data-k": k, class: "mb-chip", onclick: () => { current = k; draw(); } }, label));
  }
  input.addEventListener("input", draw);
  const wrap = el("div", { class: "mb-librarypage" }, el("div", { class: "mb-searchpage" }, input), chips, status, out);
  if (!titles.length) {
    return el("div", { class: "mb-empty" }, icon("film"), el("p", {}, "The library is empty."),
      el("p", { class: "mb-hint" }, "When the owner adds movies and shows, they appear here. You can also ask for something from Home."),
      el("button", { type: "button", class: "mb-primary", onclick: () => mbTab("home") }, "Browse Home"));
  }
  draw();
  return wrap;
};

const seasonFromPath = (p) => { const m = /Season (\d+)/.exec(p); return m ? Number(m[1]) : 1; };

// ---------- settings ----------

async function mbSettings() {
  const info = await mbInfo(true);
  const lang = el("select", { class: "mb-select" }, [["en", "English"], ["sw", "Swahili"], ["fr", "French"], ["es", "Spanish"], ["ar", "Arabic"], ["pt", "Portuguese"], ["de", "German"], ["it", "Italian"]].map(([v, n]) => el("option", { value: v }, n)));
  lang.value = mbLang();
  lang.addEventListener("change", () => { try { localStorage.setItem("mb.lang", lang.value); } catch { /* private mode */ } });
  const key = el("input", { type: "password", autocomplete: "off", placeholder: info.hasSubs ? "Key saved; type to replace" : "SubDL API key", style: "width:100%" });
  await dialog({
    title: "Settings",
    body: el("div", { class: "mb-settings" },
      el("label", {}, "Subtitle language", lang),
      el("label", {}, "Subtitle search key", el("span", { class: "mb-hint" }, "Free at subdl.com (Account, API). Needed to search for subtitles while watching."), key),
      el("div", { class: "mb-storage" },
        el("span", {}, info.used ? `Downloads on this phone: ${bytes(info.used)}` : "No downloads on this phone."),
        el("button", { type: "button", class: "mb-link", onclick: () => { $("dlg").close(); setTimeout(mbStorage, 50); } }, "Storage and space")),
      el("div", { class: "mb-set-links" },
        el("a", { href: "tgvault://check-update" }, "Check for an update"),
        el("a", { href: "tgvault://share-log" }, "Share the log")),
      el("p", { class: "mb-hint" }, "This product uses the TMDB API but is not endorsed or certified by TMDB.")),
    buttons: [{ label: "Close", value: null }, { label: "Save", value: "save", primary: true }],
    run: async () => { if (key.value.trim()) { await api("/api/mb/subs/key", { key: key.value }); MB.info = null; } },
  });
}

// ---------- storage: where downloads are kept, and freeing space ----------

async function mbStorage() {
  let st, lib;
  try {
    [st, lib] = await Promise.all([api("/api/mb/storage"), api("/api/mb/library")]);
  } catch (e) {
    toast("Couldn't read the storage: " + e.message);
    return;
  }
  // every downloaded video, with what is needed to judge what to remove
  const files = [];
  for (const t of lib.titles || []) {
    const add = (f) => {
      if (!f.local || !/\.(mkv|mp4|avi|mov|webm|m4v|ts)$/i.test(f.name || "")) return;
      const ep = f.season && f.episodes && f.episodes.length ? ` S${pad2(f.season)}E${pad2(f.episodes[0])}` : "";
      files.push({ path: f.path, label: t.title + ep, size: f.size || 0, watched: f.dur > 0 && !f.pos });
    };
    (t.files || []).forEach(add);
    (t.seasons || []).forEach((sn) => sn.episodes.forEach(add));
  }
  files.sort((a, b) => b.size - a.size);

  const where = (st.stores || []).map((x) => {
    const r = el("input", { type: "radio", name: "mbWhere", value: x.path, checked: x.path === st.current });
    r.addEventListener("change", async () => {
      try {
        await api("/api/mb/storage", { path: x.path });
        toast(`New downloads will go to: ${x.label}`);
        MB.info = null;
      } catch (e) { toast(e.message); }
    });
    return el("label", { class: "mb-rq-season mb-where" }, r, el("span", {}, ` ${x.label}`, el("span", { class: "mb-card-sub" }, ` ${bytes(x.free)} free`)));
  });

  const total = el("span", { class: "mb-hint" });
  const boxes = files.map((f) => {
    const c = el("input", { type: "checkbox", "data-path": f.path });
    c.addEventListener("change", sum);
    return { f, c };
  });
  function picked() { return boxes.filter((b) => b.c.checked); }
  function sum() {
    const n = picked();
    total.textContent = n.length ? `${n.length} selected, ${bytes(n.reduce((a, b) => a + b.f.size, 0))} to free` : "";
  }
  const list = files.length
    ? el("div", { class: "mb-space-list" }, boxes.map(({ f, c }) => el("label", { class: "mb-rq-ep mb-space-row" }, c, ` ${f.label}`, el("span", { class: "mb-card-sub" }, ` ${bytes(f.size)}`, f.watched ? " · watched" : ""))))
    : el("p", { class: "mb-hint" }, "Nothing is downloaded to this phone right now.");
  const watchedBtn = el("button", { type: "button", class: "mb-secondary", onclick: () => { boxes.forEach((b) => { b.c.checked = b.f.watched; }); sum(); } }, "Select watched");
  const allBtn = el("button", { type: "button", class: "mb-secondary", onclick: () => { const on = !boxes.every((b) => b.c.checked); boxes.forEach((b) => { b.c.checked = on; }); sum(); } }, "Select all");

  await dialog({
    title: "Storage and space",
    body: el("div", { class: "mb-settings" },
      el("p", { class: "mb-hint" }, `Downloads use ${bytes(st.used)}. Your phone has ${bytes((st.stores[0] || {}).free || 0)} free.`),
      el("h3", {}, "Keep new downloads on"),
      where,
      el("p", { class: "mb-hint" }, "Things already downloaded stay where they are and keep working. An SD card that is taken out puts new downloads back on the phone."),
      el("h3", {}, "Free up space"),
      files.length ? el("div", { class: "mb-actions" }, watchedBtn, allBtn) : null,
      list, total),
    buttons: [{ label: "Close", value: null }, ...(files.length ? [{ label: "Delete selected", value: "del", danger: true }] : [])],
    run: async () => {
      const n = picked();
      if (!n.length) throw new Error("Tick what to delete first.");
      await api("/api/mb/remove", { paths: n.map((b) => b.f.path) });
      toast(`Freed ${bytes(n.reduce((a, b) => a + b.f.size, 0))}`);
      MB.info = null;
      mbRender(true);
    },
  });
}

async function mbInfo(fresh) {
  if (!MB.info || fresh) MB.info = await api("/api/mb/info");
  return MB.info;
}

// ---------- download progress ----------

// Asks the Go side for the queue and updates what's on screen: the bar at the
// top, the bars and numbers on the page, and, when something finishes or
// fails, a message and a fresh page.
async function mbPollDownloads(force) {
  if (MB.polling && !force) return;
  MB.polling = true;
  try {
    const r = await api("/api/mb/downloads");
    const before = new Map((MB.dl ? MB.dl.items : []).map((i) => [i.path, i.state]));
    MB.dl = r;
    let changed = false;
    for (const it of r.items) {
      const was = before.get(it.path);
      if (was && was !== it.state) {
        changed = true;
        if (it.state === "done") toast(`Downloaded: ${it.title}`);
        if (it.state === "failed") toast(`Download failed: ${it.title}. ${it.error || ""}`.trim(), { label: "Retry", fn: () => dlControl("retry", it.path) });
      }
      if (!was && it.state !== "done") changed = true;
    }
    for (const [path] of before) if (!r.items.find((i) => i.path === path)) changed = true;
    mbPaintBanner();
    mbPaintProgress();
    if (changed && !$("dlg").open) mbRefreshCurrent();
  } catch { /* the page is closing, or the app is starting */ }
  MB.polling = false;
  clearTimeout(MB.pollTimer);
  MB.pollTimer = setTimeout(() => mbPollDownloads(), MB.dl && MB.dl.active ? 1200 : 6000);
}

// How the connection is treating the downloads, in words and signal bars, so a
// slow download reads as a slow connection and not a stuck app.
const QUALITY = { good: ["Good connection", 4], fair: ["Fair connection", 3], poor: ["Weak connection: slow", 1], retrying: ["Connection problem: retrying", 0], offline: ["No connection", 0], idle: ["", 0] };
function connChip() {
  const q = (MB.dl && MB.dl.quality) || "idle";
  const [label, bars] = QUALITY[q] || QUALITY.idle;
  if (!label) return null;
  const sig = el("span", { class: "mb-bars q-" + q }, [1, 2, 3, 4].map((n) => el("i", { class: n <= bars ? "on" : "" })));
  return el("span", { class: "mb-conn q-" + q }, sig, " ", label);
}

function mbPaintBanner() {
  const banner = $("mbBanner");
  const live = (MB.dl ? MB.dl.items : []).filter((i) => i.state === "running" || i.state === "queued" || i.state === "paused");
  if (!live.length) { banner.hidden = true; return; }
  const running = live.filter((i) => i.state === "running");
  const lead = running[0] || live[0];
  const done = live.reduce((n, i) => n + i.done, 0), size = live.reduce((n, i) => n + i.size, 0);
  const speed = running.reduce((n, i) => n + (i.speed || 0), 0);
  const pct = size ? Math.floor((done / size) * 100) : 0;
  const allPaused = live.every((i) => i.state === "paused");
  const eta = speed > 1 ? fmtEta(Math.round((size - done) / speed)) : "";
  banner.hidden = false;
  banner.replaceChildren(
    el("button", { type: "button", class: "mb-banner-main", onclick: () => mbTab("downloads") },
      el("strong", {}, allPaused ? "Paused" : `Downloading${live.length > 1 ? ` ${live.length} files` : ""}`), " ",
      lead.title, ` · ${pct}%`, speed > 1 ? ` · ${bytes(speed)}/s` : "", eta ? ` · ${eta}` : "", " ", connChip()),
    el("button", { type: "button", class: "mb-banner-btn", onclick: () => dlControl(allPaused ? "resume" : "pause", "") }, allPaused ? "Resume" : "Pause"));
}

// Moves the bars and numbers already on the page without rebuilding it.
function mbPaintProgress() {
  for (const it of (MB.dl ? MB.dl.items : [])) {
    for (const bar of document.querySelectorAll(`[data-dlbar="${CSS.escape(it.path)}"]`)) bar.style.width = pctOf(it) + "%";
    for (const t of document.querySelectorAll(`[data-dltext="${CSS.escape(it.path)}"]`)) t.textContent = t.classList.contains("mb-ep-dl") ? pctOf(it) + "%" : dlDetail(it);
    for (const box of document.querySelectorAll(`[data-dlplay="${CSS.escape(it.path)}"]`)) {
      box.hidden = !playable(it);
      const hint = box.querySelector("[data-dlready]");
      if (hint) hint.textContent = readyText(it);
      const btn = box.querySelector("[data-dlplaybtn]");
      if (btn && btn.lastChild) btn.lastChild.textContent = it.readyIn === 0 ? "Play now" : "Play while downloading";
    }
  }
}

function mbOnState() {
  if (!state || !state.engine) return;
  const chip = $("mbStatus");
  const tgst = (state.telegram || {}).status;
  const offline = navigator.onLine === false || tgst !== "ready" || state.telegram.online === false;
  chip.hidden = !offline;
  chip.textContent = navigator.onLine === false ? "Offline" : "Reconnecting…";
  if (!MB.pollStarted) { MB.pollStarted = true; mbPollDownloads(); }
}
