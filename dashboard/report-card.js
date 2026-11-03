// Hospital price report card: negotiated facility rates as multiples of Medicare.
// Reads data/report_card/index.json and data/report_card/<payer>.json (written by
// `tic-crawler export-dashboard` from `report-card` output). Loaded before app.js and uses its
// helpers (esc, num, getJSON, table, DATA); app.js routes #/report-card here.
// A price benchmark only: it says nothing about quality.

const RC = { index: null, cards: {} };
const times = (m) => (m == null ? "" : `${m.toFixed(m >= 10 ? 0 : 1)}×`);
const usd = (n) => (n == null ? "" : `$${Number(n).toLocaleString("en-US", { maximumFractionDigits: n >= 100 ? 0 : 2 })}`);
const codeOnly = (k) => String(k).replace(/^HCPCS:/, "");
// Sequential shade for a multiple: 1x Medicare is pale, 5x and up is the darkest step.
function heat(m) {
  if (m == null) return "";
  const pct = Math.round(Math.min(1, Math.max(0, Math.log(m / 0.5) / Math.log(10))) * 85);
  return `background: color-mix(in srgb, var(--bar) ${pct}%, transparent)`;
}
function fileLabel(f) {
  const names = (f.plan_names || []).filter(Boolean);
  const head = names.length ? names[0] : `File ${f.file_id}`;
  const more = f.plan_count > 1 ? ` (+${num(f.plan_count - 1)} plans)` : "";
  return `${head}${more}`;
}

