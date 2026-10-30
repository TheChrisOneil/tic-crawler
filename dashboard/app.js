// TiC Payer Registry dashboard. Plain JS, no build step.
// Data comes from the `data` branch, written by the crawl and dry-run workflows.
const REPO = "TheChrisOneil/tic-crawler";
const DATA = `https://raw.githubusercontent.com/${REPO}/data/out`;
const BLOB = `https://github.com/${REPO}/blob/data/out`;
const app = document.getElementById("app");
const cache = { data: null, payers: {} };

// ---------- helpers ----------
const ACCESS = {
  ok: "OK", manual: "Manual TOC", robots_disallow: "robots.txt disallows", robots_unreachable: "robots.txt unreachable",
  waf_blocked: "Bot wall / WAF", http_error: "HTTP error", network_error: "Network error", no_toc_url: "No TOC URL known",
  too_large: "TOC too large", invalid_url: "Not a URL",
};
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const shortDate = (s) => { const t = Date.parse(s); return isNaN(t) ? (s || "") : new Date(t).toISOString().slice(0, 10); };
const num = (n) => (n == null || n === "" ? "" : Number(n).toLocaleString());
function bytes(n) {
  if (!n) return n === 0 ? "0 B" : "";
  const u = ["B", "KB", "MB", "GB", "TB", "PB"];
  let i = 0, v = Number(n);
  while (v >= 1000 && i < u.length - 1) { v /= 1000; i++; }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${u[i]}`;
}
const statusBadge = (s) => {
  const cls = { ok: "b-ok", unchanged: "b-ok", failed: "b-bad", skipped: "b-warn", deferred: "b-warn", never: "b-idle" }[s] || "b-idle";
  const label = s === "never" ? "not crawled" : s === "deferred" ? "retry next run" : s;
  return `<span class="badge ${cls}">${esc(label)}</span>`;
};
const verdictBadge = (v) => {
  const cls = { accessible: "b-ok", robots_blocked: "b-warn", bot_wall: "b-bad", http_error: "b-bad", unreachable: "b-bad" }[v] || "b-idle";
  return `<span class="badge ${cls}">${esc((v || "").replace("_", " "))}</span>`;
};
const validBadge = (p) => {
  if (p.schema_valid == null) return `<span class="muted">n/a</span>`;
  return p.schema_valid ? `<span class="badge b-ok">valid</span>`
    : `<span class="badge b-warn">${num(p.validation_errors)} ${p.validation_errors === 1 ? "error" : "errors"}</span>`;
};
function fresh(p) {
  if (!p.toc_last_updated_on) return `<span class="muted">n/a</span>`;
  const cls = p.stale ? "b-bad" : "b-ok";
  return `<span class="nowrap">${esc(p.toc_last_updated_on)} <span class="badge ${cls}">${p.age_days}d</span></span>`;
}
async function getJSON(url) {
  const r = await fetch(url, { cache: "no-store" });
  if (!r.ok) throw new Error(`${r.status} ${url}`);
  return r.json();
}
async function loadData() {
  if (!cache.data) {
    cache.data = await getJSON(`${DATA}/dashboard/data.json`);
    const g = cache.data.generated_at;
    document.getElementById("generated").textContent =
      `Data generated ${g.replace("T", " ").replace("Z", " UTC")} · schema v${cache.data.schema_version} · stale after ${cache.data.stale_threshold_days} days`;
  }
  return cache.data;
}
async function loadPayer(id) {
  if (!cache.payers[id]) cache.payers[id] = await getJSON(`${DATA}/dashboard/payers/${encodeURIComponent(id)}.json`);
  return cache.payers[id];
}

// Sortable, filterable, paged table. cols: {key, label, num?, render?(row), sort?(row)}
function table(el, rows, cols, opts = {}) {
  const state = { sort: opts.sort || null, dir: opts.dir || 1, page: 0, size: opts.pageSize || 100 };
  function draw() {
    let r = rows.slice();
    if (state.sort) {
      const c = cols.find((c) => c.key === state.sort);
      const val = c.sort || ((x) => x[c.key]);
      r.sort((a, b) => {
        const va = val(a), vb = val(b);
        if (va == null && vb == null) return 0;
        if (va == null) return 1;
        if (vb == null) return -1;
        return (va > vb ? 1 : va < vb ? -1 : 0) * state.dir;
      });
    }
    const pages = Math.max(1, Math.ceil(r.length / state.size));
    state.page = Math.min(state.page, pages - 1);
    const slice = r.slice(state.page * state.size, (state.page + 1) * state.size);
    el.innerHTML = r.length === 0 ? `<div class="empty">${opts.empty || "Nothing to show."}</div>` : `
      <div class="tablewrap"><table><thead><tr>${cols.map((c) =>
        `<th class="${c.num ? "num" : ""}" data-k="${c.key}">${esc(c.label)}${state.sort === c.key ? (state.dir > 0 ? " ▲" : " ▼") : ""}</th>`).join("")}
      </tr></thead><tbody>${slice.map((row) => `<tr${opts.onclick ? ` style="cursor:pointer" data-id="${esc(opts.rowId(row))}"` : ""}>${cols.map((c) =>
        `<td class="${c.num ? "num" : ""}">${c.render ? c.render(row) : esc(row[c.key])}</td>`).join("")}</tr>`).join("")}
      </tbody></table></div>
      ${pages > 1 ? `<div class="pager"><button data-p="-1">Prev</button> Page ${state.page + 1} of ${pages} · ${num(r.length)} rows <button data-p="1">Next</button></div>`
        : `<div class="pager">${num(r.length)} rows</div>`}`;
    el.querySelectorAll("th").forEach((th) => th.onclick = () => {
      const k = th.dataset.k;
      state.dir = state.sort === k ? -state.dir : (cols.find((c) => c.key === k).num ? -1 : 1);
      state.sort = k; draw();
    });
    el.querySelectorAll("[data-p]").forEach((b) => b.onclick = () => { state.page = Math.max(0, state.page + Number(b.dataset.p)); draw(); });
    if (opts.onclick) el.querySelectorAll("tbody tr").forEach((tr) => tr.onclick = (e) => {
      if (e.target.closest("a")) return;
      opts.onclick(tr.dataset.id);
    });
  }
  draw();
  return { update(newRows) { rows = newRows; state.page = 0; draw(); } };
}

// ---------- views ----------
async function overview() {
  const d = await loadData();
  const t = d.totals;
  const tiles = [
    ["Payers tracked", num(t.payers)], ["With a TOC source", num(t.with_toc_source)],
    ["Crawled OK", num(t.ok)], ["Failed", num(t.failed)], ["Retry next run", num(t.deferred || 0)], ["Stale TOCs", num(t.stale)],
    ["MRF files", num(t.mrf_urls)], ["Size on payer servers", bytes(t.mrf_bytes)], ["Schema-invalid TOCs", num(t.schema_invalid)],
  ];
  app.innerHTML = `
    <div class="kpis">${tiles.map(([l, v]) => `<div class="kpi"><div class="v">${v}</div><div class="l">${l}</div></div>`).join("")}</div>
    <h2>Payers</h2>
    <div class="toolbar">
      <input type="search" id="q" placeholder="Search payer, state, URL…" aria-label="Search payers">
      <select id="st" aria-label="Filter by status">
        <option value="">All statuses</option><option value="crawled">Crawled</option><option value="ok">OK</option>
        <option value="failed">Failed or skipped</option><option value="never">Not crawled</option><option value="stale">Stale</option>
      </select>
      <select id="co" aria-label="Filter by rollout group">
        <option value="">All rollout groups</option>${(d.cohorts || []).map((c) => `<option value="${esc(c)}">${esc(c)}</option>`).join("")}
      </select>
    </div>
    <div id="tbl"></div>
    <p class="note">Freshness is the TOC's own <code>last_updated_on</code>. Bytes are the sum of Content-Length from HEAD checks; raw files are not downloaded.</p>`;
  const cols = [
    { key: "legal_name", label: "Payer", render: (p) => `<a href="#/payer/${encodeURIComponent(p.payer_id)}">${esc(p.legal_name)}</a><div class="muted mono">${esc(p.payer_id)}${(p.cohorts || []).map((c) => ` <span class="badge b-idle">${esc(c)}</span>`).join("")}</div>` },
    { key: "status", label: "Status", render: (p) => statusBadge(p.status) },
    { key: "toc_last_updated_on", label: "TOC updated", render: fresh, sort: (p) => p.age_days },
    { key: "last_crawled", label: "Last crawled", render: (p) => `<span class="nowrap">${esc((p.last_crawled || "").slice(0, 10))}</span>` },
    { key: "mrf_count", label: "MRF files", num: true, render: (p) => p.status === "never" ? "" : num(p.mrf_count) },
    { key: "total_bytes", label: "Size on payer servers", num: true, render: (p) => p.status === "never" ? "" : `<span class="nowrap">${bytes(p.total_bytes)}</span>` },
    { key: "schema_valid", label: "Schema", render: validBadge, sort: (p) => p.validation_errors },
    { key: "error", label: "Last error", render: (p) => `${p.access_status && p.access_status !== "ok" ? `<span class="badge b-idle">${esc(ACCESS[p.access_status] || p.access_status)}</span> ` : ""}<div class="clip muted" title="${esc(p.error)}">${esc(p.error)}</div>` },
  ];
  const tb = table(document.getElementById("tbl"), d.payers, cols, {
    sort: "total_bytes", dir: -1, onclick: (id) => location.hash = `#/payer/${encodeURIComponent(id)}`, rowId: (p) => p.payer_id,
  });
  const apply = () => {
    const q = document.getElementById("q").value.toLowerCase();
    const st = document.getElementById("st").value;
    const co = document.getElementById("co").value;
    tb.update(d.payers.filter((p) => {
      if (co && !(p.cohorts || []).includes(co)) return false;
      const hay = `${p.legal_name} ${p.payer_id} ${(p.states || []).join(" ")} ${p.toc_url} ${p.landing_url}`.toLowerCase();
      if (q && !hay.includes(q)) return false;
      if (st === "crawled") return p.status !== "never";
      if (st === "ok") return p.status === "ok" || p.status === "unchanged";
      if (st === "failed") return ["failed", "skipped", "deferred"].includes(p.status);
      if (st === "never") return p.status === "never";
      if (st === "stale") return p.stale;
      return true;
    }));
  };
  document.getElementById("q").oninput = apply;
  document.getElementById("st").onchange = apply;
  document.getElementById("co").onchange = apply;
}

