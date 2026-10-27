"use strict";

// ---------- helpers ----------

const $ = (id) => document.getElementById(id);

// el("div", {class: "x", onclick: f}, child, "text", ...) builds DOM safely.
function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else if (k === "class") node.className = v;
    else if (k === "style") node.style.cssText = v; // CSSOM is allowed by the page's CSP
    else node.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

async function api(path, body) {
  const opts = body === undefined ? {} : {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  };
  const res = await fetch(path, opts);
  let data = null;
  try { data = await res.json(); } catch { /* empty */ }
  if (!res.ok) throw new Error((data && data.error) || `Request failed (${res.status})`);
  return data;
}

function bytes(n) {
  if (!n) return "0 B";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i && n < 10 ? 1 : 0)} ${u[i]}`;
}

function when(unix) {
  if (!unix) return "";
  const d = new Date(unix * 1000);
  const diff = (Date.now() - d) / 1000;
  if (diff < 60) return "just now";
  if (diff < 3600) return `${Math.floor(diff / 60)} min ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} h ago`;
  return d.toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" });
}

let toastTimer;
function toast(msg) {
  const t = $("toast");
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, 4000);
}

function replace(node, ...children) {
  node.replaceChildren(...children.flat().filter(Boolean));
}

// Dialog: buttons is [{label, value, primary, danger}]; resolves with the value
// (or null), and run(value) may throw to show an error and keep it open.
function dialog({ title, body, buttons, run }) {
  return new Promise((resolve) => {
    const dlg = $("dlg");
    $("dlgTitle").textContent = title;
    replace($("dlgBody"), body);
    $("dlgError").hidden = true;
    const actions = buttons.map((b) => el("button", {
      type: "button",
      class: b.primary ? "primary" : b.danger ? "danger" : "",
      onclick: async (ev) => {
        if (b.value === null || !run) { dlg.close(); resolve(b.value); return; }
        ev.target.disabled = true;
        try {
          await run(b.value);
          dlg.close();
          resolve(b.value);
        } catch (err) {
          $("dlgError").textContent = err.message;
          $("dlgError").hidden = false;
        } finally {
          ev.target.disabled = false;
        }
      },
    }, b.label));
    replace($("dlgActions"), actions);
    dlg.onclose = () => resolve(null);
    dlg.showModal();
  });
}

// ---------- state ----------

let state = null;
let tab = "overview";
let cloudPath = "";

let loadedUI = null;

async function refresh() {
  try {
    state = await api("/api/state");
  } catch {
    $("connText").textContent = "tgvault is not running";
    $("conn").className = "conn warn";
    return;
  }
  // tgvault was updated while this page stayed open: load the new page.
  if (loadedUI && state.ui && state.ui !== loadedUI) { location.reload(); return; }
  loadedUI = state.ui;
  render();
  maybeReloadCloud();
  reportFinishedTasks();
}

// Tell the user when a task (download, restore, …) ends, with any errors.
const reportedTasks = new Set();
let tasksSeeded = false;
function reportFinishedTasks() {
  for (const t of state.engine.tasks || []) {
    if (!t.finished || reportedTasks.has(t.id)) continue;
    reportedTasks.add(t.id);
    if (!tasksSeeded) continue; // finished before this page opened
    if ((t.errors || []).length) {
      const list = t.errors.slice(0, 20);
      dialog({
        title: `${t.title}: ${t.errors.length} problem${t.errors.length > 1 ? "s" : ""}`,
        body: el("div", {}, el("ul", { class: "scroll error" }, list.map((x) => el("li", {}, x))),
          t.errors.length > list.length ? el("p", { class: "hint" }, `…and ${t.errors.length - list.length} more, listed at the top of the page.`) : null),
        buttons: [{ label: "OK", value: null }],
      });
    } else {
      toast(`${t.title} — done.${(t.notes || []).length ? " " + t.notes[t.notes.length - 1] : ""}`);
    }
  }
  tasksSeeded = true;
}

function render() {
  const tg = state.telegram;
  // A logged-in phone disconnects while in the background; keep showing the
  // app while it reconnects.
  const resuming = (tg.status === "idle" || tg.status === "connecting") && !!tg.account;
  const ready = tg.status === "ready" || resuming;
  $("login").hidden = ready;
  $("main").hidden = !ready;

  const conn = $("conn");
  if (ready && tg.online) {
    conn.className = "conn ok";
    $("connText").textContent = "Connected";
  } else if (ready) {
    conn.className = "conn warn";
    $("connText").textContent = resuming && !tg.error ? "Connecting…" : "Reconnecting…";
  } else {
    conn.className = "conn";
    $("connText").textContent = "Not logged in";
  }

  if (!ready) return renderLogin(tg);
  renderOverview();
  renderSettings();
  const failed = state.engine.counts.failed || 0;
  $("activityBadge").hidden = !failed;
  $("activityBadge").textContent = failed;
}

// ---------- login ----------

let lastStep = "";
let lastQR = "";
const onPhone = /Android/i.test(navigator.userAgent);
if (onPhone) document.documentElement.classList.add("phone");
const here = onPhone ? "this phone" : "this PC";
let lastServerError = "";
function renderLogin(tg) {
  const steps = {
    need_api: "step-api", need_phone: "step-phone", need_code: "step-code",
    need_password: "step-password", need_qr: "step-qr", connecting: "step-wait",
  };
  const step = steps[tg.status] || "step-wait";
  for (const s of Object.values(steps)) $(s).hidden = s !== step;
  if (step !== lastStep) {
    lastStep = step;
    $("loginError").hidden = true;
    const input = $(step).querySelector("input");
    if (input) input.focus();
  }
  // Errors reported by the app itself (e.g. the QR code expired).
  if (tg.error && step !== "step-wait" && tg.error !== lastServerError) {
    $("loginError").textContent = tg.error;
    $("loginError").hidden = false;
  }
  lastServerError = tg.error || "";

  $("codePhone").textContent = tg.phone || "";
  $("codeVia").textContent = tg.codeVia || "";
  $("resendCode").textContent = tg.codeNext || "Send the code again";
  $("waitText").textContent = tg.error || "Connecting to Telegram.";

  const pairing = state.pairing;
  $("pairBox").hidden = !pairing || tg.status === "need_password";
  if (pairing) {
    $("pairAddr").textContent = pairing.addresses.join("  or  ");
    $("pairCode").textContent = pairing.code;
  }

  const url = tg.qrUrl || "";
  if (url !== lastQR) {
    lastQR = url;
    $("qrImg").hidden = !url;
    $("qrWait").hidden = !!url;
    if (url) $("qrImg").src = `/api/login/qr.png?v=${Date.now()}`;
  }
}

async function loginAction(url, okMsg) {
  $("loginError").hidden = true;
  try {
    await api(url, {});
    if (okMsg) toast(okMsg);
  } catch (err) {
    $("loginError").textContent = err.message;
    $("loginError").hidden = false;
  }
  refresh();
}

function loginForm(formId, url, field) {
  $(formId).addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const form = ev.target;
    const btn = form.querySelector("button");
    const data = Object.fromEntries(new FormData(form));
    btn.disabled = true;
    $("loginError").hidden = true;
    try {
      await api(url, field ? { value: data.value } : data);
      if (field) form.reset();
      await refresh();
    } catch (err) {
      $("loginError").textContent = err.message;
      $("loginError").hidden = false;
    } finally {
      btn.disabled = false;
    }
  });
}

// ---------- overview ----------

function renderPhoneBanner() {
  const ph = state.phone;
  const banner = $("phoneBanner");
  if (!ph) { banner.hidden = true; return; }
  const p = ph.permissions;
  const asks = [];
  if (!p.storage) asks.push(["tgvault needs access to your files to back them up and restore them.", "Allow access to files", "tgvault://perm/storage"]);
  if (!p.battery) asks.push(["Android may stop backups in the background unless tgvault is allowed to run.", "Allow background backups", "tgvault://perm/battery"]);
  if (!p.notifications) asks.push(["Allow notifications so tgvault can tell you when something needs you.", "Allow notifications", "tgvault://perm/notifications"]);
  banner.hidden = !asks.length;
  replace(banner, asks.length ? el("h2", {}, asks.length > 1 ? "A few permissions are needed" : "One permission is needed") : null,
    ...asks.map(([text, label, href], i) => el("div", { class: "ask" },
      el("p", {}, text),
      el("a", { class: i === 0 ? "btn primary" : "btn", href }, label))));
}

let suggestions = null;
async function renderSuggestions() {
  const card = $("suggestCard");
  const show = onPhone && state.phone && state.phone.permissions.storage && !(state.roots || []).length;
  if (!show) { card.hidden = true; suggestions = null; return; }
  if (suggestions === null) {
    suggestions = [];
    try { suggestions = (await api("/api/suggest-folders")).folders || []; } catch { /* shown as empty */ }
    replace($("suggestList"), suggestions.map((f) => el("li", {},
      el("label", { class: "toggle grow" },
        el("input", { type: "checkbox", value: f.path, "data-name": f.label, checked: f.checked }),
        el("div", {}, el("div", { class: "name" }, f.label), f.hint ? el("div", { class: "sub" }, f.hint) : null)))));
  }
  card.hidden = !suggestions.length;
}

async function addSuggested() {
  const picked = [...$("suggestList").querySelectorAll("input:checked")].map((i) => ({ path: i.value, name: i.dataset.name }));
  if (!picked.length) { toast("Tick at least one folder, or use Add folder below."); return; }
  // Add parents first, so a folder inside another selected one is reported, not duplicated.
  picked.sort((a, b) => a.path.length - b.path.length);
  const problems = [];
  for (const f of picked) {
    try { await api("/api/roots", f); } catch (err) { problems.push(`${f.name}: ${err.message}`); }
  }
  toast(problems.length ? problems.join("; ") : "Folders added; scanning now.");
  suggestions = null;
  refresh();
}

function renderOverview() {
  const e = state.engine;
  const c = e.counts;
  const uploading = e.uploading || [];
  const waiting = (c.pending || 0) + (c.uploading || 0);
  const failed = c.failed || 0;
  const held = (state.projects || []).filter((p) => p.state === "suggested").length;

  const n = (x) => x.toLocaleString();
  const cool = e.cooldownUntil ? new Date(e.cooldownUntil * 1000) : null;
  let icon = "✅", big = "Everything is backed up", sub = `${n(c.synced || 0)} files and projects are safe in Telegram.`;
  if (!state.roots || !state.roots.length) {
    icon = "📁"; big = "Add a folder to start"; sub = "Pick the folders you want backed up below.";
  } else if (cool) {
    const mins = Math.max(1, Math.ceil((cool - Date.now()) / 60000));
    icon = "⏳"; big = `Pausing for Telegram's speed limit — resumes at ${cool.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`;
    sub = `Telegram limits how fast one account can send files. Nothing is wrong; tgvault waits about ${mins} min and then continues by itself. ${n(waiting)} item${waiting === 1 ? "" : "s"} queued.`;
  } else if (waiting) {
    icon = "⏫"; big = `Backing up ${n(waiting)} item${waiting === 1 ? "" : "s"}`;
    sub = "Files go up a few at a time to stay within Telegram's speed limit.";
  } else if (e.scanning) {
    icon = "🔎"; big = "Checking your folders for changes"; sub = "";
  }
  if (e.blocked && waiting) {
    icon = "📶"; big = e.blocked; sub = `${n(waiting)} item${waiting === 1 ? "" : "s"} will go up as soon as that changes. You can change this in Settings.`;
  }
  if (failed && state.roots && state.roots.length) {
    sub += ` ${n(failed)} item${failed === 1 ? " needs" : "s need"} attention — see Activity.`;
  }

  const ups = uploading.sort((a, b) => a.cloudPath.localeCompare(b.cloudPath)).map((u) => {
    const pct = u.total ? Math.round((u.done / u.total) * 100) : 0;
    const part = u.parts ? (u.part ? ` · part ${u.part} of ${u.parts}` : ` · ${u.parts} parts`) : "";
    const label = (u.phase === "uploading" ? `${bytes(u.done)} of ${bytes(u.total)}` : u.phase) + part;
    return el("div", { class: "uploads" },
      el("div", { class: "sub" }, `${u.cloudPath} — ${label}`),
      el("div", { class: "progress" }, el("div", { style: `width:${pct}%` })));
  });
  replace($("summary"),
    el("div", { class: "state-icon", "aria-hidden": "true" }, icon),
    el("div", { class: "grow" },
      el("div", { class: "big" }, big),
      el("div", { class: "hint" }, sub, held ? ` ${held} project folder(s) need your decision below.` : ""),
      ups));

  renderPhoneBanner();
  renderSuggestions();

  // Tasks (restores, freeing space)
  replace($("tasks"), (e.tasks || []).map((t) => {
    const pct = t.total ? Math.round((t.done / t.total) * 100) : 0;
    return el("div", { class: "card" },
      el("div", { class: "row-head" },
        el("h2", {}, t.title, t.finished ? " — done" : ""),
        t.finished ? el("button", { class: "small", onclick: () => api(`/api/tasks/${t.id}/dismiss`, {}).then(refresh) }, "Dismiss") : null),
      el("div", { class: "sub" }, t.finished ? `${t.done} of ${t.total} handled` : `${t.done} of ${t.total} — ${t.current || ""}`),
      t.finished ? null : el("div", { class: "progress" }, el("div", { style: `width:${pct}%` })),
      (t.errors || []).length ? el("ul", { class: "scroll error" }, t.errors.map((x) => el("li", {}, x))) : null,
      (t.notes || []).length ? el("ul", { class: "scroll" }, t.notes.map((x) => el("li", {}, x))) : null);
  }));

  // Projects waiting for a decision
  const suggested = (state.projects || []).filter((p) => p.state === "suggested");
  $("projectsCard").hidden = !suggested.length;
  replace($("projects"), suggested.map((p) => el("li", {},
    el("span", { class: "icon" }, "📦"),
    el("div", { class: "grow" },
      el("div", { class: "name" }, p.rel ? `${p.root}/${p.rel}` : p.root),
      el("div", { class: "sub" }, `Found: ${p.markers}`)),
    el("div", { class: "actions" },
      el("button", { class: "small primary", onclick: () => setProject(p.id, "confirmed") }, "Yes, back up as project"),
      el("button", { class: "small", onclick: () => setProject(p.id, "ignored") }, "No, normal files")))));

  // Roots
  const roots = state.roots || [];
  $("rootsEmpty").hidden = roots.length > 0;
  replace($("roots"), roots.map((r) => el("li", {},
    el("span", { class: "icon" }, "📁"),
    el("div", { class: "grow" },
      el("div", { class: "name" }, r.label),
      el("div", { class: "sub" }, r.path),
      r.scanError ? el("div", { class: "sub" }, el("span", { class: "pill warn" }, r.scanError)) : null,
      r.paused ? el("span", { class: "pill warn" }, "Paused") : null),
    el("div", { class: "actions" },
      el("button", { class: "small", onclick: () => api(`/api/roots/${r.id}/pause`, { paused: !r.paused }).then(refresh) }, r.paused ? "Resume" : "Pause"),
      el("button", { class: "small danger", onclick: () => removeRoot(r) }, "Stop backing up")))));
}

