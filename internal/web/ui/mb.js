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
  window.tgvaultRefresh = () => { mbRefreshCurrent(); };
}

// mbShow is called by app.js whenever the state arrives in the library app.
function mbShow() {
  mbBuild();
  document.body.classList.add("mb-on");
  $("main").hidden = true;
  $("mb").hidden = false;
  if (!MB.loaded) {
    MB.loaded = true;
    mbTab("home");
  }
  mbOnState();
}

function mbTab(name) {
  MB.tab = name;
  MB.stack = [{ name, params: {} }];
  for (const b of document.querySelectorAll(".mb-tabs button")) b.classList.toggle("active", b.dataset.mb === name);
  mbRender();
}

function mbGo(name, params = {}) {
  const cur = MB.stack[MB.stack.length - 1];
  if (cur) cur.scroll = $("mbBody").scrollTop || window.scrollY;
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

const screens = {};

async function mbRender(restore = false) {
  const view = MB.stack[MB.stack.length - 1];
  const host = $("mbBody");
  const seq = (MB.seq = (MB.seq || 0) + 1);
  $("mbBackBtn").hidden = MB.stack.length < 2;
  $("mbTitle").hidden = MB.stack.length >= 2 && view.name === "details";
  document.querySelector(".mb-top").classList.toggle("over", view.name === "details");
  if (!restore) host.replaceChildren(el("div", { class: "mb-loading" }, "Loading…"));
  try {
    const node = await screens[view.name](view.params);
    if (seq !== MB.seq) return; // another screen was opened meanwhile
    host.replaceChildren(node);
    if (restore && view.scroll) window.scrollTo(0, view.scroll); else if (!restore) window.scrollTo(0, 0);
  } catch (e) {
    if (seq !== MB.seq) return;
    host.replaceChildren(el("div", { class: "mb-error" }, el("p", {}, e.message || String(e)),
      el("button", { type: "button", onclick: () => mbRender() }, "Try again")));
  }
}

// ---------- cards and rows ----------

function pill(text, kind) { return el("span", { class: `mb-pill ${kind || ""}` }, text); }

function itemCard(it, onOpen) {
  const open = onOpen || (() => mbGo("details", { kind: it.kind, id: it.id }));
  const mark = it.inLibrary ? pill("In library", "ok")
    : it.request === "requested" ? pill("Requested", "wait")
    : it.request === "declined" ? pill("Not available", "bad") : null;
  return el("button", { type: "button", class: "mb-card", onclick: open, "aria-label": it.title },
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
  const h = await api("/api/mb/home");
  const hero = (h.rows[0] ? h.rows[0].items : []).find((i) => i.backdrop);
  const chips = el("div", { class: "mb-chips" },
    el("button", { type: "button", class: "mb-chip active" }, "All"),
    el("button", { type: "button", class: "mb-chip", onclick: () => mbGo("browse", { kind: "movie" }) }, "Movies"),
    el("button", { type: "button", class: "mb-chip", onclick: () => mbGo("browse", { kind: "show" }) }, "TV shows"));
  return el("div", { class: "mb-home" }, chips,
    hero ? heroCard(hero) : null,
    h.rows.map((r) => row(r.title, r.items, r.key === "trending" ? null : () => mbGo("browse", { kind: r.key.includes("shows") ? "show" : "movie" }))));
};

function heroCard(it) {
  const mark = it.inLibrary ? pill("In library", "ok") : it.request === "requested" ? pill("Requested", "wait") : null;
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
    if (isShow) return el("button", { type: "button", class: "mb-secondary", onclick: () => requestDialog(d) }, icon("clock"), st === "requested" ? "Request more" : "Request seasons");
    if (lib && files.length) return null;
    if (st === "requested") return el("button", { type: "button", class: "mb-secondary", disabled: true }, icon("check"), "Requested; waiting");
    return el("button", { type: "button", class: "mb-primary", onclick: () => requestDialog(d) }, icon("clock"), st === "declined" ? "Ask again" : "Request");
  };
  if (!isShow && lib && files.length) {
    const f = files[0];
    if (f.local) {
      actions.append(el("button", { type: "button", class: "mb-primary", onclick: () => playFiles([f.path], f.path, d) }, icon("play"), f.pos ? "Resume" : "Play"),
        el("button", { type: "button", class: "mb-secondary", onclick: () => removeFiles([f.path], it.title) }, icon("trash"), "Remove from phone"));
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

  const state = reqState(it) === "requested" ? el("p", { class: "mb-note" }, icon("clock"), "You've asked for this; you'll see it here when it's ready.")
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
    head.addEventListener("click", async () => {
      body.hidden = !body.hidden;
      head.setAttribute("aria-expanded", String(!body.hidden));
      if (!body.hidden && !body.dataset.loaded) {
        body.dataset.loaded = "1";
        body.replaceChildren(el("div", { class: "mb-loading" }, "Loading…"));
        try { await fillSeason(body, d, s); } catch (e) { body.replaceChildren(el("p", { class: "mb-hint" }, e.message)); body.dataset.loaded = ""; }
      }
    });
    wrap.append(el("div", { class: "mb-season" }, head, body));
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
    eps = (await api(`/api/mb/season?id=${it.id}&n=${s.number}&title=${encodeURIComponent(it.title)}`)).episodes;
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
  else action = el("button", { type: "button", class: "mb-round ghost", "aria-label": "Download", onclick: () => downloadFiles([f.path], `${d.item.title} episode ${e.number}`) }, icon("down"));
  return el("div", { class: "mb-ep" + (f ? "" : " off") },
    el("div", { class: "mb-ep-n" }, e.number),
    el("div", { class: "mb-ep-main" },
      el("div", { class: "mb-ep-name" }, e.name || `Episode ${e.number}`),
      el("div", { class: "mb-card-sub" }, e.runtime ? mins(e.runtime) : "", f && !f.local ? ` · ${bytes(f.size)}` : "", f && f.local ? " · on this phone" : ""),
      pct ? el("div", { class: "mb-progress" }, el("span", { style: `width:${pct}%` })) : null),
    action);
}

// ---------- requests ----------

async function requestDialog(d) {
  const it = d.item;
  if (it.kind === "movie") {
    const ok = await dialog({
      title: `Request ${it.title}?`,
      body: el("p", {}, "The owner gets a notice, adds it to the library, and it will show up under Downloads when it's ready."),
      buttons: [{ label: "Not now", value: null }, { label: "Request", value: "go", primary: true }],
      run: async () => { await api("/api/mb/request", { kind: "movie", tmdb: it.id, title: it.title, year: it.year || 0, poster: it.poster || "" }); },
    });
    if (ok) { toast("Requested. You'll see it under Downloads."); mbRender(true); }
    return;
  }
  const have = new Map((d.seasons || []).map((s) => [s.number, s]));
  const picks = new Map(); // season -> true (whole) | Set of episodes
  const rows = (d.seasons || []).map((s) => {
    const complete = s.episodes > 0 && s.have >= s.episodes;
    const box = el("input", { type: "checkbox", id: `rq${s.number}`, disabled: complete });
    const epBox = el("div", { class: "mb-rq-eps", hidden: true });
    const more = el("button", { type: "button", class: "mb-link", onclick: async () => {
      epBox.hidden = !epBox.hidden;
      if (!epBox.hidden && !epBox.dataset.loaded) {
        epBox.dataset.loaded = "1";
        try {
          const r = await api(`/api/mb/season?id=${it.id}&n=${s.number}&title=${encodeURIComponent(it.title)}`);
          epBox.replaceChildren(...r.episodes.map((e) => {
            const c = el("input", { type: "checkbox", disabled: !!e.file, "data-ep": e.number });
            c.addEventListener("change", () => { box.checked = false; });
            return el("label", { class: "mb-rq-ep" }, c, ` ${e.number}. ${e.name}`, e.file ? " (in library)" : "");
          }));
        } catch (er) { epBox.textContent = er.message; epBox.dataset.loaded = ""; }
      }
    } }, "Pick episodes");
    return el("div", { class: "mb-rq" },
      el("label", { class: "mb-rq-season" }, box, ` ${s.name || "Season " + s.number}`, el("span", { class: "mb-card-sub" }, ` ${s.episodes} episodes`, s.have ? `, ${s.have} already here` : "")), complete ? null : more, epBox);
  });
  const ok = await dialog({
    title: `Request ${it.title}`,
    body: el("div", { class: "mb-rq-list" }, el("p", { class: "mb-hint" }, "Choose whole seasons, or pick single episodes."), rows),
    buttons: [{ label: "Cancel", value: null }, { label: "Request", value: "go", primary: true }],
    run: async () => {
      const seasons = [], episodes = {};
      for (const s of d.seasons) {
        const box = document.getElementById(`rq${s.number}`);
        if (box && box.checked) { seasons.push(s.number); continue; }
        const picked = [...document.querySelectorAll(`#dlgBody .mb-rq`)].find((x) => x.querySelector(`#rq${s.number}`));
        const eps = picked ? [...picked.querySelectorAll("input[data-ep]:checked")].map((c) => Number(c.dataset.ep)) : [];
        if (eps.length) episodes[s.number] = eps;
      }
      if (!seasons.length && !Object.keys(episodes).length) throw new Error("Choose at least one season or episode.");
      const payload = { kind: "show", tmdb: it.id, title: it.title, year: it.year || 0, poster: it.poster || "", seasons };
      if (Object.keys(episodes).length) payload.episodes = episodes;
      await api("/api/mb/request", payload);
    },
  });
  if (ok) { toast("Requested. You'll see it under Downloads."); mbRender(true); }
}

// ---------- downloading, removing, playing ----------

async function downloadFiles(paths, label) {
  try {
    await api("/api/mb/download", { paths });
    toast(`Downloading ${label}…`);
    if (typeof refresh === "function") refresh();
  } catch (e) { toast(e.message); }
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
  const filter = MB.dlFilter || "all";
  const want = (t) => filter === "all" || (filter === "phone" ? t.local > 0 : t.local < t.total);
  const fileOf = (t) => (t.kind === "movie" ? t.files : t.seasons.flatMap((s) => s.episodes));

  const chips = el("div", { class: "mb-chips" }, [["all", "All"], ["phone", "On this phone"], ["cloud", "Not downloaded"]].map(([k, label]) =>
    el("button", { type: "button", class: "mb-chip" + (k === filter ? " active" : ""), onclick: () => { MB.dlFilter = k; mbRender(true); } }, label)));

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
  const reqs = mine.length ? el("section", { class: "mb-reqs" }, el("div", { class: "mb-row-head" }, el("h2", {}, "Requests")),
    mine.map((r) => {
      const ready = r.state === "fulfilled" || r.available;
      const status = r.state === "declined" ? pill("Not available", "bad") : ready ? pill("Ready to download", "ok") : pill("Requested", "wait");
      const what = r.kind === "movie" ? "Movie" : !r.seasons || !r.seasons.length ? (r.episodes ? "Chosen episodes" : "All seasons") : r.seasons.length === 1 ? `Season ${r.seasons[0]}` : `Seasons ${r.seasons.join(", ")}`;
      const lt = titles.find((t) => t.kind === (r.kind === "movie" ? "movie" : "show") && t.norm === norm(r.title));
      return el("button", { type: "button", class: "mb-req", onclick: () => mbGo("details", { kind: r.kind === "movie" ? "movie" : "show", id: r.tmdb, lib: lt || undefined }) },
        el("div", { class: "mb-req-poster" }, mbImg(r.poster, "w154", r.title)),
        el("div", { class: "mb-req-main" }, el("strong", {}, r.title, r.year ? ` (${r.year})` : ""), el("div", { class: "mb-card-sub" }, what, " · asked ", when(r.at)), status));
    })) : null;

  const movies = titles.filter((t) => t.kind === "movie" && want(t));
  const shows = titles.filter((t) => t.kind === "show" && want(t));
  const libCard = (t) => {
    const mark = t.local === t.total ? pill("On phone", "ok") : t.local ? pill(`${t.local}/${t.total} on phone`, "ok") : pill("Download", "info");
    return el("button", { type: "button", class: "mb-card", onclick: () => mbGo("details", { kind: t.kind, id: t.tmdb || 0, lib: t }) },
      el("div", { class: "mb-poster" }, mbImg(t.poster, "w342", t.title), mark),
      el("div", { class: "mb-card-title" }, t.title), el("div", { class: "mb-card-sub" }, t.year || "", t.kind === "show" ? `${t.seasons.length} season${t.seasons.length === 1 ? "" : "s"}` : ` · ${bytes(t.size)}`));
  };
  const empty = !titles.length ? el("div", { class: "mb-empty" }, icon("film"), el("p", {}, "Nothing here yet."), el("p", { class: "mb-hint" }, "Request a movie or show from Home. When the owner adds it, it appears here to download and watch offline.")) : null;
  return el("div", { class: "mb-downloads" }, chips, cont, reqs,
    movies.length ? el("section", {}, el("h2", {}, "Movies"), el("div", { class: "mb-grid" }, movies.map(libCard))) : null,
    shows.length ? el("section", {}, el("h2", {}, "TV shows"), el("div", { class: "mb-grid" }, shows.map(libCard))) : null,
    !movies.length && !shows.length && titles.length ? el("p", { class: "mb-hint" }, "Nothing matches that filter.") : null, empty);
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
      el("div", { class: "mb-set-links" },
        el("a", { href: "tgvault://check-update" }, "Check for an update"),
        el("a", { href: "tgvault://share-log" }, "Share the log")),
      el("p", { class: "mb-hint" }, "This product uses the TMDB API but is not endorsed or certified by TMDB.")),
    buttons: [{ label: "Close", value: null }, { label: "Save", value: "save", primary: true }],
    run: async () => { if (key.value.trim()) { await api("/api/mb/subs/key", { key: key.value }); MB.info = null; } },
  });
}

async function mbInfo(fresh) {
  if (!MB.info || fresh) MB.info = await api("/api/mb/info");
  return MB.info;
}

// ---------- download progress, from the state that app.js polls ----------

function mbOnState() {
  if (!state || !state.engine) return;
  const tasks = (state.engine.tasks || []).filter((t) => t.kind === "restore");
  const running = tasks.filter((t) => !t.finished);
  const banner = $("mbBanner");
  if (running.length) {
    const t = running[0];
    banner.hidden = false;
    banner.textContent = `Downloading${t.total > 1 ? ` ${Math.min(t.done + 1, t.total)} of ${t.total}` : ""}: ${(t.current || t.title || "").split("/").pop()}`;
  } else {
    banner.hidden = true;
  }
  const now = new Set(running.map((t) => t.id));
  const before = MB.running || new Set();
  let finished = false;
  for (const t of tasks) {
    if (t.finished && before.has(t.id)) {
      finished = true;
      toast((t.errors || []).length ? `Download problem: ${t.errors[0]}` : "Download finished.");
      api(`/api/tasks/${t.id}/dismiss`, {}).catch(() => {});
    }
  }
  MB.running = now;
  if (finished) mbRefreshCurrent();
}