async function reportCardView(arg) {
  if (!RC.index) RC.index = await getJSON(`${DATA}/report_card/index.json`).catch(() => ({ cards: [] }));
  const cards = RC.index.cards || [];
  if (!cards.length) {
    app.innerHTML = `<h2>Hospital price report card</h2><div class="empty">No report card yet. It is built by the price pilot
      (<span class="mono">extract-rates</span>, <span class="mono">join-quality</span>, <span class="mono">report-card</span>).</div>`;
    return;
  }
  const id = arg && cards.some((c) => c.payer_id === arg) ? arg : cards[0].payer_id;
  if (!RC.cards[id]) RC.cards[id] = await getJSON(`${DATA}/report_card/${encodeURIComponent(id)}.json`);
  const c = RC.cards[id];
  const codes = c.codes || [];
  const used = new Set((c.hospitals || []).flatMap((h) => Object.values(h.cells).flatMap((x) => Object.keys(x.by_file || {}))));
  const files = (c.files || []).filter((f) => used.has(f.file_id));
  loadData().catch(() => {}); // fills the header's "data generated" line
  const excluded = c.excluded_rows || {};
  const exTotal = Object.values(excluded).reduce((a, b) => a + b, 0);
  app.innerHTML = `
    <h2>Hospital price report card</h2>
    <p class="note">What <b>${esc(c.payer_name || id)}</b> has agreed to pay each hospital for outpatient services, as a multiple of what
    Medicare pays for the same service. <b>2×</b> means twice Medicare. This is a price comparison only, not a quality rating.</p>
    <div class="toolbar">
      <label>Payer <select id="rc-payer">${cards.map((x) => `<option value="${esc(x.payer_id)}"${x.payer_id === id ? " selected" : ""}>${esc(x.payer_name || x.payer_id)}</option>`).join("")}</select></label>
      <label>Plans <select id="rc-file"><option value="">All published files</option>${files.map((f) => `<option value="${esc(f.file_id)}">${esc(fileLabel(f))}</option>`).join("")}</select></label>
      <input type="search" id="rc-q" placeholder="Search hospital or town…" aria-label="Search hospitals">
    </div>
    <div class="kpis">
      <div class="kpi"><div class="v">${num((c.hospitals || []).length)}</div><div class="l">Hospitals with a price</div></div>
      <div class="kpi"><div class="v">${num(codes.length)}</div><div class="l">Services with a Medicare benchmark</div></div>
      <div class="kpi"><div class="v">${num(files.length)}</div><div class="l">Rate files with hospital prices</div></div>
      <div class="kpi"><div class="v">${num(exTotal)}</div><div class="l">Rate rows left out</div></div>
    </div>
    <h3>By service</h3>
    <div id="rc-codes"></div>
    <h3>By hospital</h3>
    <p class="note">Each cell is the hospital's median negotiated facility rate divided by Medicare's payment. Click a cell for the dollar amounts.
    Darker means a higher multiple. A blank cell means no usable rate was found, which is not the same as "not covered".</p>
    <div id="rc-detail"></div>
    <div id="rc-grid"></div>
    <details class="panel"><summary><b>How this is calculated, and what it leaves out</b></summary>
      <ul>
        <li><b>Rates</b>: the payer's own in-network rate files (Transparency in Coverage), facility (institutional) fees only.
          Doctors' fees are billed separately and are not included, so the full cost of a visit is higher.</li>
        <li><b>Medicare</b>: ${esc((c.medicare_releases || []).join("; "))}. Hospital outpatient payments use the OPPS national rate,
          and labs use the Clinical Laboratory Fee Schedule. Office visits are compared with the hospital clinic visit (G0463).
          Medicare amounts are national and not adjusted for local wages, which run above average in Massachusetts,
          so multiples there are somewhat overstated.</li>
        <li><b>Counted once per tax ID</b>: a rate shared by many doctors in one group counts once, so large groups don't dominate.</li>
        <li><b>Left out</b>: ${Object.entries(excluded).map(([k, v]) => `${num(v)} ${esc({ not_dollar: "percentage-of-charges or per diem rows", outlier_vs_medicare: `rates under ${c.rules?.min_multiple}× or over ${c.rules?.max_multiple}× Medicare (almost always data errors)`, placeholder: "placeholder rates (0 or 999999)", expired: "expired rates", non_ffs: "bundled or capitated rates" }[k] || k)}`).join("; ") || "nothing"}.</li>
        <li><b>No Medicare benchmark</b>: ${esc((c.codes_without_benchmark || []).map(codeOnly).join(", ") || "none")}. These are inpatient stays (MS-DRG),
          services Medicare doesn't pay hospitals for separately, or services Medicare doesn't cover.</li>
        <li>Hospitals are matched to rate files through NPI records at high or medium confidence. Generated ${esc((c.generated_at || "").slice(0, 10))}.</li>
      </ul>
    </details>`;

  const codeLabel = Object.fromEntries(codes.map((k) => [k.code, k.label || codeOnly(k.code)]));
  table(document.getElementById("rc-codes"), codes, [
    { key: "label", label: "Service", render: (k) => `${esc(k.label || codeOnly(k.code))} <span class="muted mono">${esc(codeOnly(k.code))}</span>` },
    { key: "medicare_amount", label: "Medicare pays", num: true, render: (k) => `${usd(k.medicare_amount)}${k.medicare_code !== codeOnly(k.code) ? ` <span class="muted mono" title="Medicare benchmark code">${esc(k.medicare_code)}</span>` : ""}` },
    { key: "hospitals", label: "Hospitals", num: true },
    { key: "median_multiple", label: "Median", num: true, render: (k) => `<b>${times(k.median_multiple)}</b>` },
    { key: "max_multiple", label: "Range across hospitals", render: (k) => `${times(k.min_multiple)} to ${times(k.max_multiple)}`, sort: (k) => k.max_multiple },
  ], { sort: "label", pageSize: 50 });

  const hospitals = c.hospitals || [];
  let file = "";
  const value = (h, code) => {
    const cell = h.cells[code];
    if (!cell) return null;
    return file ? cell.by_file?.[file] ?? null : cell.multiple;
  };
  const cols = [
    { key: "name", label: "Hospital", render: (h) => `${esc(h.name)}<div class="muted">${esc([h.city, h.state].filter(Boolean).join(", "))}</div>`
      + (h.flat_codes ? `<span class="badge b-warn" title="The same rate is listed for ${h.flat_codes} different services, which usually means a default or case rate rather than a price for each service. Check before relying on these cells.">Same ${usd(h.flat_rate)} for ${h.flat_codes} services</span>` : "") },
    ...codes.map((k) => ({
      key: k.code, label: codeOnly(k.code), num: true, sort: (h) => value(h, k.code),
      render: (h) => {
        const m = value(h, k.code);
        return m == null ? "" : `<button class="rc-cell" style="${heat(m)}" data-h="${esc(h.ccn)}" data-c="${esc(k.code)}" title="${esc(codeLabel[k.code])}">${times(m)}</button>`;
      },
    })),
  ];
  const grid = table(document.getElementById("rc-grid"), hospitals, cols, { sort: "name", pageSize: 100, empty: "No hospital matches." });
  const detail = document.getElementById("rc-detail");
  document.getElementById("rc-grid").addEventListener("click", (e) => {
    const b = e.target.closest(".rc-cell");
    if (!b) return;
    const h = hospitals.find((x) => x.ccn === b.dataset.h);
    const k = codes.find((x) => x.code === b.dataset.c);
    const cell = h.cells[k.code];
    const perFile = Object.entries(cell.by_file || {}).map(([fid, m]) => {
      const f = files.find((x) => x.file_id === fid) || { file_id: fid };
      return `<li>${esc(fileLabel(f))}: <b>${times(m)}</b></li>`;
    }).join("");
    detail.innerHTML = `<div class="panel"><b>${esc(h.name)}</b> · ${esc(k.label || codeOnly(k.code))}
      <p>Median negotiated facility rate <b>${usd(cell.median)}</b> (${usd(cell.min)} to ${usd(cell.max)} across ${num(cell.rates)} distinct
      ${cell.rates === 1 ? "rate" : "rates"}). Medicare pays ${usd(k.medicare_amount)}, so the median is <b>${times(cell.multiple)}</b> Medicare.
      ${cell.outliers ? `${num(cell.outliers)} implausible ${cell.outliers === 1 ? "rate was" : "rates were"} left out.` : ""}</p>
      ${perFile ? `<p class="muted">By rate file (different plans can pay different prices at the same hospital):</p><ul>${perFile}</ul>` : ""}
      ${h.flat_codes ? `<p><span class="badge b-warn">Check</span> This hospital lists the same ${usd(h.flat_rate)} for ${h.flat_codes} different services, which usually means a default or case rate.</p>` : ""}
      <p class="note">This is the facility fee only. The doctor's fee and any other services are billed separately.</p></div>`;
    detail.scrollIntoView({ behavior: "smooth", block: "nearest" });
  });
  const apply = () => {
    const q = document.getElementById("rc-q").value.toLowerCase();
    file = document.getElementById("rc-file").value;
    grid.update(hospitals.filter((h) => (!q || `${h.name} ${h.city} ${h.state}`.toLowerCase().includes(q))
      && (!file || Object.values(h.cells).some((x) => x.by_file?.[file] != null))));
  };
  document.getElementById("rc-q").oninput = apply;
  document.getElementById("rc-file").onchange = apply;
  document.getElementById("rc-payer").onchange = (e) => (location.hash = `#/report-card/${encodeURIComponent(e.target.value)}`);
}