async function setProject(id, st) {
  try {
    await api(`/api/projects/${id}`, { state: st });
    toast(st === "confirmed" ? "It will be backed up as one ZIP." : "It will be backed up as normal files.");
    refresh();
  } catch (err) { toast(err.message); }
}

// Folder picker for the phone, which has no system folder dialog tgvault can use.
async function pickFolderOnPhone(title = "Choose a folder to back up", button = "Back up this folder") {
  let current = "";
  const where = el("div", { class: "sub" });
  const list = el("ul", { class: "list folder-pick" });
  const vols = el("div", { class: "actions" });
  async function go(p) {
    try {
      const r = await api(`/api/browse?path=${encodeURIComponent(p)}`);
      current = r.path;
      where.textContent = r.path;
      replace(vols, (r.volumes || []).length > 1
        ? r.volumes.map((v) => el("button", { type: "button", class: "small", onclick: () => go(v.path) }, v.label)) : []);
      replace(list,
        r.parent ? el("li", { class: "clickable", onclick: () => go(r.parent) }, el("span", { class: "icon" }, "⬆️"), el("div", { class: "grow name" }, "Up one folder")) : null,
        (r.dirs || []).map((d) => el("li", { class: "clickable", onclick: () => go(`${r.path}/${d}`) }, el("span", { class: "icon" }, "📁"), el("div", { class: "grow name" }, d))));
    } catch (err) { toast(err.message); }
  }
  await go("");
  let picked = null;
  await dialog({
    title,
    body: el("div", {}, vols, where, list),
    buttons: [{ label: "Cancel", value: null }, { label: button, value: "pick", primary: true }],
    run: async () => { picked = current; },
  });
  return picked;
}

