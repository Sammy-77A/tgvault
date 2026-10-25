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
        el("input", { type: "checkbox", value: f.path, checked: f.checked }),
        el("div", {}, el("div", { class: "name" }, f.label), f.hint ? el("div", { class: "sub" }, f.hint) : null)))));
  }
  card.hidden = !suggestions.length;
}

async function addSuggested() {
  const picked = [...$("suggestList").querySelectorAll("input:checked")].map((i) => i.value);
  if (!picked.length) { toast("Tick at least one folder, or use Add folder below."); return; }
  // Add parents first, so a folder inside another selected one is reported, not duplicated.
  picked.sort((a, b) => a.length - b.length);
  const problems = [];
  for (const p of picked) {
    try { await api("/api/roots", { path: p }); } catch (err) { problems.push(`${p.split("/").pop()}: ${err.message}`); }
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
async function pickFolderOnPhone() {
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
    title: "Choose a folder to back up",
    body: el("div", {}, vols, where, list),
    buttons: [{ label: "Cancel", value: null }, { label: "Back up this folder", value: "pick", primary: true }],
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

// On the phone, "download" saves into the phone's Download/tgvault folder.
async function saveToPhone(body) {
  toast("Saving…");
  try {
    const r = await api("/api/save", body);
    toast(`Saved to ${r.saved.replace(/^.*\/Download\//, "Download/")}`);
  } catch (err) { toast(err.message); }
}

function downloadButton(label, href, saveBody) {
  if (onPhone) return el("button", { class: "small", onclick: (ev) => { ev.stopPropagation(); saveToPhone(saveBody); } }, label);
  return el("a", { class: "btn small", href, onclick: (ev) => ev.stopPropagation() }, label);
}

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
    el("button", { class: "small", onclick: () => restore(cloudPath, true, otherDevice(cloudPath)) }, "Restore folder"),
    downloadButton("Download ZIP", `/api/download-zip?path=${encodeURIComponent(cloudPath)}`, { path: cloudPath, zip: true }),
    el("button", { class: "small", onclick: () => freeSpace(cloudPath) }, "Free up space"),
    el("button", { class: "small danger", onclick: () => removeFromBackup(cloudPath, otherDevice(cloudPath) ? deviceOf(cloudPath) : "") }, "Remove from backup"),
  ] : []);

  $("cloudEmpty").hidden = data.entries.length > 0;
  replace($("cloudList"), data.entries.map((e) => {
    const [label, cls] = localLabel(e);
    const isFolder = e.kind === "folder";
    const icon = isFolder ? "📁" : e.kind === "project" ? "📦" : "📄";
    const sub = isFolder
      ? `${e.files} item${e.files > 1 ? "s" : ""} · ${bytes(e.size)}`
      : `${bytes(e.size)} · backed up ${when(e.uploaded)}${e.versions > 1 ? ` · ${e.versions} versions` : ""}${e.kind === "project" ? " · project ZIP" : ""}`;
    const actions = isFolder ? [] : [
      downloadButton("Download", `/api/download?path=${encodeURIComponent(e.path)}`, { path: e.path }),
      el("button", { class: "small", onclick: (ev) => { ev.stopPropagation(); restore(e.path, false, e.local === "other-device"); } }, "Restore"),
      e.versions > 1 ? el("button", { class: "small", onclick: (ev) => { ev.stopPropagation(); showVersions(e); } }, "Versions") : null,
      e.local === "same" ? el("button", { class: "small", onclick: (ev) => { ev.stopPropagation(); freeSpace(e.path); } }, "Free up space") : null,
      e.local === "cloud-only" || e.local === "other-device"
        ? el("button", { class: "small danger", onclick: (ev) => { ev.stopPropagation(); removeFromBackup(e.path, e.local === "other-device" ? e.device : ""); } }, "Remove from backup") : null,
    ];
    return el("li", { class: isFolder ? "clickable" : "", onclick: isFolder ? () => openCloud(e.path) : null },
      el("span", { class: "icon" }, icon),
      el("div", { class: "grow" },
        el("div", { class: "name" }, e.name),
        el("div", { class: "sub" }, sub, " ", label ? el("span", { class: `pill ${cls}` }, label) : null)),
      el("div", { class: "actions" }, actions));
  }));
}

