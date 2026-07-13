"use strict";

const $ = (id) => document.getElementById(id);

const CLASS_LABELS = {
  dead: "Dead",
  soft_404: "Soft 404",
  blocked: "Blocked",
  unknown: "Unknown",
  alive: "Alive",
};

let currentReport = null;
let currentFilter = "problems";
let eventSource = null;

$("scan-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  startScan($("url").value.trim());
});

async function startScan(url) {
  if (!url) return;
  resetView();
  $("scan-btn").disabled = true;
  showProgress({ phase: "starting" });

  try {
    const resp = await fetch("/api/scans", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        url,
        max_depth: parseInt($("opt-depth").value, 10),
        max_pages: parseInt($("opt-pages").value, 10),
        check_external: $("opt-external").checked,
        soft404: $("opt-soft404").checked,
        fragments: $("opt-fragments").checked,
      }),
    });
    if (!resp.ok) throw new Error((await resp.json()).error || resp.statusText);
    const { id } = await resp.json();
    watchScan(id);
  } catch (err) {
    showError(`Could not start scan: ${err.message}`);
    $("scan-btn").disabled = false;
  }
}

function watchScan(id) {
  if (eventSource) eventSource.close();
  eventSource = new EventSource(`/api/scans/${id}/events`);
  eventSource.addEventListener("progress", (e) => {
    showProgress(JSON.parse(e.data));
  });
  eventSource.addEventListener("done", async () => {
    eventSource.close();
    eventSource = null;
    await loadScan(id);
    $("scan-btn").disabled = false;
    loadRecent();
  });
  eventSource.onerror = () => {
    // Connection dropped: fall back to polling the record.
    eventSource.close();
    eventSource = null;
    pollScan(id);
  };
}

async function pollScan(id) {
  for (let i = 0; i < 600; i++) {
    await new Promise((r) => setTimeout(r, 2000));
    const resp = await fetch(`/api/scans/${id}`);
    if (!resp.ok) break;
    const rec = await resp.json();
    if (rec.status !== "running") {
      renderRecord(rec);
      $("scan-btn").disabled = false;
      loadRecent();
      return;
    }
  }
  showError("Lost contact with the scan.");
  $("scan-btn").disabled = false;
}

async function loadScan(id) {
  const resp = await fetch(`/api/scans/${id}`);
  if (!resp.ok) {
    showError("Could not load scan results.");
    return;
  }
  renderRecord(await resp.json());
}

function showProgress(p) {
  $("progress").hidden = false;
  const fill = $("progress-fill");
  const phase = p.phase || "starting";
  let text = `Phase: ${phase}`;
  if (phase === "crawl" || phase === "starting") {
    fill.classList.add("indeterminate");
    if (p.pages_crawled) text += ` · ${p.pages_crawled} pages · ${p.links_found} links found`;
  } else {
    fill.classList.remove("indeterminate");
    const pct = p.links_total ? Math.round((100 * p.links_checked) / p.links_total) : 0;
    fill.style.width = `${pct}%`;
    text += ` · checked ${p.links_checked}/${p.links_total}`;
    if (p.broken) text += ` · ${p.broken} broken so far`;
  }
  $("progress-text").textContent = text;
}

function renderRecord(rec) {
  $("progress").hidden = true;
  if (rec.status === "failed") {
    showError(`Scan failed: ${rec.error || "unknown error"}`);
    return;
  }
  if (!rec.report) {
    showError("Scan finished but produced no report.");
    return;
  }
  currentReport = rec.report;
  renderSummary(currentReport);
  renderFilters();
  renderResults();
}

function renderSummary(rep) {
  const counts = rep.counts || {};
  const secs = (new Date(rep.finished_at) - new Date(rep.started_at)) / 1000;
  $("summary").hidden = false;
  $("summary").innerHTML = `
    <div class="stat"><div class="num">${rep.pages_crawled}</div><div class="lbl">pages</div></div>
    <div class="stat"><div class="num">${rep.total_links}</div><div class="lbl">links</div></div>
    <div class="stat dead"><div class="num">${counts.dead || 0}</div><div class="lbl">dead</div></div>
    <div class="stat soft"><div class="num">${counts.soft_404 || 0}</div><div class="lbl">soft 404</div></div>
    <div class="stat blocked"><div class="num">${counts.blocked || 0}</div><div class="lbl">blocked</div></div>
    <div class="stat alive"><div class="num">${counts.alive || 0}</div><div class="lbl">alive</div></div>
    <div class="stat"><div class="num">${secs.toFixed(1)}s</div><div class="lbl">duration</div></div>`;
}

function renderFilters() {
  const filters = [
    ["problems", "Problems"],
    ["dead", "Dead"],
    ["soft_404", "Soft 404"],
    ["blocked", "Blocked"],
    ["unknown", "Unknown"],
    ["anchors", "Missing anchors"],
    ["all", "All"],
  ];
  $("results").hidden = false;
  $("filters").innerHTML = "";
  for (const [key, label] of filters) {
    const btn = document.createElement("button");
    btn.textContent = label;
    btn.className = key === currentFilter ? "active" : "";
    btn.onclick = () => {
      currentFilter = key;
      renderFilters();
      renderResults();
    };
    $("filters").appendChild(btn);
  }
}

function filterResults(rep) {
  const rs = rep.results || [];
  switch (currentFilter) {
    case "problems":
      return rs.filter((r) => r.class !== "alive" || (r.missing_fragments || []).length);
    case "anchors":
      return rs.filter((r) => (r.missing_fragments || []).length);
    case "all":
      return rs;
    default:
      return rs.filter((r) => r.class === currentFilter);
  }
}