async function addFolder() {
  if (onPhone) {
    const p = await pickFolderOnPhone();
    if (!p) return;
    try {
      await api("/api/roots", { path: p });
      toast("Folder added; scanning now.");
      refresh();
    } catch (err) { toast(err.message); }
    return;
  }
  const input = el("input", { type: "text", placeholder: "C:\\Users\\you\\Documents", autocomplete: "off" });
  const body = el("div", {},
    el("p", { class: "hint" }, "Everything inside this folder (including subfolders) will be backed up and kept up to date."),
    el("label", {}, "Folder", input),
    el("button", { type: "button", class: "small", onclick: async () => {
      try {
        const r = await api("/api/pick-folder", {});
        if (r.path) input.value = r.path;
      } catch (err) { toast(err.message); }
    } }, "Browse…"));
  setTimeout(() => input.focus(), 50);
  await dialog({
    title: "Add a folder to back up", body,
    buttons: [{ label: "Cancel", value: null }, { label: "Add folder", value: "add", primary: true }],
    run: async () => { await api("/api/roots", { path: input.value }); toast("Folder added; scanning now."); refresh(); },
  });
}

async function removeRoot(r) {
  await dialog({
    title: `Stop backing up ${r.label}?`,
    body: el("p", {}, "tgvault will stop watching this folder. Copies already in Telegram are kept, and you can still restore them from Cloud files."),
    buttons: [{ label: "Cancel", value: null }, { label: "Stop backing up", value: "rm", danger: true }],
    run: async () => { await api(`/api/roots/${r.id}/remove`, {}); refresh(); },
  });
}

// ---------- cloud files ----------

const localLabels = {
  same: [`On ${here}`, "ok"],
  changed: [`Changed on ${here}`, "info"],
  "cloud-only": ["Cloud only", "warn"],
  mixed: [`Partly on ${here}`, "info"],
};

function localLabel(e) {
  if (e.local === "other-device") return [e.device === "Phone" ? "From your phone" : "From your PC", "info"];
  return localLabels[e.local] || ["", ""];
}

const typeIcons = { audio: "🎵", video: "🎬", image: "🖼️", document: "📄", archive: "🗜️", other: "📄" };

function deviceOf(p) { return p === "Phone" || p.startsWith("Phone/") ? "Phone" : "PC"; }
function otherDevice(p) { return p !== "" && deviceOf(p) !== state.engine.device; }

function showError(message, title = "That didn't work") {
  dialog({ title, body: el("p", {}, message), buttons: [{ label: "OK", value: null }] });
}

// The browser state: current folder, filters, sort and selection.
const cloud = {
  entries: [], total: 0, searching: false, stamp: "", loadedAt: 0,
  sort: "name", desc: false,
  selected: new Map(), // path -> entry
  order: [],    // entries as shown, so ranges follow the list on screen
  anchor: null, // path of the item ranges start from
  list: "cloudList",
  key: (e) => e.path,
  open: (e) => { if (e.kind !== "folder") return false; openCloud(e.path); return true; },
  view: (e) => { if (e.kind === "file") openViewer(e); },
  render: () => renderSelection(),
};

function cloudFilters() {
  return {
    q: $("cloudSearch").value.trim(), type: $("cloudType").value,
    from: $("cloudFrom").value, to: $("cloudTo").value,
  };
}

async function loadCloud() {
  const f = cloudFilters();
  cloud.searching = !!(f.q || f.type || f.from || f.to);
  let data;
  try {
    if (cloud.searching) {
      const qs = new URLSearchParams({ path: cloudPath, q: f.q, type: f.type, from: f.from, to: f.to, limit: "2000" });
      data = await api(`/api/search?${qs}`);
    } else {
      data = await api(`/api/cloud?path=${encodeURIComponent(cloudPath)}`);
    }
  } catch (err) { toast(err.message); return; }
  cloud.entries = data.entries || [];
  cloud.total = data.total || cloud.entries.length;
  cloud.stamp = state ? state.cloudStamp : "";
  cloud.loadedAt = Date.now();
  renderCloud();
}

function sortEntries(list) {
  const dir = cloud.desc ? -1 : 1;
  const by = {
    name: (a, b) => a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: "base" }),
    mtime: (a, b) => (a.mtime || 0) - (b.mtime || 0),
    size: (a, b) => a.size - b.size,
    type: (a, b) => (a.type || "").localeCompare(b.type || "") || a.name.localeCompare(b.name),
  }[cloud.sort];
  return [...list].sort((a, b) => {
    const fa = a.kind === "folder", fb = b.kind === "folder";
    if (fa !== fb) return fa ? -1 : 1; // folders stay on top
    return by(a, b) * dir;
  });
}

function renderCloud() {
  const parts = cloudPath ? cloudPath.split("/") : [];
  const crumbs = [el("button", { onclick: () => openCloud("") }, "All backups")];
  parts.forEach((p, i) => {
    crumbs.push(el("span", {}, "/"));
    const target = parts.slice(0, i + 1).join("/");
    crumbs.push(el("button", { onclick: () => openCloud(target) }, p));
  });
  replace($("crumbs"), crumbs);

  const where = cloudPath ? `in ${cloudPath} and its subfolders` : "everywhere";
  $("cloudInfo").textContent = cloud.searching
    ? `${cloud.total.toLocaleString()} match${cloud.total === 1 ? "" : "es"} ${where}${cloud.total > cloud.entries.length ? ` (showing the first ${cloud.entries.length.toLocaleString()})` : ""}`
    : "";
  $("cloudEmpty").hidden = cloud.entries.length > 0;
  $("cloudEmpty").textContent = cloud.searching ? "Nothing matches." : "Nothing here yet.";
  $("cloudDir").textContent = cloud.desc ? "↓" : "↑";

  cloud.order = sortEntries(cloud.entries);
  replaceRows(cloud, cloud.order.map(cloudRow));
  renderSelection();
}

function cloudRow(e) {
  const [label, cls] = localLabel(e);
  const isFolder = e.kind === "folder";
  const icon = isFolder ? "📁" : e.kind === "project" ? "📦" : typeIcons[e.type] || "📄";
  const folder = e.path.includes("/") ? e.path.slice(0, e.path.lastIndexOf("/")) : "";
  const sub = isFolder
    ? `${e.files} item${e.files > 1 ? "s" : ""} · ${bytes(e.size)}`
    : `${bytes(e.size)} · modified ${when(e.mtime)}${e.versions > 1 ? ` · ${e.versions} versions` : ""}${e.parts ? ` · stored in ${e.parts} parts` : ""}${e.kind === "project" ? " · project ZIP" : ""}${e.changes ? ` · ${plural(e.changes, "change")} since its full backup` : ""}`;
  const box = pickBox(cloud, e, e.name);
  // Download and Restore act on the whole selection when this item is part of it.
  const targets = () => (cloud.selected.has(e.path) && cloud.selected.size > 1 ? [...cloud.selected.values()] : [e]);
  const actions = isFolder ? [] : [
    e.kind === "file" ? el("button", { class: "small", onclick: (ev) => { ev.stopPropagation(); openViewer(e); } }, "View") : null,
    el("button", { class: "small", "data-label": "Download", onclick: (ev) => { ev.stopPropagation(); bulkDownload(targets()); } }, "Download"),
    el("button", { class: "small", "data-label": "Restore", onclick: (ev) => { ev.stopPropagation(); restore(targets()); } }, "Restore"),
    e.versions > 1 ? el("button", { class: "small", onclick: (ev) => { ev.stopPropagation(); showVersions(e); } }, "Versions") : null,
  ];
  return el("li", pickRow(cloud, e, {
    class: isFolder ? "clickable" : "",
    ondblclick: (ev) => { if (e.kind === "file" && !ev.target.closest("button, input, a")) openViewer(e); },
  }),
    box,
    el("span", { class: "icon" }, icon),
    el("div", { class: "grow" },
      el("div", { class: "name" }, e.name),
      cloud.searching && folder ? el("div", { class: "sub" }, `in ${folder}`) : null,
      el("div", { class: "sub" }, sub, " ", label ? el("span", { class: `pill ${cls}` }, label) : null)),
    el("div", { class: "actions" }, actions));
}

function openCloud(p) {
  cloudPath = p;
  cloud.selected.clear();
  cloud.anchor = null;
  loadCloud();
}

// ---- selection and bulk actions ----

// Selectable lists (Cloud files, Free up space) share these handlers. A list
// ("picker") has: selected (key -> item), order (items as shown), anchor (key
// ranges start from), list (the <ul> id), key(item), render() and, optionally,
// open(item) for items a plain click opens instead of selecting.

// Click: select only this item (folders open). Ctrl/Cmd+Click adds or removes
// one item; Shift+Click selects from the anchor to this item.
function rowClick(p, ev, x) {
  if (ev.target.closest("button, input, a")) return;
  if (ev.shiftKey) selectRange(p, x, ev.ctrlKey || ev.metaKey);
  else if (ev.ctrlKey || ev.metaKey) toggleOne(p, x);
  else if (!(p.open && p.open(x))) selectOnly(p, x);
}

