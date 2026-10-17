import { api } from "./api.js";
import { createStore } from "./state.js";
import { mount as mountCameras } from "./cameras.js";
import { mount as mountFaults } from "./faults.js";
import { mount as mountLibrary } from "./library.js";
import { mount as mountMetrics } from "./metrics.js";

const store = createStore({
  cameras: [], catalogue: [], library: null, metrics: null,
  conn: "connecting", error: "", quality: {}, qualityEvent: null,
  addRequest: null, faultsFor: null,
});

// fail shows an action error. Background failures go to the connection pill and
// never touch this region.
function fail(e) {
  store.set({ error: e && e.message ? e.message : String(e) });
}
window.__camfarmFail = fail; // lets tests raise an action error without a server round trip

const ctx = { store, api, fail };
const $ = (s) => document.querySelector(s);

// --- connection pill and error region ---
const CONN = {
  connecting: ["connecting…", ""],
  ok: ["connected", "ok"],
  lost: ["disconnected, retrying", "bad"],
};
store.subscribe((st, patch) => {
  if ("conn" in patch) {
    const [text, cls] = CONN[st.conn];
    const pill = $("#conn");
    pill.textContent = text;
    pill.className = "pill" + (cls ? " " + cls : "");
  }
  if ("error" in patch) $("#error").textContent = st.error;
});

// --- tabs ---
const tabs = [...document.querySelectorAll("button.tab")];
function selectTab(name, focus) {
  for (const t of tabs) {
    const on = t.dataset.tab === name;
    t.setAttribute("aria-selected", String(on));
    t.tabIndex = on ? 0 : -1;
    $("#panel-" + t.dataset.tab).hidden = !on;
    if (on && focus) t.focus();
  }
  try { history.replaceState(null, "", "#" + name); } catch (_) { /* ignore */ }
}
tabs.forEach((t, i) => {
  t.addEventListener("click", () => selectTab(t.dataset.tab, false));
  t.addEventListener("keydown", (e) => {
    const move = { ArrowRight: 1, ArrowLeft: -1 }[e.key];
    if (move === undefined) return;
    e.preventDefault();
    selectTab(tabs[(i + move + tabs.length) % tabs.length].dataset.tab, true);
  });
});
const initial = location.hash.slice(1);
selectTab(tabs.some((t) => t.dataset.tab === initial) ? initial : "cameras", false);

// --- data refresh ---
let seq = 0, applied = 0;
async function refreshCameras() {
  const my = ++seq;
  try {
    const cams = await api.cameras();
    if (my < applied) return; // an older response must not overwrite a newer one
    applied = my;
    store.set({ cameras: cams, conn: "ok" });
  } catch (_) {
    if (my >= applied) store.set({ conn: "lost" }); // an older failure must not undo a newer success
  }
}
async function refreshReplay() {
  try {
    const f = await api.fleet();
    $("#replay").textContent = f.replayLine;
  } catch (_) { /* the pill reports connectivity */ }
}
let timer = 0;
function refreshSoon() {
  clearTimeout(timer);
  timer = setTimeout(refreshCameras, 50);
}

// --- feature modules (each task adds its line) ---
mountCameras($("#panel-cameras"), ctx);
mountFaults($("#drawer"), ctx);
mountLibrary($("#panel-library"), ctx);
mountMetrics($("#metrics"), ctx);

// --- event stream ---
if (window.EventSource) {
  const es = new EventSource("/api/v1/events?metrics=1");
  ["camera_added", "camera_removed", "fault_injected", "fault_cleared"].forEach((t) => es.addEventListener(t, refreshSoon));
  es.addEventListener("metrics", (e) => store.set({ metrics: JSON.parse(e.data) }));
  es.addEventListener("quality", (e) => store.set({ qualityEvent: JSON.parse(e.data) }));
  es.onopen = () => {
    store.set({ conn: "ok" });
    refreshCameras();
    refreshReplay();
  };
  es.onerror = () => store.set({ conn: "lost" });
} else {
  store.set({ conn: "lost" });
}
refreshCameras();
refreshReplay();