function renderResults() {
  const list = $("result-list");
  list.innerHTML = "";
  const rs = filterResults(currentReport);
  if (!rs.length) {
    list.innerHTML = `<p class="empty">${
      currentFilter === "problems" ? "No problems found 🎉" : "Nothing in this category."
    }</p>`;
    return;
  }
  for (const r of rs.slice(0, 500)) list.appendChild(renderResult(r));
  if (rs.length > 500) {
    const p = document.createElement("p");
    p.className = "empty";
    p.textContent = `…and ${rs.length - 500} more (use the JSON API for the full list)`;
    list.appendChild(p);
  }
}

function renderResult(r) {
  const div = document.createElement("div");
  div.className = "result";

  const head = document.createElement("div");
  head.className = "result-head";
  head.innerHTML = `<span class="badge ${r.class}">${CLASS_LABELS[r.class] || r.class}</span>`;
  const a = document.createElement("a");
  a.className = "result-url";
  a.href = r.url;
  a.target = "_blank";
  a.rel = "noopener";
  a.textContent = r.url;
  head.appendChild(a);
  div.appendChild(head);

  const meta = document.createElement("div");
  meta.className = "result-meta";
  const bits = [];
  if (r.status) bits.push(`HTTP ${r.status}`);
  if (r.reason) bits.push(r.reason.replaceAll("_", " "));
  bits.push(`confidence ${Math.round(r.confidence * 100)}%`);
  if (r.attempts > 1) bits.push(`${r.attempts} attempts`);
  if (r.cached) bits.push("from cache");
  if (r.final_url && r.final_url !== r.url) bits.push(`→ ${r.final_url}`);
  meta.textContent = bits.join(" · ");
  div.appendChild(meta);

  if ((r.missing_fragments || []).length) {
    const frags = document.createElement("div");
    frags.className = "result-frags";
    frags.textContent = `missing anchors: ${r.missing_fragments.map((f) => "#" + f).join(", ")}`;
    div.appendChild(frags);
  }

  const refs = (r.refs || []).filter((ref) => ref.page);
  if (refs.length) {
    const det = document.createElement("details");
    det.className = "refs";
    const sum = document.createElement("summary");
    sum.textContent = `found on ${refs.length} page${refs.length > 1 ? "s" : ""}`;
    det.appendChild(sum);
    const ul = document.createElement("ul");
    for (const ref of refs.slice(0, 25)) {
      const li = document.createElement("li");
      const pa = document.createElement("a");
      pa.href = ref.page;
      pa.target = "_blank";
      pa.rel = "noopener";
      pa.textContent = ref.page;
      li.appendChild(pa);
      if (ref.text) {
        const t = document.createElement("span");
        t.className = "anchor-text";
        t.textContent = ` — “${ref.text}”`;
        li.appendChild(t);
      }
      ul.appendChild(li);
    }
    det.appendChild(ul);
    div.appendChild(det);
  }

  if (r.class === "dead" || r.class === "soft_404") {
    const wb = document.createElement("button");
    wb.className = "wayback";
    wb.textContent = "Find archived copy";
    wb.onclick = () => findWayback(r.url, wb);
    div.appendChild(wb);
  }
  return div;
}

async function findWayback(url, btn) {
  btn.disabled = true;
  btn.textContent = "Searching archive.org…";
  try {
    const resp = await fetch(
      `https://archive.org/wayback/available?url=${encodeURIComponent(url)}`
    );
    const data = await resp.json();
    const snap = data.archived_snapshots && data.archived_snapshots.closest;
    if (snap && snap.available) {
      const a = document.createElement("a");
      a.className = "wayback";
      a.href = snap.url;
      a.target = "_blank";
      a.rel = "noopener";
      a.textContent = `Archived copy (${snap.timestamp.slice(0, 4)}) ↗`;
      btn.replaceWith(a);
    } else {
      btn.textContent = "No archived copy found";
    }
  } catch {
    btn.textContent = "Archive lookup failed";
    btn.disabled = false;
  }
}

async function loadRecent() {
  try {
    const resp = await fetch("/api/scans");
    if (!resp.ok) return;
    const recs = await resp.json();
    const list = $("recent-list");
    list.innerHTML = "";
    if (!recs || !recs.length) {
      list.innerHTML = `<p class="empty">No scans yet.</p>`;
      return;
    }
    for (const rec of recs.slice(0, 10)) {
      const item = document.createElement("div");
      item.className = "recent-item";
      const when = new Date(rec.created_at).toLocaleString();
      let right = rec.status;
      if (rec.status === "done" && rec.report) {
        right = rec.report.broken
          ? `<span class="broken-count">${rec.report.broken} broken</span>`
          : `<span class="ok-count">all good</span>`;
      }
      item.innerHTML = `<span class="seed">${escapeHTML(rec.seed)}</span>
        <span class="meta">${right} · ${when}</span>`;
      item.onclick = () => {
        if (rec.status === "running") watchScan(rec.id);
        else loadScan(rec.id);
      };
      list.appendChild(item);
    }
  } catch {
    /* recent list is best-effort */
  }
}

function escapeHTML(s) {
  const d = document.createElement("span");
  d.textContent = s;
  return d.innerHTML;
}

function resetView() {
  $("summary").hidden = true;
  $("results").hidden = true;
  document.querySelectorAll(".error-banner").forEach((el) => el.remove());
}

function showError(msg) {
  $("progress").hidden = true;
  document.querySelectorAll(".error-banner").forEach((el) => el.remove());
  const div = document.createElement("div");
  div.className = "error-banner";
  div.textContent = msg;
  $("scan-form").after(div);
}

loadRecent();