function selectOnly(p, x) {
  p.selected.clear();
  p.selected.set(p.key(x), x);
  p.anchor = p.key(x);
  p.render();
}

function toggleOne(p, x) {
  const k = p.key(x);
  if (p.selected.has(k)) p.selected.delete(k);
  else { p.selected.set(k, x); p.anchor = k; }
  p.render();
}

function clearSelection(p) {
  p.selected.clear();
  p.anchor = null;
  p.render();
}

// selectRange selects every item from the anchor to x, inclusive, in the
// order shown, whether or not those rows are on screen. With add, the range is
// added to the selection; otherwise it replaces it. With nothing selected the
// first item is the anchor.
function selectRange(p, x, add) {
  const list = p.order;
  let from = p.selected.size ? list.findIndex((y) => p.key(y) === p.anchor) : -1;
  if (from < 0) from = 0;
  const to = list.findIndex((y) => p.key(y) === p.key(x));
  if (to < 0 || !list.length) return;
  p.anchor = p.key(list[from]);
  if (!add) p.selected.clear();
  for (let i = Math.min(from, to); i <= Math.max(from, to); i++) p.selected.set(p.key(list[i]), list[i]);
  p.render();
}

// The row's checkbox: a plain click adds or removes the row, Shift+Click
// adds the range from the anchor.
function pickBox(p, x, name) {
  let shift = false;
  return el("input", {
    type: "checkbox", class: "pick", "aria-label": `Select ${name}`, checked: p.selected.has(p.key(x)),
    onclick: (ev) => { ev.stopPropagation(); shift = ev.shiftKey; },
    onchange: (ev) => {
      if (shift) selectRange(p, x, true);
      else if (ev.target.checked !== p.selected.has(p.key(x))) toggleOne(p, x);
    },
  });
}

// Attributes that make an <li> a selectable row of picker p.
function pickRow(p, x, attrs = {}) {
  return {
    ...attrs, tabindex: "0", "data-path": p.key(x),
    onmousedown: (ev) => { if (ev.shiftKey) ev.preventDefault(); }, // no text selection on Shift+Click
    onclick: (ev) => rowClick(p, ev, x),
    oncontextmenu: (ev) => rowMenu(p, ev, x),
  };
}

// Arrow keys move between rows; Shift+Up/Down grows or shrinks the range
// from the anchor; Space toggles the row; Enter opens a folder.
function listKeys(p, ev) {
  const li = ev.target.closest && ev.target.closest("li[data-path]");
  if (!li || ev.target !== li) return;
  const i = p.order.findIndex((y) => p.key(y) === li.dataset.path);
  const x = p.order[i];
  if (!x) return;
  if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
    const next = p.order[i + (ev.key === "ArrowDown" ? 1 : -1)];
    if (!next) return;
    ev.preventDefault();
    if (ev.shiftKey) {
      if (!p.selected.size || !p.order.some((y) => p.key(y) === p.anchor)) {
        p.selected.set(p.key(x), x);
        p.anchor = p.key(x);
      }
      selectRange(p, next, false);
    }
    rowFor(p, p.key(next))?.focus();
  } else if (ev.key === " ") {
    ev.preventDefault();
    toggleOne(p, x);
  } else if (ev.key === "Enter") {
    if (!(p.open && p.open(x)) && p.view) p.view(x);
  }
}

function rowFor(p, key) {
  return [...$(p.list).children].find((li) => li.dataset.path === key);
}

// Right-click (long-press on the phone) menu: "Select up to here" and, on a
// selected item, "Deselect".
function rowMenu(p, ev, x) {
  ev.preventDefault();
  closeMenu();
  const items = [el("button", { type: "button", onclick: () => { closeMenu(); selectRange(p, x, true); } }, "Select up to here")];
  if (p.selected.has(p.key(x))) {
    items.push(el("button", { type: "button", onclick: () => { closeMenu(); toggleOne(p, x); } }, "Deselect"));
  }
  const menu = el("div", { class: "ctxmenu", id: "ctxmenu", role: "menu" }, items);
  document.body.append(menu);
  const r = menu.getBoundingClientRect();
  menu.style.left = `${Math.max(4, Math.min(ev.clientX, innerWidth - r.width - 4))}px`;
  menu.style.top = `${Math.max(4, Math.min(ev.clientY, innerHeight - r.height - 4))}px`;
  items[0].focus();
}

function closeMenu() {
  const m = $("ctxmenu");
  if (m) { m.remove(); return true; }
  return false;
}

// Touch screens: holding a row for half a second opens the row menu, without
// relying on the browser turning the long-press into a contextmenu event. The
// touch that ends the press doesn't also select the row.
function longPress(p) {
  const list = $(p.list);
  let timer = null, start = null, fired = false;
  const cancel = () => { clearTimeout(timer); timer = null; };
  list.addEventListener("touchstart", (ev) => {
    const li = ev.target.closest("li[data-path]");
    fired = false;
    cancel();
    if (!li || ev.touches.length !== 1 || ev.target.closest("button, input, a")) return;
    const t = ev.touches[0];
    start = { x: t.clientX, y: t.clientY };
    timer = setTimeout(() => {
      timer = null;
      const x = p.order.find((y) => p.key(y) === li.dataset.path);
      if (!x) return;
      fired = true;
      rowMenu(p, { preventDefault() {}, clientX: start.x, clientY: start.y }, x);
    }, 500);
  }, { passive: true });
  list.addEventListener("touchmove", (ev) => {
    const t = ev.touches[0];
    if (timer && Math.hypot(t.clientX - start.x, t.clientY - start.y) > 10) cancel();
  }, { passive: true });
  list.addEventListener("touchend", (ev) => { cancel(); if (fired) ev.preventDefault(); });
  list.addEventListener("touchcancel", cancel);
  // The browser's own long-press menu, and a click that may follow, would
  // undo ours.
  list.addEventListener("contextmenu", (ev) => { if (fired) { ev.preventDefault(); ev.stopPropagation(); } }, true);
  list.addEventListener("click", (ev) => { if (fired) { fired = false; ev.preventDefault(); ev.stopPropagation(); } }, true);
}

// replaceRows swaps in freshly rendered rows. The list reloads by itself while
// backups run, so the focused row keeps focus (Shift+Up/Down carries on).
function replaceRows(p, rows) {
  const list = $(p.list);
  const focused = list.contains(document.activeElement) && document.activeElement.closest("li[data-path]");
  const key = focused && focused.dataset.path;
  replace(list, rows);
  if (key) rowFor(p, key)?.focus({ preventScroll: true });
}

// Marks selected rows and updates per-row buttons that act on the selection.
function renderRows(p) {
  const n = p.selected.size;
  for (const li of $(p.list).children) {
    const on = p.selected.has(li.dataset.path);
    li.classList.toggle("selected", on);
    li.setAttribute("aria-selected", on ? "true" : "false");
    const box = li.querySelector(".pick");
    if (box) box.checked = on;
    for (const b of li.querySelectorAll("button[data-label]")) {
      b.textContent = on && n > 1 ? `${b.dataset.label} ${n.toLocaleString()}` : b.dataset.label;
    }
  }
}

function renderSelection() {
  renderRows(cloud);
  const sel = [...cloud.selected.values()];
  const n = sel.length;
  const visible = cloud.entries.length;
  const all = $("selAll");
  all.checked = visible > 0 && cloud.entries.every((e) => cloud.selected.has(e.path));
  all.indeterminate = n > 0 && !all.checked;
  $("selCount").textContent = n ? `${n.toLocaleString()} selected` : "Select all";
  const anyHere = sel.some((e) => !otherDevice(e.path));
  const anyLocal = sel.some((e) => e.local === "same" || e.local === "mixed" || e.kind === "folder");
  replace($("selActions"), n ? [
    el("button", { class: "small primary", onclick: () => bulkDownload(sel) }, "Download"),
    el("button", { class: "small", onclick: () => restore(sel) }, "Restore"),
    el("button", { class: "small", onclick: () => copyTo(sel) }, "Copy to…"),
    el("button", { class: "small", onclick: () => moveInBackup(sel) }, "Move in backup…"),
    onPhone ? el("button", { class: "small", onclick: () => shareFiles(sel) }, "Share")
      : el("button", { class: "small", onclick: () => copyLinks(sel) }, "Copy Telegram links"),
    anyHere && anyLocal ? el("button", { class: "small", onclick: () => freeSpace(sel.map((e) => e.path)) }, `Delete from ${here}`) : null,
    el("button", { class: "small danger", onclick: () => removeFromBackup(sel) }, "Remove from backup"),
    el("button", { class: "small link", onclick: () => { cloud.selected.clear(); renderCloud(); } }, "Clear"),
  ] : []);
  $("selActions").hidden = !n;
}