async function payerView(id) {
  const [d, det] = await Promise.all([loadData(), loadPayer(id)]);
  const p = d.payers.find((x) => x.payer_id === id) || {};
  const r = det.result || {};
  const v = r.validation || {};
  const rows = det.rows || [];
  const maxLen = Math.max(1, ...rows.map((x) => x.content_length || 0));
  const tocFile = (det.manifest_file || "").replace(/^manifests\//, "toc/").replace(/\.csv$/, ".json.gz");
  app.innerHTML = `
    <p><a href="#/">← All payers</a></p>
    <h2>${esc(det.payer.legal_name)} ${statusBadge(p.status || "never")}</h2>
    <div class="grid2">
      <div class="panel"><dl class="meta">
        <dt>Payer id</dt><dd class="mono">${esc(id)}</dd>
        <dt>Kind</dt><dd>${esc(det.payer.kind)}</dd>
        <dt>States</dt><dd>${esc((det.payer.states || []).join(", ")) || "<span class=muted>n/a</span>"}</dd>
        <dt>Landing page</dt><dd>${det.payer.landing_url ? `<a href="${esc(det.payer.landing_url)}" target="_blank" rel="noopener">${esc(det.payer.landing_url)}</a>` : "<span class=muted>none</span>"}</dd>
        <dt>TOC URL</dt><dd class="mono">${esc(r.toc_url || det.payer.toc_url || det.payer.toc_link_pattern || "none")}</dd>
        <dt>Notes</dt><dd>${esc(det.payer.notes) || "<span class=muted>none</span>"}</dd>
      </dl></div>
      <div class="panel"><dl class="meta">
        <dt>Last crawled</dt><dd>${esc(r.crawled_at || "never")}</dd>
        <dt>TOC updated</dt><dd>${fresh(p)}</dd>
        <dt>TOC version</dt><dd>${esc(v.file_version || "n/a")} (validated against v${esc(v.pinned_version || d.schema_version)})</dd>
        <dt>TOC size</dt><dd>${bytes(r.toc_bytes)} · SHA-256 <span class="mono">${esc((r.toc_sha256 || "").slice(0, 16))}…</span></dd>
        <dt>MRF files</dt><dd>${num(r.mrf_count)} · ${bytes(r.total_bytes)}</dd>
        <dt>Schema</dt><dd>${validBadge(p)} ${v.item_count ? `· ${num(v.invalid_items)} of ${num(v.item_count)} records with errors` : ""}</dd>
        <dt>Error</dt><dd>${esc(r.error) || "<span class=muted>none</span>"}</dd>
        <dt>Downloads</dt><dd>${det.manifest_file ? `<a href="${BLOB}/${det.manifest_file}">manifest CSV</a> · <a href="${DATA}/${tocFile}">TOC (.json.gz)</a>` : "<span class=muted>none yet</span>"}</dd>
      </dl></div>
    </div>
    <div class="tabs" role="tablist">
      <button data-t="files" class="active">Files (${num(rows.length)})</button>
      <button data-t="plans">Plans (${num(det.toc_plans?.plans?.length || 0)})</button>
      <button data-t="errors">Record errors (${num(v.error_count || 0)})</button>
    </div>
    <div id="tab"></div>`;
  const tab = document.getElementById("tab");
  const tabs = {
    files() {
      tab.innerHTML = `<div class="toolbar">
          <input type="search" id="fq" placeholder="Search URL, description, plan names…">
          <select id="ft"><option value="">All file types</option><option>in-network</option><option>allowed-amount</option></select>
          <select id="fs"><option value="">All results</option><option value="ok">HTTP 2xx</option><option value="err">Errors</option></select>
        </div><div id="ftbl"></div>`;
      const cols = [
        { key: "file_type", label: "Type", render: (x) => `<span class="nowrap">${esc(x.file_type)}</span>` },
        { key: "description", label: "Description", render: (x) => `<div class="clip" title="${esc(x.description)}">${esc(x.description)}</div><div class="mono clip muted" title="${esc(x.url)}">${esc(x.url)}</div>` },
        { key: "plan_count", label: "Plans", num: true, render: (x) => `${num(x.plan_count)}` },
        { key: "plan_names", label: "Plan names", render: (x) => `<div class="clip" title="${esc(x.plan_names)}">${esc(x.plan_names)}${x.plans_truncated ? " …" : ""}</div>` },
        { key: "http_status", label: "HTTP", render: (x) => x.check_error ? `<span class="badge b-bad">${esc(x.http_status || "")} ${esc(x.check_error)}</span>` : `<span class="badge b-ok">${esc(x.http_status)}</span> <span class="muted">${esc(x.check_method)}</span>` },
        { key: "content_length", label: "Size", num: true, render: (x) => `<span class="nowrap">${bytes(x.content_length)}</span><span class="sizebar" style="width:${Math.round(60 * (x.content_length || 0) / maxLen)}px"></span>` },
        { key: "last_modified", label: "Modified", render: (x) => `<span class="muted nowrap" title="${esc(x.last_modified)}">${esc(shortDate(x.last_modified))}</span>`, sort: (x) => Date.parse(x.last_modified) || null },
      ];
      const tb = table(document.getElementById("ftbl"), rows, cols, { sort: "content_length", dir: -1, empty: "No manifest for this payer yet." });
      const apply = () => {
        const q = document.getElementById("fq").value.toLowerCase();
        const t = document.getElementById("ft").value, s = document.getElementById("fs").value;
        tb.update(rows.filter((x) => (!t || x.file_type === t)
          && (!s || (s === "ok" ? !x.check_error : !!x.check_error))
          && (!q || `${x.url} ${x.description} ${x.plan_names}`.toLowerCase().includes(q))));
      };
      ["fq", "ft", "fs"].forEach((i) => document.getElementById(i).oninput = apply);
    },
    plans() {
      const tp = det.toc_plans || {};
      const plans = tp.plans || [];
      tab.innerHTML = `<p class="note">${tp.error ? esc(tp.error) : `${num(plans.length)} distinct plans shown from ${num(tp.plan_refs)} plan references in the TOC${tp.truncated ? ` (first ${num(tp.cap)} distinct plans only)` : ""}.`}</p>
        <div class="toolbar"><input type="search" id="pq" placeholder="Search plan, sponsor, id…">
        <select id="pm"><option value="">All markets</option><option>group</option><option>individual</option></select></div><div id="ptbl"></div>`;
      const cols = [
        { key: "plan_name", label: "Plan" },
        { key: "plan_sponsor_name", label: "Sponsor" },
        { key: "plan_id_type", label: "ID type" },
        { key: "plan_id", label: "Plan ID", render: (x) => `<span class="mono">${esc(x.plan_id)}</span>` },
        { key: "plan_market_type", label: "Market" },
        { key: "in_network_files", label: "In-network files", num: true },
        { key: "allowed_amount_files", label: "Allowed-amount files", num: true },
      ];
      const tb = table(document.getElementById("ptbl"), plans, cols, { sort: "in_network_files", dir: -1, empty: "No TOC stored for this payer." });
      const apply = () => {
        const q = document.getElementById("pq").value.toLowerCase(), m = document.getElementById("pm").value;
        tb.update(plans.filter((x) => (!m || String(x.plan_market_type).toLowerCase() === m)
          && (!q || `${x.plan_name} ${x.plan_sponsor_name} ${x.plan_id} ${x.issuer_name}`.toLowerCase().includes(q))));
      };
      ["pq", "pm"].forEach((i) => document.getElementById(i).oninput = apply);
    },
    errors() {
      const kinds = v.error_kinds || [];
      tab.innerHTML = `<p class="note">Errors against the CMS table-of-contents schema, grouped by kind. Record positions in examples are zero-based.</p><div id="etbl"></div>`;
      table(document.getElementById("etbl"), kinds, [
        { key: "count", label: "Count", num: true, render: (k) => num(k.count) },
        { key: "kind", label: "Error", render: (k) => `<span class="mono">${esc(k.kind)}</span>` },
        { key: "examples", label: "Example records", render: (k) => `<span class="mono muted">${esc((k.examples || []).join(", "))}</span>`, sort: (k) => k.examples?.[0] },
      ], { sort: "count", dir: -1, empty: "No schema errors." });
    },
  };
  document.querySelectorAll(".tabs button").forEach((b) => b.onclick = () => {
    document.querySelectorAll(".tabs button").forEach((x) => x.classList.toggle("active", x === b));
    tabs[b.dataset.t]();
  });
  tabs.files();
}

async function errorsView() {
  const d = await loadData();
  const acc = d.errors.access || [];
  const rec = d.errors.records || [];
  const payerName = Object.fromEntries(d.payers.map((p) => [p.payer_id, p.legal_name]));
  const byCause = {};
  acc.forEach((e) => {
    let cause = e.access_status ? (ACCESS[e.access_status] || e.access_status) : null;
    if (!cause) {
      cause = /robots\.txt (unreachable|unavailable)/.test(e.error) ? ACCESS.robots_unreachable
        : e.error.includes("robots") ? ACCESS.robots_disallow : /cloudflare|captcha|akamai/i.test(e.error) ? ACCESS.waf_blocked
        : e.error === "invalid_url" ? ACCESS.invalid_url : e.error === "no_toc_url" ? ACCESS.no_toc_url
        : /\b[45]\d\d\b/.test(String(e.status) + " " + e.error) ? ACCESS.http_error : "Other";
    }
    e.cause = cause;
    byCause[cause] = (byCause[cause] || 0) + 1;
  });
  app.innerHTML = `
    <h2>Access errors</h2>
    <p class="note">Problems reaching a payer's TOC (payer level) or an individual MRF file (file level).</p>
    <div class="kpis">${Object.entries(byCause).map(([k, v]) => `<div class="kpi"><div class="v">${num(v)}</div><div class="l">${esc(k)}</div></div>`).join("") || `<div class="kpi"><div class="v">0</div><div class="l">Access errors</div></div>`}</div>
    <div class="toolbar"><select id="lv"><option value="">All levels</option><option value="payer">Payer (TOC)</option><option value="mrf">File (MRF)</option></select></div>
    <div id="atbl"></div>
    <h2>Record errors</h2>
    <p class="note">Schema errors inside TOC records, grouped by kind across payers.</p>
    <div id="rtbl"></div>`;
  const acols = [
    { key: "payer_id", label: "Payer", render: (e) => `<a href="#/payer/${encodeURIComponent(e.payer_id)}">${esc(payerName[e.payer_id] || e.payer_id)}</a>` },
    { key: "level", label: "Level", render: (e) => e.level === "payer" ? "Payer (TOC)" : `File (${esc(e.file_type)})` },
    { key: "cause", label: "Cause", render: (e) => `<span class="badge ${e.status === "deferred" ? "b-warn" : "b-bad"}">${esc(e.cause)}</span>${e.status === "deferred" ? ` <span class="muted">retry next run</span>` : ""}` },
    { key: "error", label: "Detail", render: (e) => `<div class="mono">${esc(e.error)}</div>` },
    { key: "url", label: "URL", render: (e) => `<div class="mono clip muted" title="${esc(e.url)}">${esc(e.url)}</div>` },
  ];
  const at = table(document.getElementById("atbl"), acc, acols, { sort: "level", dir: -1, empty: "No access errors." });
  document.getElementById("lv").onchange = (ev) => at.update(acc.filter((e) => !ev.target.value || e.level === ev.target.value));
  table(document.getElementById("rtbl"), rec, [
    { key: "payer_id", label: "Payer", render: (e) => `<a href="#/payer/${encodeURIComponent(e.payer_id)}">${esc(payerName[e.payer_id] || e.payer_id)}</a>` },
    { key: "count", label: "Count", num: true, render: (k) => num(k.count) },
    { key: "kind", label: "Error", render: (k) => `<span class="mono">${esc(k.kind)}</span>` },
    { key: "examples", label: "Example records", render: (k) => `<span class="mono muted">${esc((k.examples || []).slice(0, 3).join(", "))}</span>`, sort: (k) => k.examples?.[0] },
  ], { sort: "count", dir: -1, empty: "No record errors." });
}

async function accessView() {
  const d = await loadData();
  const a = d.access_audit;
  if (!a) { app.innerHTML = `<div class="empty">No access audit yet. It runs with every crawl.</div>`; return; }
  const payerName = Object.fromEntries(d.payers.map((p) => [p.payer_id, p.legal_name]));
  const counts = {};
  a.probes.forEach((p) => counts[p.verdict] = (counts[p.verdict] || 0) + 1);
  const yn = (b) => b == null ? `<span class="muted">?</span>` : b ? `<span class="badge b-ok">allow</span>` : `<span class="badge b-warn">deny</span>`;
  app.innerHTML = `
    <h2>Access audit</h2>
    <p class="note">What a polite automated client (<span class="mono">${esc(a.user_agent)}</span>) sees on each payer host, audited ${esc(a.audited_at)}.
    URLs are fetched only when robots.txt allows it. "Bot wall" means a CAPTCHA or bot challenge (for example Cloudflare) was served instead of the file.</p>
    <div class="kpis">${Object.entries(counts).map(([k, v]) => `<div class="kpi"><div class="v">${num(v)}</div><div class="l">${verdictBadge(k)}</div></div>`).join("")}</div>
    <h3>URLs</h3><div id="ptbl"></div>
    <h3>robots.txt by host</h3><div id="rtbl"></div>`;
  table(document.getElementById("ptbl"), a.probes, [
    { key: "payer_id", label: "Payer", render: (p) => `<a href="#/payer/${encodeURIComponent(p.payer_id)}">${esc(payerName[p.payer_id] || p.payer_id)}</a>` },
    { key: "role", label: "Role" },
    { key: "verdict", label: "Verdict", render: (p) => `${verdictBadge(p.verdict)} ${p.wall ? `<span class="muted">${esc(p.wall)}</span>` : ""}` },
    { key: "status", label: "HTTP", num: true },
    { key: "robots_allows_us", label: "Us", render: (p) => yn(p.robots_allows_us) },
    { key: "star", label: "*", render: (p) => yn(p.robots_allows?.["*"]), sort: (p) => p.robots_allows?.["*"] },
    { key: "google", label: "Googlebot", render: (p) => yn(p.robots_allows?.Googlebot), sort: (p) => p.robots_allows?.Googlebot },
    { key: "server", label: "Server", render: (p) => `<span class="muted">${esc(p.server)}</span>` },
    { key: "url", label: "URL", render: (p) => `<div class="mono clip" title="${esc(p.url)}">${esc(p.url)}</div>` },
  ], { sort: "verdict" });
  table(document.getElementById("rtbl"), a.robots, [
    { key: "origin", label: "Host", render: (r) => `<span class="mono">${esc(r.origin)}</span>` },
    { key: "status", label: "HTTP", num: true },
    { key: "policy", label: "Policy", render: (r) => esc({ rules: "rules", allow_all: "allow all (no robots.txt)", disallow_all: "disallow all (5xx or unreachable)" }[r.policy] || r.policy) },
    { key: "wall", label: "Bot wall", render: (r) => r.wall ? `<span class="badge b-bad">${esc(r.wall)}</span>` : "" },
    { key: "text", label: "robots.txt", render: (r) => r.text ? `<details><summary>${num(r.text.split("\n").length)} lines</summary><pre>${esc(r.text)}</pre></details>` : `<span class="muted">${esc(r.error)}</span>` },
  ], { sort: "origin" });
}

async function requestView() {
  const newIssue = `https://github.com/${REPO}/issues/new?template=payer-request.yml`;
  app.innerHTML = `
    <h2>Request a payer</h2>
    <div class="panel">
      <p>Ask for a health plan, issuer or TPA to be added. Within a few minutes a bot checks the landing page and TOC URL you give
      (robots.txt, bot walls, and a full parse and schema check of the TOC) and replies on the request with the result and a ready-to-paste seed entry.</p>
      <p><a class="btn" href="${newIssue}" target="_blank" rel="noopener">Open a payer request</a></p>
      <p class="note">Requests are GitHub issues on the tic-crawler repository, so you need a GitHub account.</p>
    </div>
    <h3>Open requests</h3><div id="reqs"><div class="empty">Loading…</div></div>`;
  try {
    const issues = await getJSON(`https://api.github.com/repos/${REPO}/issues?labels=payer-request&state=all&per_page=50`);
    const el = document.getElementById("reqs");
    table(el, issues, [
      { key: "number", label: "#", num: true, render: (i) => `<a href="${esc(i.html_url)}" target="_blank" rel="noopener">${i.number}</a>` },
      { key: "title", label: "Request", render: (i) => esc(i.title.replace(/^Payer request:\s*/, "")) },
      { key: "state", label: "State", render: (i) => `<span class="badge ${i.state === "open" ? "b-warn" : "b-ok"}">${esc(i.state)}</span>` },
      { key: "comments", label: "Replies", num: true },
      { key: "created_at", label: "Opened", render: (i) => esc(i.created_at.slice(0, 10)) },
    ], { sort: "number", dir: -1, empty: "No requests yet." });
  } catch (e) {
    document.getElementById("reqs").innerHTML = `<div class="empty">Could not load requests (${esc(e.message)}).</div>`;
  }
}

// ---------- router ----------
async function route() {
  const h = location.hash.replace(/^#\/?/, "");
  const [view, arg] = h.split("/");
  const name = view || "overview";
  document.querySelectorAll("#nav a").forEach((a) => a.classList.toggle("active", a.dataset.route === (name === "payer" ? "overview" : name)));
  app.innerHTML = `<div class="empty">Loading…</div>`;
  try {
    if (name === "payer" && arg) await payerView(decodeURIComponent(arg));
    else if (name === "errors") await errorsView();
    else if (name === "access") await accessView();
    else if (name === "request") await requestView();
    else await overview();
  } catch (e) {
    app.innerHTML = `<div class="empty">Could not load data: ${esc(e.message)}.<br>The first crawl may not have published yet.</div>`;
  }
  window.scrollTo(0, 0);
}
window.addEventListener("hashchange", route);
route();
