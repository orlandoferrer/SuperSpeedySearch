"use strict";

// ---------------------------------------------------------------------------
// Backend bridge. In the Wails app, window.go.main.App and window.runtime are
// injected. In a plain browser (frontend development), fall back to a demo
// backend with fake data so the UI is workable standalone.
// ---------------------------------------------------------------------------
const demoHandlers = {}; // event handlers registered in demo mode
const wails = window.go && window.go.main && window.go.main.App;
const Backend = wails || demoBackend();
const Events = window.runtime
  ? { on: (name, cb) => window.runtime.EventsOn(name, cb) }
  : { on: (name, cb) => (demoHandlers[name] = cb) };

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------
const state = {
  mode: "names", // "names" | "contents"
  nodes: [], // NodeInfo[]
  selected: new Set(), // node URLs included in searches
  searching: false,
  contentSummaries: {}, // nodeURL -> summary
  contentErrors: [], // {node_url, message}
  resultCount: 0,
  query: "",
};

const $ = (id) => document.getElementById(id);
const resultsEl = $("results");
const statusEl = $("status");
const summaryEl = $("summary");

// ---------------------------------------------------------------------------
// Nodes
// ---------------------------------------------------------------------------
async function refreshNodes() {
  $("node-list").innerHTML = `<li class="empty">Discovering…</li>`;
  let nodes = [];
  try {
    nodes = (await Backend.Discover()) || [];
  } catch (e) {
    toast("Discovery failed: " + e);
  }
  state.nodes = nodes;
  // keep selections; select newly-seen online nodes by default
  for (const n of nodes) {
    if (n.online && !state.selected.has(n.url) && !n.deselected) state.selected.add(n.url);
  }
  for (const url of [...state.selected]) {
    if (!nodes.some((n) => n.url === url)) state.selected.delete(url);
  }
  renderNodes();
}