function selectAll(on) {
  for (const e of cloud.entries) {
    if (on) cloud.selected.set(e.path, e); else cloud.selected.delete(e.path);
  }
  renderCloud();
}

async function startBulk(action, entries, extra = {}) {
  return api("/api/bulk", { action, paths: entries.map((e) => e.path), ...extra });
}

function names(entries) {
  return entries.length === 1 ? entries[0].name : `${entries.length.toLocaleString()} items`;
}

async function bulkDownload(entries) {
  if (onPhone) {
    try {
      await startBulk("download", entries);
      toast(`Downloading ${names(entries)} to Download/tgvault. Progress is at the top of the page.`);
      refresh();
    } catch (err) { showError(err.message, "Couldn't download"); }
    return;
  }
  if (entries.length === 1 && entries[0].kind !== "folder") {
    location.href = `/api/download?path=${encodeURIComponent(entries[0].path)}`;
    return;
  }
  // Several items: one ZIP, sent as a form so the browser saves it.
  const form = el("form", { method: "POST", action: "/api/bulk-zip", hidden: true },
    el("input", { type: "hidden", name: "paths", value: JSON.stringify(entries.map((e) => e.path)) }));
  document.body.append(form);
  form.submit();
  form.remove();
  toast("Preparing a ZIP; your browser will save it.");
}

async function restore(entries) {
  const fromOther = entries.some((e) => otherDevice(e.path));
  if (fromOther) return copyTo(entries, `These were backed up from your ${deviceOf(entries[0].path) === "Phone" ? "phone" : "PC"}, so choose a folder on ${here} to restore them into.`);
  await dialog({
    title: `Restore ${names(entries)}`,
    body: el("div", {},
      el("p", {}, "Put them back where they were on " + here + "."),
      el("p", { class: "hint" }, "Files already there are never overwritten; a different existing file keeps its name and the restored copy gets “(restored)” added.")),
    buttons: [{ label: "Cancel", value: null }, { label: "Copy somewhere else…", value: "other" }, { label: "Restore", value: "go", primary: true }],
    run: async (v) => {
      if (v === "other") { setTimeout(() => copyTo(entries), 0); return; }
      await startBulk("restore", entries, { dest: "" });
      toast("Restoring; progress is at the top of the page.");
      refresh();
    },
  });
}

async function copyTo(entries, why) {
  let dest = null;
  if (onPhone) {
    dest = await pickFolderOnPhone("Choose where to save them", "Save here");
    if (!dest) return;
  } else {
    const input = el("input", { type: "text", placeholder: "C:\\Users\\you\\Restored", autocomplete: "off" });
    const ok = await dialog({
      title: `Copy ${names(entries)} to a folder`,
      body: el("div", {},
        why ? el("p", {}, why) : null,
        el("label", {}, "Folder", input),
        el("button", { type: "button", class: "small", onclick: async () => {
          try { const r = await api("/api/pick-folder", {}); if (r.path) input.value = r.path; } catch (err) { toast(err.message); }
        } }, "Browse…"),
        el("p", { class: "hint" }, "Folders keep their structure. Nothing already there is overwritten.")),
      buttons: [{ label: "Cancel", value: null }, { label: "Copy here", value: "go", primary: true }],
      run: async () => { if (!input.value.trim()) throw new Error("Choose a folder first."); dest = input.value.trim(); },
    });
    if (!ok) return;
  }
  try {
    await startBulk("restore", entries, { dest });
    toast("Copying; progress is at the top of the page.");
    refresh();
  } catch (err) { showError(err.message, "Couldn't copy"); }
}

async function moveInBackup(entries) {
  const folders = [...new Set((await api("/api/cloud?path=")).entries.filter((e) => e.kind === "folder").map((e) => e.path))];
  const list = el("datalist", { id: "moveTargets" }, folders.map((f) => el("option", { value: f })));
  const input = el("input", { type: "text", list: "moveTargets", placeholder: "Documents/Archive", autocomplete: "off", value: cloudPath });
  await dialog({
    title: `Move ${names(entries)} in the backup`,
    body: el("div", {},
      el("p", {}, "Changes where they're filed in your Telegram backup. Nothing is downloaded or deleted."),
      el("label", {}, "Move to this backup folder", input, list),
      el("p", { class: "hint" }, `Only for things no longer backed up from ${here}; otherwise they'd simply upload again at their old place. Files stay in their device's part of the backup (PC or Phone).`)),
    buttons: [{ label: "Cancel", value: null }, { label: "Move", value: "go", primary: true }],
    run: async () => {
      await startBulk("move", entries, { to: input.value });
      toast("Moving; progress is at the top of the page.");
      cloud.selected.clear();
      refresh();
    },
  });
}

async function shareFiles(entries) {
  try {
    await startBulk("share", entries);
    toast("Getting the files ready to share…");
  } catch (err) { showError(err.message, "Couldn't share"); }
}

async function copyLinks(entries) {
  try {
    const { links } = await startBulk("links", entries);
    if (!links.length) throw new Error("Nothing to link to.");
    await navigator.clipboard.writeText(links.join("\n"));
    toast(`Copied ${links.length} Telegram link${links.length > 1 ? "s" : ""}. Paste them into any Telegram chat to share or forward the files.`);
  } catch (err) { showError(err.message, "Couldn't copy the links"); }
}

async function freeSpace(paths) {
  let plan;
  try {
    plan = await api("/api/bulk", { action: "free-plan", paths });
  } catch (err) { showError(err.message); return; }
  const n = plan.candidates.length;
  const body = el("div", {},
    n ? el("p", {}, `Delete ${n.toLocaleString()} file${n > 1 ? "s" : ""} (${bytes(plan.bytes)}) from ${here}.`)
      : el("p", {}, "Nothing here can be deleted yet, because nothing is fully backed up."),
    n ? el("p", { class: "hint" }, `Only the copy on ${here} is deleted. The backup in Telegram stays, and you can restore it any time from Cloud files. Each file is checked against its Telegram copy first.`) : null,
    plan.candidates.some((c) => c.kind === "project")
      ? el("p", { class: "hint" }, "Project folders are deleted whole, including rebuildable folders like node_modules that were not backed up.") : null,
    plan.kept && plan.kept.length ? el("div", {}, el("p", { class: "hint" }, `Kept on ${here}:`),
      el("ul", { class: "scroll" }, plan.kept.slice(0, 200).map((k) => el("li", {}, k)))) : null);
  await dialog({
    title: "Free up space", body,
    buttons: n ? [{ label: "Cancel", value: null }, { label: `Delete ${bytes(plan.bytes)} from ${here}`, value: "go", danger: true }]
      : [{ label: "Close", value: null }],
    run: async () => {
      await api("/api/bulk", { action: "free", paths: plan.candidates.map((c) => c.cloudPath) });
      toast("Freeing space; progress is at the top of the page.");
      cloud.selected.clear();
      refresh();
    },
  });
}

async function removeFromBackup(entries) {
  const others = entries.filter((e) => otherDevice(e.path));
  const from = others.length && deviceOf(others[0].path) === "Phone" ? "your phone" : "your PC";
  await dialog({
    title: "Remove from backup?",
    body: el("div", {},
      el("p", {}, `Permanently delete ${names(entries)}, with every version, from your Telegram backup channel.`),
      others.length ? el("p", {}, `Some were backed up from ${from}. If ${from} still has them, it will back them up again the next time it checks Telegram. To stop that, stop backing up the folder on ${from} first.`) : null,
      el("p", { class: "hint" }, `This can't be undone. Files on ${here} are not touched. It only works for things no longer being backed up from ${here}.`)),
    buttons: [{ label: "Cancel", value: null }, { label: "Delete from Telegram", value: "go", danger: true }],
    run: async () => {
      await startBulk("delete-cloud", entries);
      toast("Removing; progress is at the top of the page.");
      cloud.selected.clear();
      refresh();
    },
  });
}

function plural(n, word) {
  return `${n.toLocaleString()} ${word}${n === 1 ? "" : "s"}`;
}

// What a stored version holds: its size, or for a project change only what
// changed since the version before it.
function versionWhat(v) {
  if (!v.seq) return v.kind === "project" ? `${bytes(v.size)} · full backup` : bytes(v.size);
  const parts = [`${plural(v.changed || 0, "file")} changed`];
  if (v.deleted) parts.push(`${plural(v.deleted, "file")} deleted`);
  return `${parts.join(", ")} (${bytes(v.size)})`;
}

