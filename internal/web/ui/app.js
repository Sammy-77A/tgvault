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

async function refresh() {
  try {
    state = await api("/api/state");
  } catch {
    $("connText").textContent = "tgvault is not running";
    $("conn").className = "conn warn";
    return;
  }
  render();
}

function render() {
  const tg = state.telegram;
  const ready = tg.status === "ready";
  $("login").hidden = ready;
  $("main").hidden = !ready;

  const conn = $("conn");
  if (ready && tg.online) {
    conn.className = "conn ok";
    $("connText").textContent = "Connected";
  } else if (ready) {
    conn.className = "conn warn";
    $("connText").textContent = "Reconnecting…";
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

  const url = tg.qrUrl || "";
  if (url !== lastQR) {
    lastQR = url;
    $("qrImg").hidden = !url;
    $("qrWait").hidden = !!url;
    if (url) $("qrImg").src = `/api/login/qr.png?v=${Date.now()}`;
    // On a phone the QR code can't be scanned; the same tg://login link can be opened instead.
    $("qrPhone").hidden = !(url && onPhone);
    if (url) $("qrOpen").href = url;
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
  if (failed && state.roots && state.roots.length) {
    sub += ` ${n(failed)} item${failed === 1 ? " needs" : "s need"} attention — see Activity.`;
  }

  const ups = uploading.sort((a, b) => a.cloudPath.localeCompare(b.cloudPath)).map((u) => {
    const pct = u.total ? Math.round((u.done / u.total) * 100) : 0;
    const label = u.phase === "uploading" ? `${bytes(u.done)} of ${bytes(u.total)}` : u.phase;
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

async function addFolder() {
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
  same: ["On this PC", "ok"],
  changed: ["Changed on PC", "info"],
  "cloud-only": ["Cloud only", "warn"],
  mixed: ["Partly on PC", "info"],
};

async function loadCloud() {
  let data;
  try {
    data = await api(`/api/cloud?path=${encodeURIComponent(cloudPath)}`);
  } catch (err) { toast(err.message); return; }

  const parts = cloudPath ? cloudPath.split("/") : [];
  const crumbs = [el("button", { onclick: () => openCloud("") }, "All backups")];
  parts.forEach((p, i) => {
    crumbs.push(el("span", {}, "/"));
    const target = parts.slice(0, i + 1).join("/");
    crumbs.push(el("button", { onclick: () => openCloud(target) }, p));
  });
  replace($("crumbs"), crumbs);
  replace($("folderActions"), cloudPath ? [
    el("button", { class: "small", onclick: () => restore(cloudPath, true) }, "Restore folder"),
    el("a", { class: "btn small", href: `/api/download-zip?path=${encodeURIComponent(cloudPath)}` }, "Download ZIP"),
    el("button", { class: "small", onclick: () => freeSpace(cloudPath) }, "Free up space"),
    el("button", { class: "small danger", onclick: () => removeFromBackup(cloudPath) }, "Remove from backup"),
  ] : []);

  $("cloudEmpty").hidden = data.entries.length > 0;
  replace($("cloudList"), data.entries.map((e) => {
    const [label, cls] = localLabels[e.local] || ["", ""];
    const isFolder = e.kind === "folder";
    const icon = isFolder ? "📁" : e.kind === "project" ? "📦" : "📄";
    const sub = isFolder
      ? `${e.files} item${e.files > 1 ? "s" : ""} · ${bytes(e.size)}`
      : `${bytes(e.size)} · backed up ${when(e.uploaded)}${e.versions > 1 ? ` · ${e.versions} versions` : ""}${e.kind === "project" ? " · project ZIP" : ""}`;
    const actions = isFolder ? [] : [
      el("a", { class: "btn small", href: `/api/download?path=${encodeURIComponent(e.path)}`, onclick: (ev) => ev.stopPropagation() }, "Download"),
      el("button", { class: "small", onclick: (ev) => { ev.stopPropagation(); restore(e.path, false); } }, "Restore"),
      e.versions > 1 ? el("button", { class: "small", onclick: (ev) => { ev.stopPropagation(); showVersions(e); } }, "Versions") : null,
      e.local === "same" ? el("button", { class: "small", onclick: (ev) => { ev.stopPropagation(); freeSpace(e.path); } }, "Free up space") : null,
      e.local === "cloud-only" ? el("button", { class: "small danger", onclick: (ev) => { ev.stopPropagation(); removeFromBackup(e.path); } }, "Remove from backup") : null,
    ];
    return el("li", { class: isFolder ? "clickable" : "", onclick: isFolder ? () => openCloud(e.path) : null },
      el("span", { class: "icon" }, icon),
      el("div", { class: "grow" },
        el("div", { class: "name" }, e.name),
        el("div", { class: "sub" }, sub, " ", label ? el("span", { class: `pill ${cls}` }, label) : null)),
      el("div", { class: "actions" }, actions));
  }));
}

function openCloud(p) {
  cloudPath = p;
  loadCloud();
}

async function restore(path, isFolder) {
  const orig = el("input", { type: "radio", name: "dest", value: "orig", checked: true });
  const other = el("input", { type: "radio", name: "dest", value: "other" });
  const destInput = el("input", { type: "text", placeholder: "C:\\Users\\you\\Restored", autocomplete: "off" });
  const body = el("div", {},
    el("p", {}, `Restore ${path}${isFolder ? " with all its subfolders" : ""}.`),
    el("label", { class: "choice" }, orig, el("div", {}, el("b", {}, "Original location"),
      el("div", { class: "hint" }, "Files already there are never overwritten; a different existing file keeps its name and the restored copy gets “(restored)” added."))),
    el("label", { class: "choice" }, other, el("div", { class: "grow" }, el("b", {}, "Another folder"), destInput,
      el("button", { type: "button", class: "small", onclick: async () => {
        try {
          const r = await api("/api/pick-folder", {});
          if (r.path) { destInput.value = r.path; other.checked = true; }
        } catch (err) { toast(err.message); }
      } }, "Browse…"))));
  destInput.addEventListener("input", () => { other.checked = true; });
  await dialog({
    title: "Restore", body,
    buttons: [{ label: "Cancel", value: null }, { label: "Restore", value: "go", primary: true }],
    run: async () => {
      await api("/api/restore", { path, dest: other.checked ? destInput.value : "" });
      toast("Restoring; progress is on the Overview tab.");
      refresh();
    },
  });
}

async function freeSpace(path) {
  let plan;
  try {
    plan = await api(`/api/free-plan?path=${encodeURIComponent(path)}`);
  } catch (err) { toast(err.message); return; }
  const n = plan.candidates.length;
  const body = el("div", {},
    n ? el("p", {}, `Delete ${n} item${n > 1 ? "s" : ""} (${bytes(plan.bytes)}) from this PC. Each one is checked against its Telegram copy first, and you can restore it any time from Cloud files.`)
      : el("p", {}, "Nothing here can be removed yet, because nothing is fully backed up."),
    plan.candidates.some((c) => c.kind === "project")
      ? el("p", { class: "hint" }, "Project folders are deleted whole, including rebuildable folders like node_modules that were not backed up.") : null,
    plan.kept && plan.kept.length ? el("div", {}, el("p", { class: "hint" }, "Kept on this PC:"),
      el("ul", { class: "scroll" }, plan.kept.slice(0, 200).map((k) => el("li", {}, k)))) : null);
  await dialog({
    title: "Free up space", body,
    buttons: n ? [{ label: "Cancel", value: null }, { label: `Delete ${bytes(plan.bytes)} from this PC`, value: "go", danger: true }]
      : [{ label: "Close", value: null }],
    run: async () => {
      await api("/api/free", { path });
      toast("Freeing space; progress is on the Overview tab.");
      refresh();
    },
  });
}

async function removeFromBackup(path) {
  await dialog({
    title: "Remove from backup?",
    body: el("div", {},
      el("p", {}, `Permanently delete ${path} and all its versions from your Telegram backup channel.`),
      el("p", { class: "hint" }, "This can't be undone. Files on this PC are not touched. It only works for things no longer being backed up from this PC.")),
    buttons: [{ label: "Cancel", value: null }, { label: "Delete from Telegram", value: "go", danger: true }],
    run: async () => {
      await api("/api/delete-cloud", { path });
      toast("Removing; progress is on the Overview tab.");
      if (cloudPath === path) cloudPath = path.split("/").slice(0, -1).join("/");
      refresh();
      loadCloud();
    },
  });
}

async function showVersions(entry) {
  let vs;
  try { vs = await api(`/api/versions?path=${encodeURIComponent(entry.path)}`); } catch (err) { toast(err.message); return; }
  const body = el("ul", { class: "list" }, vs.map((v, i) => el("li", {},
    el("div", { class: "grow" },
      el("div", { class: "name" }, i === 0 ? "Latest" : `Older version ${i}`),
      el("div", { class: "sub" }, `${bytes(v.size)} · backed up ${when(v.uploadedAt)}`)),
    el("a", { class: "btn small", href: `/api/download?msg=${v.msgId}` }, "Download"))));
  await dialog({ title: entry.name, body, buttons: [{ label: "Close", value: null }] });
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
  ["skipped", "Not backed up", "tgvault won't upload these (for example files over 2 GB)."],
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
}

// ---------- wiring ----------

function showTab(name) {
  tab = name;
  for (const b of document.querySelectorAll(".tabs button")) b.classList.toggle("active", b.dataset.tab === name);
  for (const p of document.querySelectorAll("[data-panel]")) p.hidden = p.dataset.panel !== name;
  if (name === "cloud") loadCloud();
  if (name === "activity") loadActivity();
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

  for (const b of document.querySelectorAll(".tabs button")) b.addEventListener("click", () => showTab(b.dataset.tab));
  $("addFolder").addEventListener("click", addFolder);
  $("autostart").addEventListener("change", (ev) => api("/api/autostart", { on: ev.target.checked })
    .then(() => toast(ev.target.checked ? "tgvault will start when you sign in." : "Start-up turned off."))
    .catch((err) => { toast(err.message); refresh(); }));
  $("notify").addEventListener("change", (ev) => api("/api/notify", { on: ev.target.checked })
    .then(() => toast(ev.target.checked ? "Notifications on." : "Notifications off."))
    .catch((err) => { toast(err.message); refresh(); }));
  $("testNotify").addEventListener("click", () => api("/api/notify/test", {})
    .then(() => toast("Sent. Look at the bottom-right of your screen."))
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