function renderNodes() {
  const list = $("node-list");
  list.innerHTML = "";
  if (!state.nodes.length) {
    list.innerHTML = `<li class="empty">No nodes found. Is a node running?<br/>Add one by URL with “+”.</li>`;
    return;
  }
  for (const n of state.nodes) {
    const li = document.createElement("li");
    li.className = "node" + (n.online ? " online" : "");
    const needsToken = !n.online && /unauthorized/i.test(n.error || "");
    if (needsToken) li.classList.add("needs-token");

    const check = document.createElement("input");
    check.type = "checkbox";
    check.checked = state.selected.has(n.url);
    check.disabled = !n.online;
    check.addEventListener("change", () => {
      check.checked ? state.selected.add(n.url) : state.selected.delete(n.url);
    });

    const dot = el("span", "dot");
    const meta = el("div", "meta");
    const name = el("div", "name");
    name.textContent = n.name || n.id || n.url.replace(/^https?:\/\//, "");
    const sub = el("div", "sub");
    if (n.online) {
      sub.textContent = `${fmtCount(n.indexed_files)} files` + (n.last_scan ? ` · scanned ${ago(n.last_scan)}` : "");
    } else {
      sub.textContent = needsToken ? "needs token" : "offline";
      li.title = n.error || "";
    }
    meta.append(name, sub);

    const actions = el("div", "node-actions");
    actions.append(
      iconBtn("🔑", "Set token for this node", () =>
        askText(`Token for ${name.textContent}`, "Paste the node's auth token (from its startup log or auth_token file).", "", async (v) => {
          await Backend.SetNodeToken(n.url, v);
          refreshNodes();
        })
      ),
      iconBtn("↻", "Trigger a rescan", async () => {
        try {
          await Backend.TriggerScan(n.url);
          toast("Rescan started on " + name.textContent);
        } catch (e) {
          toast("" + e);
        }
      })
    );
    if (n.source === "manual") {
      actions.append(
        iconBtn("✕", "Remove this manual node", async () => {
          await Backend.RemoveManualNode(n.url);
          refreshNodes();
        })
      );
    }

    li.append(check, dot, meta, actions);
    list.append(li);
  }
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------
async function runSearch(ev) {
  ev && ev.preventDefault();
  const query = $("query").value.trim();
  if (!query || state.searching) return;
  const nodes = [...state.selected];
  if (!nodes.length) {
    toast("Select at least one online node");
    return;
  }
  state.query = query;
  const extensions = $("ext-filter")
    .value.split(",")
    .map((s) => s.trim())
    .filter(Boolean);
  const limit = parseInt($("limit").value, 10) || 100;

  resultsEl.innerHTML = "";
  summaryEl.classList.add("hidden");
  state.resultCount = 0;
  state.contentSummaries = {};
  state.contentErrors = [];

  if (state.mode === "names") {
    setBusy(true, `Searching ${nodes.length} node${nodes.length > 1 ? "s" : ""}…`);
    try {
      const resp = await Backend.SearchMetadata(query, extensions, nodes, limit);
      renderMetaResults(resp.results || [], resp.errors || []);
    } catch (e) {
      toast("" + e);
    } finally {
      setBusy(false);
    }
  } else {
    setBusy(true, "Deep searching…");
    $("cancel-btn").classList.remove("hidden");
    try {
      await Backend.StartContentSearch(query, extensions, nodes, limit);
    } catch (e) {
      toast("" + e);
      setBusy(false);
      $("cancel-btn").classList.add("hidden");
    }
  }
}

function renderMetaResults(results, errors) {
  if (!results.length) {
    resultsEl.innerHTML = `<div class="empty-state"><p>No matches for “${esc(state.query)}”.</p></div>`;
  }
  for (const r of results) resultsEl.append(metaRow(r));
  statusEl.textContent = `${results.length} result${results.length === 1 ? "" : "s"}`;
  renderSummaryBar(
    errors.map((e) => nodeLabel(e.node_url) + ": " + e.error),
    null
  );
}

function metaRow(r) {
  const row = el("div", "result");
  const icon = el("div", "icon");
  icon.textContent = r.is_dir ? "📁" : iconFor(r.extension);
  const body = el("div", "body");
  const fname = el("div", "fname");
  fname.textContent = r.filename;
  const fpath = el("div", "fpath");
  fpath.textContent = r.display_path;
  body.append(fname, fpath);
  const side = el("div", "side");
  side.innerHTML = `${r.is_dir ? "" : fmtSize(r.size_bytes)} ${r.modified_at ? "· " + ago(r.modified_at) : ""}<br/><span class="badge">${esc(r.node_id)}</span>`;
  row.append(icon, body, side, rowActions(r));
  return row;
}

function contentRow(r) {
  const row = el("div", "result");
  const icon = el("div", "icon");
  icon.textContent = iconFor("");
  const body = el("div", "body");
  const fname = el("div", "fname");
  fname.textContent = r.filename + (r.line ? `:${r.line}` : "");
  const fpath = el("div", "fpath");
  fpath.textContent = r.display_path;
  const snip = el("div", "snippet");
  snip.innerHTML = highlight(r.snippet, state.query);
  body.append(fname, fpath, snip);
  const side = el("div", "side");
  side.innerHTML = `<span class="badge">${esc(r.node_id)}</span>`;
  row.append(icon, body, side, rowActions(r));
  return row;
}

function rowActions(r) {
  const actions = el("div", "row-actions");
  actions.append(
    textBtn("Copy", "Copy full path", async () => {
      try {
        await Backend.ClipboardSet(r.path);
        toast("Path copied");
      } catch {
        navigator.clipboard && navigator.clipboard.writeText(r.path);
        toast("Path copied");
      }
    })
  );
  if (r.open_uri) {
    actions.append(textBtn("Open", "Open via " + r.open_uri.split(":")[0], () => Backend.OpenURI(r.open_uri).catch((e) => toast("" + e))));
    if (r.open_uri.startsWith("file://")) {
      actions.append(textBtn("Reveal", "Reveal in file manager", () => Backend.RevealFileURI(r.open_uri).catch((e) => toast("" + e))));
    }
  }
  return actions;
}

// Streaming content-search events from the Go side.
Events.on("content", (ev) => {
  if (ev.type === "result" && ev.result) {
    state.resultCount++;
    resultsEl.append(contentRow(ev.result));
    statusEl.textContent = `${state.resultCount} match${state.resultCount === 1 ? "" : "es"}…`;
  } else if (ev.type === "summary" && ev.summary) {
    state.contentSummaries[ev.node_url] = ev.summary;
  } else if (ev.type === "error") {
    state.contentErrors.push(ev);
  }
});

Events.on("content_done", (cancelled) => {
  setBusy(false);
  $("cancel-btn").classList.add("hidden");
  if (!state.resultCount && !cancelled) {
    resultsEl.innerHTML = `<div class="empty-state"><p>No content matches for “${esc(state.query)}”.</p></div>`;
  }
  const sums = Object.values(state.contentSummaries);
  const searched = sums.reduce((a, s) => a + (s.searched_files || 0), 0);
  const skipped = sums.reduce((a, s) => a + (s.skipped_files || 0), 0);
  const timedOut = sums.some((s) => s.timed_out);
  statusEl.textContent = cancelled ? "Cancelled" : `${state.resultCount} match${state.resultCount === 1 ? "" : "es"}`;
  renderSummaryBar(
    state.contentErrors.map((e) => nodeLabel(e.node_url) + ": " + e.message),
    `Searched ${fmtCount(searched)} files` + (skipped ? `, skipped ${fmtCount(skipped)}` : "") + (timedOut ? " · some nodes hit the time limit" : "")
  );
});

function renderSummaryBar(errors, info) {
  const parts = [];
  if (info) parts.push(esc(info));
  for (const e of errors) parts.push(`<span class="node-error">⚠ ${esc(e)}</span>`);
  if (!parts.length) {
    summaryEl.classList.add("hidden");
    return;
  }
  summaryEl.innerHTML = parts.join(" &nbsp;·&nbsp; ");
  summaryEl.classList.remove("hidden");
}

function setBusy(busy, msg) {
  state.searching = busy;
  statusEl.classList.toggle("busy", busy);
  if (msg) statusEl.textContent = msg;
  $("search-btn").disabled = busy;
}

// ---------------------------------------------------------------------------
// UI plumbing
// ---------------------------------------------------------------------------
$("search-form").addEventListener("submit", runSearch);
$("cancel-btn").addEventListener("click", () => Backend.CancelContentSearch());
$("refresh-nodes").addEventListener("click", refreshNodes);
$("add-node").addEventListener("click", () =>
  askText("Add node", "URL or host of a node, e.g. 192.168.1.40 or http://nas.local:37373", "", async (v) => {
    try {
      await Backend.AddManualNode(v);
      refreshNodes();
    } catch (e) {
      toast("" + e);
    }
  })
);
$("default-token").addEventListener("click", () =>
  askText("Default token", "Used for any node without its own token. Handy when all your nodes share one token.", "", async (v) => {
    await Backend.SetDefaultToken(v);
    refreshNodes();
  })
);
for (const btn of document.querySelectorAll(".seg")) {
  btn.addEventListener("click", () => {
    document.querySelectorAll(".seg").forEach((b) => b.classList.remove("active"));
    btn.classList.add("active");
    state.mode = btn.dataset.mode;
    $("query").focus();
  });
}
document.addEventListener("keydown", (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key === "f") {
    e.preventDefault();
    $("query").focus();
    $("query").select();
  }
  if (e.key === "Escape" && state.searching && state.mode === "contents") Backend.CancelContentSearch();
});

// modal
function askText(title, desc, initial, onOk) {
  $("modal-title").textContent = title;
  $("modal-desc").textContent = desc;
  const input = $("modal-input");
  input.value = initial;
  $("modal-backdrop").classList.remove("hidden");
  input.focus();
  const done = () => {
    $("modal-backdrop").classList.add("hidden");
    $("modal-ok").onclick = $("modal-cancel").onclick = input.onkeydown = null;
  };
  $("modal-ok").onclick = () => {
    const v = input.value.trim();
    done();
    onOk(v);
  };
  $("modal-cancel").onclick = done;
  input.onkeydown = (e) => {
    if (e.key === "Enter") $("modal-ok").onclick();
    if (e.key === "Escape") done();
  };
}

let toastTimer;
function toast(msg) {
  const t = $("toast");
  t.textContent = msg;
  t.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.add("hidden"), 3200);
}