async function showVersions(entry) {
  let vs;
  try { vs = await api(`/api/versions?path=${encodeURIComponent(entry.path)}`); } catch (err) { toast(err.message); return; }
  const body = el("ul", { class: "list" }, vs.map((v, i) => el("li", {},
    el("div", { class: "grow" },
      el("div", { class: "name" }, i === 0 ? "Latest" : `Older version ${i}`),
      el("div", { class: "sub" }, `${versionWhat(v)} · backed up ${when(v.uploadedAt)}${v.parts ? ` · ${v.parts} parts` : ""}`)),
    entry.kind === "file" ? el("button", { type: "button", class: "small", onclick: () => {
      $("dlg").close();
      openViewer({ ...entry, size: v.size, mtime: v.mtime, parts: v.parts }, [entry], i === 0 ? 0 : v.msgId);
    } }, "View") : null,
    onPhone ? null : el("a", { class: "btn small", href: `/api/download?msg=${v.msgId}` }, "Download"))));
  await dialog({ title: entry.name, body, buttons: [{ label: "Close", value: null }] });
}

// ---------- previews ----------

// What the viewer can show, by file extension. Everything else gets an
// information card with Download (and Open with… on the phone).
const viewExts = {
  image: ["jpg", "jpeg", "png", "gif", "webp", "bmp", "ico", "avif", "svg"],
  video: ["mp4", "m4v", "mov", "webm", "mkv", "3gp", "ogv"],
  audio: ["mp3", "m4a", "aac", "ogg", "oga", "opus", "wav", "flac", "weba"],
  pdf: ["pdf"],
  text: ["txt", "md", "log", "csv", "tsv", "json", "xml", "html", "htm", "css", "js", "mjs", "ts", "tsx", "jsx",
    "go", "py", "java", "kt", "c", "h", "cpp", "cs", "rb", "php", "rs", "sh", "bat", "ps1", "yml", "yaml",
    "toml", "ini", "cfg", "conf", "sql", "srt", "vtt", "env", "gitignore"],
};
const textLimit = 512 * 1024;

function viewKind(name) {
  const i = name.lastIndexOf(".");
  const ext = (i >= 0 ? name.slice(i + 1) : name).toLowerCase();
  for (const [kind, exts] of Object.entries(viewExts)) if (exts.includes(ext)) return kind;
  return "other";
}

// The open viewer: the files it steps through (in the order shown), which
// one is showing, and for an older version its message ID.
const viewer = { list: [], i: 0, msgId: 0 };

// openViewer shows a file; ← → (or a swipe) move through list.
function openViewer(entry, list = cloud.order.filter((x) => x.kind === "file"), msgId = 0) {
  viewer.list = list.length ? list : [entry];
  viewer.i = Math.max(0, viewer.list.findIndex((x) => x.path === entry.path));
  viewer.msgId = msgId;
  closeMenu();
  renderViewer();
}

function closeViewer() {
  const v = $("viewer");
  if (!v) return false;
  for (const m of v.querySelectorAll("video, audio")) { m.pause(); m.removeAttribute("src"); m.load(); }
  v.remove();
  document.body.classList.remove("viewing");
  return true;
}

function stepViewer(d) {
  const i = viewer.i + d;
  if (viewer.msgId || i < 0 || i >= viewer.list.length) return;
  viewer.i = i;
  viewer.msgId = 0;
  renderViewer();
}

function renderViewer() {
  const keepFocus = $("viewer") && $("viewer").contains(document.activeElement);
  closeViewer();
  const e = viewer.list[viewer.i];
  const msgId = viewer.msgId || e.msgId;
  const src = `/api/preview?msg=${msgId}`;
  const kind = viewKind(e.name);
  const info = `${bytes(e.size)} · modified ${when(e.mtime)}${viewer.msgId ? " · older version" : ""}${e.parts ? ` · stored in ${e.parts} parts` : ""}`;
  const canStep = !viewer.msgId && viewer.list.length > 1;
  const download = onPhone
    ? el("button", { type: "button", class: "small", onclick: () => bulkDownload([e]) }, "Download")
    : el("a", { class: "btn small", href: `/api/download?msg=${msgId}` }, "Download");
  const openWith = onPhone ? el("button", { type: "button", class: "small", onclick: () => openWithApp(e) }, "Open with…") : null;
  const body = el("div", { class: "vbody" }, viewContent(e, kind, src, msgId));
  const box = el("div", { class: "viewer", id: "viewer", role: "dialog", "aria-modal": "true", "aria-label": `Preview of ${e.name}`, tabindex: "-1" },
    el("div", { class: "vbar" },
      el("div", { class: "vtitle" }, el("div", { class: "name" }, e.name),
        el("div", { class: "sub" }, info, canStep ? ` · ${viewer.i + 1} of ${viewer.list.length}` : "")),
      el("div", { class: "vactions" }, download, openWith,
        el("button", { type: "button", class: "small", "aria-label": "Close preview", onclick: closeViewer }, "Close"))),
    body,
    canStep ? el("button", { type: "button", class: "vnav prev", "aria-label": "Previous file", disabled: viewer.i === 0, onclick: () => stepViewer(-1) }, "‹") : null,
    canStep ? el("button", { type: "button", class: "vnav next", "aria-label": "Next file", disabled: viewer.i === viewer.list.length - 1, onclick: () => stepViewer(1) }, "›") : null);
  swipeToStep(body);
  document.body.append(box);
  document.body.classList.add("viewing");
  if (!keepFocus || !box.contains(document.activeElement)) box.focus();
}

function viewContent(e, kind, src, msgId) {
  const card = (why) => el("div", { class: "vcard" },
    el("div", { class: "vicon" }, typeIcons[e.type] || "📄"),
    el("p", {}, why),
    el("p", { class: "hint" }, onPhone ? "Download saves it to Download/tgvault; Open with… lets another app show it." : "Download saves it to this PC."));
  if (kind === "image") {
    const img = el("img", { src, alt: e.name, class: "fit" });
    img.addEventListener("click", () => img.classList.toggle("fit"));
    img.addEventListener("error", () => img.replaceWith(card("This picture can't be shown here (it may be in a format the browser doesn't read).")));
    return img;
  }
  if (kind === "video" || kind === "audio") {
    // On the phone nothing is fetched until you press play.
    const media = el(kind, { src, controls: true, preload: onPhone ? "none" : "metadata", playsinline: true });
    media.addEventListener("error", () => media.replaceWith(card(`This ${kind === "video" ? "video" : "recording"} can't play here: its format isn't supported by ${onPhone ? "this phone" : "this browser"}.`)));
    if (kind === "video") {
      // Some videos (often x265/HEVC) play their sound but no picture here.
      media.addEventListener("loadedmetadata", () => {
        if (!media.videoWidth && media.parentNode) {
          const wrap = el("div", { class: "vvideo" }, el("p", { class: "vwarn" },
            `This video's picture can't be shown here (its format isn't supported by ${onPhone ? "this phone" : "this browser"}); the sound may still play. ${onPhone ? "Open with… lets a video app play it." : "Download it to watch it in a video player."}`));
          media.replaceWith(wrap);
          wrap.append(media);
        }
      });
    }
    return kind === "audio" ? el("div", { class: "vaudio" }, el("div", { class: "vicon" }, "🎵"), media) : media;
  }
  if (kind === "pdf") {
    if (onPhone) return card("PDFs open in a PDF app on the phone.");
    return el("div", { class: "vcard" }, el("div", { class: "vicon" }, "📄"),
      el("p", {}, "PDFs open in the browser's own PDF viewer."),
      el("a", { class: "btn primary", href: src, target: "_blank", rel: "noopener" }, "Open the PDF in a new tab"));
  }
  if (kind === "text") {
    const pre = el("pre", { class: "vtext" }, "Loading…");
    loadText(pre, src, e.size);
    return pre;
  }
  return card(`${e.name} can't be previewed here.`);
}

async function loadText(pre, src, size) {
  try {
    const r = await fetch(src, { headers: { Range: `bytes=0-${textLimit - 1}` } });
    if (!r.ok && r.status !== 206) throw new Error((await r.json().catch(() => ({}))).error || `error ${r.status}`);
    const text = new TextDecoder("utf-8").decode(await r.arrayBuffer());
    pre.textContent = text;
    if (size > textLimit) pre.append(el("div", { class: "vmore" }, `Showing the first ${bytes(textLimit)} of ${bytes(size)}. Download it to see the rest.`));
  } catch (err) {
    pre.textContent = `Couldn't load it: ${err.message}`;
  }
}

