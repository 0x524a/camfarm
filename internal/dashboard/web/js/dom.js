// el builds an element. Children that are strings become text nodes, so server
// text can never be parsed as HTML. false/null/undefined props are skipped.
export function el(tag, props = {}, children = []) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "text") n.textContent = v;
    else if (k === "class") n.className = v;
    else if (k === "dataset") Object.assign(n.dataset, v);
    else if (k.startsWith("on") && typeof v === "function") n.addEventListener(k.slice(2), v);
    else n.setAttribute(k, v === true ? "" : String(v));
  }
  for (const c of [].concat(children)) if (c !== null && c !== undefined && c !== false) n.append(c);
  return n;
}

export const DASH = "-";

export function fmtNum(v, digits = 1) {
  return typeof v === "number" && Number.isFinite(v) ? v.toFixed(digits) : DASH;
}

export function fmtPct(v, digits = 0) {
  return typeof v === "number" && Number.isFinite(v) ? v.toFixed(digits) + "%" : DASH;
}

export function fmtBytes(n) {
  if (typeof n !== "number" || !Number.isFinite(n) || n < 0) return DASH;
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n.toFixed(0) : n.toFixed(1)) + " " + units[i];
}

// sparkline draws values (0..max) as a small area chart; null values are skipped.
export function sparkline(values, max) {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", "0 0 100 26");
  svg.setAttribute("preserveAspectRatio", "none");
  svg.setAttribute("aria-hidden", "true");
  const pts = values.filter((v) => typeof v === "number" && Number.isFinite(v));
  if (pts.length < 2) return svg;
  const h = 26;
  const xy = pts.map((v, i) => [
    (i / (pts.length - 1)) * 100,
    h - 2 - (Math.min(Math.max(v, 0), max) / max) * (h - 5),
  ]);
  const line = xy.map((p) => p[0].toFixed(1) + "," + p[1].toFixed(1)).join(" ");
  const area = document.createElementNS(ns, "polyline");
  area.setAttribute("points", line + " 100,26 0,26");
  area.setAttribute("class", "spark-area");
  const stroke = document.createElementNS(ns, "polyline");
  stroke.setAttribute("points", line);
  stroke.setAttribute("class", "spark-line");
  stroke.setAttribute("vector-effect", "non-scaling-stroke");
  svg.append(area, stroke);
  return svg;
}

// copyText copies text and resolves true on success. It falls back to a hidden
// textarea where the Clipboard API is missing or refused.
export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch (_) { /* fall through */ }
  const ta = el("textarea", { "aria-hidden": "true", style: "position:fixed;opacity:0" });
  ta.value = text;
  document.body.append(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand("copy"); } catch (_) { ok = false; }
  ta.remove();
  return ok;
}