// helpers
function el(tag, cls) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  return e;
}
function iconBtn(txt, title, onclick) {
  const b = el("button");
  b.textContent = txt;
  b.title = title;
  b.addEventListener("click", onclick);
  return b;
}
function textBtn(txt, title, onclick) {
  return iconBtn(txt, title, onclick);
}
function esc(s) {
  return ("" + (s ?? "")).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
function highlight(snippet, query) {
  const safe = esc(snippet);
  const q = esc(query);
  const idx = safe.toLowerCase().indexOf(q.toLowerCase());
  if (idx < 0) return safe;
  return safe.slice(0, idx) + "<mark>" + safe.slice(idx, idx + q.length) + "</mark>" + safe.slice(idx + q.length);
}
function fmtSize(b) {
  if (b == null) return "";
  if (b < 1024) return b + " B";
  const units = ["KB", "MB", "GB", "TB"];
  let i = -1;
  do {
    b /= 1024;
    i++;
  } while (b >= 1024 && i < units.length - 1);
  return b.toFixed(b >= 10 ? 0 : 1) + " " + units[i];
}
function fmtCount(n) {
  return (n ?? 0).toLocaleString();
}
function ago(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return "";
  const s = (Date.now() - d.getTime()) / 1000;
  if (s < 90) return "just now";
  if (s < 5400) return Math.round(s / 60) + "m ago";
  if (s < 129600) return Math.round(s / 3600) + "h ago";
  return Math.round(s / 86400) + "d ago";
}
function iconFor(ext) {
  const map = {
    ".pdf": "📕", ".doc": "📄", ".docx": "📄", ".txt": "📄", ".md": "📝",
    ".xls": "📊", ".xlsx": "📊", ".csv": "📊", ".numbers": "📊",
    ".jpg": "🖼️", ".jpeg": "🖼️", ".png": "🖼️", ".gif": "🖼️", ".heic": "🖼️",
    ".mp4": "🎬", ".mov": "🎬", ".mkv": "🎬", ".avi": "🎬",
    ".mp3": "🎵", ".flac": "🎵", ".m4a": "🎵", ".wav": "🎵",
    ".zip": "🗜️", ".gz": "🗜️", ".7z": "🗜️", ".rar": "🗜️",
    ".go": "💻", ".js": "💻", ".ts": "💻", ".py": "💻", ".sh": "💻",
  };
  return map[(ext || "").toLowerCase()] || "📄";
}
function nodeLabel(url) {
  const n = state.nodes.find((x) => x.url === url);
  return n ? n.name || n.id || url : url;
}

// ---------------------------------------------------------------------------
// Demo backend for plain-browser development (no Wails runtime).
// ---------------------------------------------------------------------------
function demoBackend() {
  const files = [
    { node_id: "macbook-pro", filename: "tax 2024.pdf", display_path: "MacBook Pro:Documents/taxes/tax 2024.pdf", path: "/Users/o/Documents/taxes/tax 2024.pdf", extension: ".pdf", size_bytes: 482133, modified_at: "2026-02-11T10:00:00Z", match_type: "filename", open_uri: "file:///Users/o/Documents/taxes/tax%202024.pdf" },
    { node_id: "synology-main", filename: "tax-summary.xlsx", display_path: "Synology:Documents/finance/tax-summary.xlsx", path: "/mnt/documents/finance/tax-summary.xlsx", extension: ".xlsx", size_bytes: 88211, modified_at: "2026-06-30T10:00:00Z", match_type: "filename", open_uri: "smb://synology.local/documents/finance/tax-summary.xlsx" },
    { node_id: "synology-main", filename: "receipt.jpg", display_path: "Synology:Photos/2024/taxes/receipt.jpg", path: "/mnt/photos/2024/taxes/receipt.jpg", extension: ".jpg", size_bytes: 2400000, modified_at: "2025-04-02T10:00:00Z", match_type: "path" },
  ];
  return {
    Discover: async () => [
      { url: "http://192.168.1.25:37373", id: "macbook-pro", name: "MacBook Pro", source: "mdns", online: true, auth_required: true, indexed_files: 48211, last_scan: new Date(Date.now() - 3600e3).toISOString() },
      { url: "http://192.168.1.40:37373", id: "synology-main", name: "Synology", source: "mdns", online: true, auth_required: true, indexed_files: 812400, last_scan: new Date(Date.now() - 7200e3).toISOString() },
      { url: "http://10.0.0.9:37373", source: "manual", online: false, error: "unauthorized: check the node's auth token" },
    ],
    SearchMetadata: async (q) => ({ results: files.filter((f) => f.filename.includes(q) || f.display_path.includes(q) || q === "tax"), errors: [] }),
    StartContentSearch: async (q) => {
      let i = 0;
      const rows = [
        { node_id: "macbook-pro", filename: "notes.md", display_path: "MacBook Pro:Documents/notes.md", path: "/Users/o/Documents/notes.md", snippet: "remember: property tax due in April", line: 12, match_type: "content", open_uri: "file:///Users/o/Documents/notes.md" },
        { node_id: "synology-main", filename: "letter.txt", display_path: "Synology:Documents/letter.txt", path: "/mnt/documents/letter.txt", snippet: "...regarding the property tax assessment of the home...", line: 3, match_type: "content" },
      ];
      const t = setInterval(() => {
        if (i < rows.length) demoHandlers["content"]?.({ type: "result", result: rows[i], node_url: "demo" });
        else {
          clearInterval(t);
          demoHandlers["content"]?.({ type: "summary", node_url: "demo", summary: { searched_files: 1284, skipped_files: 12 } });
          demoHandlers["content_done"]?.(false);
        }
        i++;
      }, 350);
    },
    CancelContentSearch: async () => demoHandlers["content_done"]?.(true),
    SetNodeToken: async () => {}, SetDefaultToken: async () => {}, AddManualNode: async () => {},
    RemoveManualNode: async () => {}, TriggerScan: async () => {}, ClipboardSet: async (t) => navigator.clipboard?.writeText(t),
    OpenURI: async () => {}, RevealFileURI: async () => {},
  };
}

// boot
refreshNodes();