// Phone: hand a file to another app (a PDF reader, an office app…).
async function openWithApp(e) {
  try {
    await startBulk("open", [e]);
    toast(`Getting ${e.name} ready; your apps will open in a moment.`);
  } catch (err) { toast(err.message); }
}

// A horizontal swipe on the viewer moves to the previous or next file.
function swipeToStep(node) {
  let x0 = null, y0 = 0;
  node.addEventListener("touchstart", (ev) => {
    if (ev.touches.length !== 1 || ev.target.closest("video, audio, img:not(.fit)")) { x0 = null; return; }
    x0 = ev.touches[0].clientX; y0 = ev.touches[0].clientY;
  }, { passive: true });
  node.addEventListener("touchend", (ev) => {
    if (x0 === null) return;
    const t = ev.changedTouches[0];
    const dx = t.clientX - x0, dy = t.clientY - y0;
    x0 = null;
    if (Math.abs(dx) > 60 && Math.abs(dy) < 50) stepViewer(dx < 0 ? 1 : -1);
  }, { passive: true });
}

function viewerKeys(ev) {
  if (!$("viewer") || $("dlg").open) return false;
  if (ev.key === "Escape") { closeViewer(); return true; }
  if (ev.target.closest && ev.target.closest("video, audio")) return false; // their own seeking keys
  if (ev.key === "ArrowLeft") { stepViewer(-1); return true; }
  if (ev.key === "ArrowRight") { stepViewer(1); return true; }
  return false;
}

// The phone's back button closes a preview or dialog before leaving the page.
window.tgvaultBack = () => {
  if (closeViewer()) return true;
  if ($("dlg").open) { $("dlg").close(); return true; }
  return closeMenu();
};

// Reload the list when another device's files arrive (or anything changes),
// without disturbing the selection.
function maybeReloadCloud() {
  if (tab !== "cloud" || !state || state.cloudStamp === cloud.stamp) return;
  if (Date.now() - cloud.loadedAt < 10000) return;
  loadCloud();
}

// Pull-to-refresh on the phone and the Refresh button: ask Telegram for new
// files from other devices, then reload.
async function refreshCloud(loud) {
  if (state && state.telegram.status === "ready") {
    try {
      const r = await api("/api/sync", {});
      if (r.added) toast(`${r.added} new file${r.added > 1 ? "s" : ""} from your other device.`);
    } catch (err) { if (loud === true) toast(err.message); }
  }
  await refresh();
  if (tab === "cloud") await loadCloud();
  if (tab === "space") await loadSpace();
}
window.tgvaultRefresh = refreshCloud;

// ---------- free up space ----------

const space = {
  items: [], count: 0, bytes: 0, limit: 200,
  selected: new Map(), // cloud path -> recommendation
  order: [], anchor: null, // as in cloud
  list: "recList",
  key: (r) => r.cloudPath,
  render: () => renderSpaceSelection(),
};

async function loadSpace(all = false) {
  const qs = new URLSearchParams({ sort: $("recSort").value, type: $("recType").value, min: $("recMin").value, limit: all ? "0" : String(space.limit) });
  let data;
  try { data = await api(`/api/recommendations?${qs}`); } catch (err) { toast(err.message); return; }
  space.items = data.items;
  space.count = data.count;
  space.bytes = data.bytes;
  const known = new Set(space.items.map((r) => r.cloudPath));
  for (const p of space.selected.keys()) if (!known.has(p) && !all) space.selected.delete(p);
  renderSpace();
}

function renderSpace() {
  $("spaceSummary").textContent = space.count
    ? `${space.count.toLocaleString()} file${space.count > 1 ? "s" : ""} (${bytes(space.bytes)}) on ${here} are safely backed up and could be deleted here.`
    : `Nothing to suggest right now: no file on ${here} matching these filters has an up-to-date backup yet.`;
  space.order = space.items;
  replaceRows(space, space.items.map((r) => {
    const name = r.cloudPath.slice(r.cloudPath.lastIndexOf("/") + 1);
    const folder = r.cloudPath.slice(0, r.cloudPath.lastIndexOf("/"));
    return el("li", pickRow(space, r),
      pickBox(space, r, name),
      el("span", { class: "icon" }, typeIcons[r.type] || "📄"),
      el("div", { class: "grow" },
        el("div", { class: "name" }, name),
        el("div", { class: "sub" }, `${bytes(r.size)} · modified ${when(r.mtime)} · in ${folder}`)));
  }));
  $("recMore").hidden = space.items.length >= space.count;
  $("recMore").textContent = `Show more (${(space.count - space.items.length).toLocaleString()} left)`;
  renderSpaceSelection();
}

function renderSpaceSelection() {
  renderRows(space);
  const sel = [...space.selected.values()];
  const total = sel.reduce((s, r) => s + r.size, 0);
  $("recAll").checked = space.count > 0 && sel.length === space.count;
  $("recAll").indeterminate = sel.length > 0 && sel.length < space.count;
  $("recCount").textContent = sel.length ? `${sel.length.toLocaleString()} selected · ${bytes(total)} would be freed` : "Select all";
  $("recDelete").hidden = !sel.length;
  $("recDelete").textContent = `Delete ${bytes(total)} from ${here}`;
}

async function selectAllSpace(on) {
  if (!on) { space.selected.clear(); renderSpace(); return; }
  if (space.items.length < space.count) await loadSpace(true); // "all" means every match, not just the loaded ones
  for (const r of space.items) space.selected.set(r.cloudPath, r);
  renderSpace();
}

async function deleteRecommended() {
  const sel = [...space.selected.values()];
  const total = sel.reduce((s, r) => s + r.size, 0);
  await dialog({
    title: `Delete ${sel.length.toLocaleString()} file${sel.length > 1 ? "s" : ""} from ${here}?`,
    body: el("div", {},
      el("p", {}, `This frees ${bytes(total)} on ${here}.`),
      el("p", {}, el("b", {}, "Only the copy on " + here + " is deleted."), " The backup in Telegram stays intact, and you can restore any of these files from Cloud files whenever you like."),
      el("p", { class: "hint" }, "Before deleting, tgvault checks each file still matches its backup and that the backup is still in Telegram. Anything that doesn't is kept.")),
    buttons: [{ label: "Cancel", value: null }, { label: `Delete ${bytes(total)} from ${here}`, value: "go", danger: true }],
    run: async () => {
      await api("/api/bulk", { action: "free", paths: sel.map((r) => r.cloudPath) });
      toast("Deleting; progress is at the top of the page.");
      space.selected.clear();
      refresh();
      setTimeout(loadSpace, 3000);
    },
  });
}

// ---------- activity ----------

const stateLabels = {
  uploading: ["Uploading", "info"], pending: ["Waiting", "info"],
  failed: ["Problem", "bad"], skipped: ["Skipped", "warn"],
};

const activityGroups = [
  ["uploading", "Uploading now", "Files being sent right now."],
  ["failed", "Needs attention", "These failed for a reason other than Telegram's speed limit. tgvault keeps retrying them."],
  ["pending", "Queued", "Waiting their turn. Files upload a few at a time to stay within Telegram's speed limit."],
  ["skipped", "Not backed up", "tgvault won't upload these (for example paths too long for Telegram)."],
];

async function loadActivity() {
  let data;
  try { data = await api("/api/activity"); } catch (err) { toast(err.message); return; }
  const counts = data.counts || {};
  const cool = state && state.engine.cooldownUntil;
  let any = false;
  const sections = activityGroups.map(([key, title, hint]) => {
    const items = data[key] || [];
    const total = counts[key] || items.length;
    if (!total) return null;
    any = true;
    let extra = hint;
    if (key === "pending" && cool) {
      extra = `Paused for Telegram's speed limit until ${new Date(cool * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}; this is normal.`;
    }
    return el("section", { class: "group" },
      el("div", { class: "row-head" },
        el("h2", {}, `${title} `, el("span", { class: "count" }, total.toLocaleString())),
        key === "failed" ? el("button", { class: "small", onclick: () => api("/api/retry-all", {}).then(() => { toast("Retrying now."); loadActivity(); }) }, "Retry all now") : null),
      el("p", { class: "hint" }, extra),
      el("ul", { class: "list" }, items.map((it) => activityRow(it, key))),
      total > items.length ? el("p", { class: "hint" }, `…and ${(total - items.length).toLocaleString()} more.`) : null);
  });
  $("activityEmpty").hidden = any;
  replace($("activity"), sections);
}