function deviceOf(p) { return p === "Phone" || p.startsWith("Phone/") ? "Phone" : "PC"; }
function otherDevice(p) { return p !== "" && deviceOf(p) !== state.engine.device; }

function openCloud(p) {
  cloudPath = p;
  loadCloud();
}

async function restore(path, isFolder, fromOtherDevice) {
  const orig = el("input", { type: "radio", name: "dest", value: "orig", checked: !fromOtherDevice, disabled: !!fromOtherDevice });
  const other = el("input", { type: "radio", name: "dest", value: "other", checked: !!fromOtherDevice });
  const destInput = el("input", { type: "text", placeholder: onPhone ? "/storage/emulated/0/Download/tgvault" : "C:\\Users\\you\\Restored", autocomplete: "off" });
  if (onPhone && fromOtherDevice) destInput.value = state.phone.downloadDir;
  const body = el("div", {},
    el("p", {}, `Restore ${path}${isFolder ? " with all its subfolders" : ""}.`),
    el("label", { class: "choice" }, orig, el("div", {}, el("b", {}, "Original location"),
      el("div", { class: "hint" }, fromOtherDevice
        ? `These were backed up from your ${deviceOf(path) === "Phone" ? "phone" : "PC"}, so choose a folder on ${here}.`
        : "Files already there are never overwritten; a different existing file keeps its name and the restored copy gets “(restored)” added."))),
    el("label", { class: "choice" }, other, el("div", { class: "grow" }, el("b", {}, "Another folder"), destInput,
      onPhone ? null : el("button", { type: "button", class: "small", onclick: async () => {
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
    n ? el("p", {}, `Delete ${n} item${n > 1 ? "s" : ""} (${bytes(plan.bytes)}) from ${here}. Each one is checked against its Telegram copy first, and you can restore it any time from Cloud files.`)
      : el("p", {}, "Nothing here can be removed yet, because nothing is fully backed up."),
    plan.candidates.some((c) => c.kind === "project")
      ? el("p", { class: "hint" }, "Project folders are deleted whole, including rebuildable folders like node_modules that were not backed up.") : null,
    plan.kept && plan.kept.length ? el("div", {}, el("p", { class: "hint" }, `Kept on ${here}:`),
      el("ul", { class: "scroll" }, plan.kept.slice(0, 200).map((k) => el("li", {}, k)))) : null);
  await dialog({
    title: "Free up space", body,
    buttons: n ? [{ label: "Cancel", value: null }, { label: `Delete ${bytes(plan.bytes)} from ${here}`, value: "go", danger: true }]
      : [{ label: "Close", value: null }],
    run: async () => {
      await api("/api/free", { path });
      toast("Freeing space; progress is on the Overview tab.");
      refresh();
    },
  });
}

async function removeFromBackup(path, fromDevice) {
  const from = fromDevice === "Phone" ? "your phone" : "your PC";
  await dialog({
    title: "Remove from backup?",
    body: el("div", {},
      el("p", {}, `Permanently delete ${path} and all its versions from your Telegram backup channel.`),
      fromDevice ? el("p", {}, `These were backed up from ${from}. If ${from} still has them, it will back them up again the next time it checks Telegram. To stop that, stop backing up the folder on ${from} first.`) : null,
      el("p", { class: "hint" }, `This can't be undone. Files on ${here} are not touched. It only works for things no longer being backed up from ${here}.`)),
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
    downloadButton("Download", `/api/download?msg=${v.msgId}`, { path: entry.path, msg: v.msgId }))));
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