function activityRow(it, key) {
  let note = it.note || "";
  if (key === "failed" && it.nextAttempt) {
    note += ` (next try ${new Date(it.nextAttempt * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })})`;
  }
  const up = key === "uploading" && state && (state.engine.uploading || []).find((u) => u.itemId === it.id);
  if (up && up.total) note = `${bytes(up.done)} of ${bytes(up.total)}`;
  return el("li", {},
    el("span", { class: "icon" }, it.kind === "project" ? "📦" : "📄"),
    el("div", { class: "grow" },
      el("div", { class: "name" }, it.cloudPath),
      el("div", { class: "sub" }, bytes(it.size), note ? ` · ${note}` : "")),
    key === "failed" || key === "skipped"
      ? el("button", { class: "small", onclick: () => api(`/api/items/${it.id}/retry`, {}).then(() => { toast("Will try again shortly."); loadActivity(); }) }, "Retry")
      : null);
}

// ---------- settings ----------

function renderSettings() {
  $("autostart").checked = !!state.autostart;
  $("notify").checked = !!state.notify;
  const tg = state.telegram;
  $("accountText").textContent = `Logged in as ${tg.account || "?"}. Files are stored in your private channel “${tg.channel}”.`;
  const rc = state.engine.reconcile;
  $("reconcileText").textContent = rc.running ? "Checking…"
    : rc.error ? `Last check failed: ${rc.error}`
    : rc.last ? `Last checked ${when(rc.last)}: ${rc.files} backed-up items found in Telegram.`
    : "Re-reads the backup channel to find files added or deleted there.";
  $("versionText").textContent = `tgvault ${state.version}`;
  if (state.phone) {
    $("wifiOnly").checked = state.phone.wifiOnly;
    $("chargingOnly").checked = state.phone.chargingOnly;
    const p = state.phone.permissions;
    const yes = (b) => (b ? "allowed" : "not allowed");
    $("permText").textContent = `Files: ${yes(p.storage)}. Background backups: ${yes(p.battery)}. Notifications: ${yes(p.notifications)}.`;
  }
}

// ---------- wiring ----------

function showTab(name) {
  tab = name;
  for (const b of document.querySelectorAll(".tabs button")) b.classList.toggle("active", b.dataset.tab === name);
  for (const p of document.querySelectorAll("[data-panel]")) p.hidden = p.dataset.panel !== name;
  if (name === "cloud") loadCloud();
  if (name === "activity") loadActivity();
  if (name === "space") loadSpace();
}

document.addEventListener("DOMContentLoaded", () => {
  loginForm("form-api", "/api/setup", false);
  loginForm("form-phone", "/api/login/phone", true);
  loginForm("form-code", "/api/login/code", true);
  loginForm("form-password", "/api/login/password", true);
  $("changePhone").addEventListener("click", () => loginAction("/api/login/qr/cancel"));
  $("startQR").addEventListener("click", () => loginAction("/api/login/qr"));
  $("codeToQR").addEventListener("click", () => loginAction("/api/login/qr"));
  $("cancelQR").addEventListener("click", () => loginAction("/api/login/qr/cancel"));
  $("resendCode").addEventListener("click", () => loginAction("/api/login/resend", "Asked Telegram to send the code again."));

  for (const b of document.querySelectorAll(".tabs button")) {
    b.addEventListener("click", () => showTab(b.dataset.tab));
    if (onPhone && b.dataset.short) b.firstChild.textContent = b.dataset.short;
  }
  // File browser: search, filters, sort and selection.
  let searchTimer;
  $("cloudSearch").addEventListener("input", () => { clearTimeout(searchTimer); searchTimer = setTimeout(loadCloud, 300); });
  for (const id of ["cloudType", "cloudFrom", "cloudTo"]) $(id).addEventListener("change", loadCloud);
  $("cloudSort").addEventListener("change", (ev) => { cloud.sort = ev.target.value; renderCloud(); });
  $("cloudDir").addEventListener("click", () => { cloud.desc = !cloud.desc; renderCloud(); });
  $("cloudDates").addEventListener("click", () => { $("cloudDateRow").hidden = !$("cloudDateRow").hidden; });
  $("cloudClear").addEventListener("click", () => {
    for (const id of ["cloudSearch", "cloudType", "cloudFrom", "cloudTo"]) $(id).value = "";
    loadCloud();
  });
  $("cloudRefresh").addEventListener("click", () => refreshCloud(true));
  $("selAll").addEventListener("change", (ev) => selectAll(ev.target.checked));
  for (const p of [cloud, space]) {
    $(p.list).addEventListener("keydown", (ev) => listKeys(p, ev));
    longPress(p);
  }
  document.addEventListener("click", (ev) => { if (!ev.target.closest("#ctxmenu")) closeMenu(); });
  addEventListener("scroll", closeMenu, true);
  addEventListener("blur", closeMenu);
  document.addEventListener("keydown", (ev) => {
    if (viewerKeys(ev)) { ev.preventDefault(); return; }
    if ($("viewer")) return; // the rows behind the preview don't react
    if (ev.key !== "Escape" || $("dlg").open) return;
    if (closeMenu()) return;
    if (ev.target.matches && ev.target.matches("input:not([type=checkbox]), select, textarea")) return; // e.g. clearing the search box
    const p = [cloud, space].find((p) => !$(p.list).closest("[data-panel]").hidden);
    if (p && p.selected.size) clearSelection(p);
  });
  // Free up space.
  for (const id of ["recSort", "recType", "recMin"]) $(id).addEventListener("change", () => { space.selected.clear(); loadSpace(); });
  $("recMore").addEventListener("click", () => { space.limit += 500; loadSpace(); });
  $("recAll").addEventListener("change", (ev) => selectAllSpace(ev.target.checked));
  $("recDelete").addEventListener("click", deleteRecommended);
  $("addFolder").addEventListener("click", addFolder);
  $("suggestGo").addEventListener("click", addSuggested);
  for (const id of ["wifiOnly", "chargingOnly"]) {
    $(id).addEventListener("change", (ev) => api("/api/phone-settings", { [id]: ev.target.checked })
      .then(() => { toast("Saved."); refresh(); })
      .catch((err) => { toast(err.message); refresh(); }));
  }
  $("autostart").addEventListener("change", (ev) => api("/api/autostart", { on: ev.target.checked })
    .then(() => toast(ev.target.checked ? "tgvault will start when you sign in." : "Start-up turned off."))
    .catch((err) => { toast(err.message); refresh(); }));
  $("notify").addEventListener("change", (ev) => api("/api/notify", { on: ev.target.checked })
    .then(() => toast(ev.target.checked ? "Notifications on." : "Notifications off."))
    .catch((err) => { toast(err.message); refresh(); }));
  $("pairPhone").addEventListener("click", () => {
    const addr = el("input", { autocomplete: "off", spellcheck: "false", placeholder: "192.168.1.23:40123" });
    const code = el("input", { autocomplete: "off", inputmode: "numeric", placeholder: "123456" });
    dialog({
      title: "Log in a phone",
      body: el("div", {},
        el("p", {}, "Open tgvault on the phone. Enter the address and code it shows:"),
        el("label", {}, "Address", addr),
        el("label", {}, "Code", code),
        el("p", { class: "hint" }, "This can take up to a minute while the phone connects to Telegram.")),
      buttons: [{ label: "Cancel", value: null }, { label: "Log in the phone", value: "go", primary: true }],
      run: async () => {
        await api("/api/pair-phone", { address: addr.value, code: code.value });
        toast("The phone is logged in.");
      },
    });
    addr.focus();
  });
  $("testNotify").addEventListener("click", () => api("/api/notify/test", {})
    .then(() => toast(onPhone ? "Sent. Check your notifications." : "Sent. Look at the bottom-right of your screen."))
    .catch((err) => toast(err.message)));
  $("reconcile").addEventListener("click", () => api("/api/reconcile", {}).then(() => { toast("Checking Telegram…"); refresh(); }));
  $("logout").addEventListener("click", () => dialog({
    title: "Log out of Telegram?",
    body: el("p", {}, "Backups stop until you log in again. Your files in Telegram are not touched."),
    buttons: [{ label: "Cancel", value: null }, { label: "Log out", value: "out", danger: true }],
    run: async () => { await api("/api/logout", {}); refresh(); },
  }));
  $("quit").addEventListener("click", () => dialog({
    title: "Quit tgvault?",
    body: el("p", {}, "Backups pause until you run tgvault again."),
    buttons: [{ label: "Cancel", value: null }, { label: "Quit", value: "quit", danger: true }],
    run: async () => {
      await api("/api/quit", {});
      document.body.replaceChildren(el("main", {}, el("div", { class: "card narrow" }, el("h1", {}, "tgvault has stopped"), el("p", {}, "Run tgvault.exe to start it again."))));
    },
  }));

  refresh();
  setInterval(() => {
    if (document.hidden) return;
    refresh();
    if (tab === "activity") loadActivity();
  }, 2000);
});
