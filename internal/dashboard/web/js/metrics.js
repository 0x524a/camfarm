import { el, fmtBytes, fmtNum, fmtPct, sparkline, DASH } from "./dom.js";

const HISTORY = 40;

export function mount(root, ctx) {
  const { store, api } = ctx;
  const hist = new Map(); // tile key -> number[]
  let enabled = true;

  function push(key, v) {
    const h = hist.get(key) || [];
    h.push(typeof v === "number" && Number.isFinite(v) ? v : null);
    while (h.length > HISTORY) h.shift();
    hist.set(key, h);
    return h;
  }

  // tile builds one tile. value/unit/note are text; v feeds the sparkline.
  function tile(key, title, head, value, unit, note, v, max) {
    const h = push(key, v);
    return el("div", { class: "tile", dataset: { tile: key } }, [
      el("div", { class: "tile-head" }, [el("span", { text: title }), el("span", { text: head })]),
      el("div", { class: "tile-value" }, [value, unit ? el("small", { text: " " + unit }) : null]),
      sparkline(h, max),
      note ? el("div", { class: "tile-note", text: note }) : null,
    ]);
  }

  function unavailable(key, title, reason) {
    return el("div", { class: "tile", dataset: { tile: key } }, [
      el("div", { class: "tile-head" }, [el("span", { text: title }), el("span", { class: "pill bad", text: "unavailable" })]),
      el("div", { class: "tile-reason", text: reason || "No reason given." }),
    ]);
  }

  function render(s) {
    const tiles = [];
    const c = s.cpu;
    tiles.push(c.available
      ? tile("cpu", "CPU", c.logicalCPUs ? c.logicalCPUs + " logical" : "", fmtPct(c.usagePercent, 0), "",
        "load " + fmtNum(c.load1, 2) + (c.limitCPUs ? " · limit " + fmtNum(c.limitCPUs, 1) + " CPUs" : ""), c.usagePercent, 100)
      : unavailable("cpu", "CPU", c.reason));

    const m = s.memory;
    tiles.push(m.available
      ? tile("memory", "Memory", "of " + fmtBytes(m.totalBytes), fmtBytes(m.usedBytes), "",
        "swap " + fmtBytes(m.swapUsedBytes) + (m.limitBytes ? " · limit " + fmtBytes(m.limitBytes) : ""),
        m.totalBytes ? (m.usedBytes / m.totalBytes) * 100 : null, 100)
      : unavailable("memory", "Memory", m.reason));

    const g = s.gpu;
    if (g.available && (g.devices || []).length) {
      for (const d of g.devices) {
        const pre = g.devices.length > 1 ? "GPU " + d.index + " " : "GPU ";
        tiles.push(tile("gpu-" + d.index, pre + "busy", d.name || "", fmtPct(d.gpuUtilPercent, 0), "",
          "kernel-busy time · enc " + fmtPct(d.encoderUtilPercent, 0) + " · dec " + fmtPct(d.decoderUtilPercent, 0), d.gpuUtilPercent, 100));
        const mt = d.memTotalBytes, mu = d.memUsedBytes;
        tiles.push(tile("gpu-mem-" + d.index, pre + "memory", mt != null ? "of " + fmtBytes(mt) : DASH, mu != null ? fmtBytes(mu) : DASH, "",
          (d.temperatureC != null ? d.temperatureC + " °C" : DASH) + " · " + (d.powerWatts != null ? fmtNum(d.powerWatts, 0) + " W" : DASH),
          mt && mu != null ? (mu / mt) * 100 : null, 100));
      }
    } else {
      tiles.push(unavailable("gpu", "GPU", g.reason || "No GPU found."));
    }

    const p = s.process;
    tiles.push(p.available
      ? tile("process", "camfarm process", fmtBytes(p.rssBytes) + " RSS", fmtPct(p.cpuPercent, 1), "CPU",
        p.goroutines + " goroutines · " + p.openFDs + " open files", p.cpuPercent, 100)
      : unavailable("process", "camfarm process", p.reason));

    root.replaceChildren(...tiles);
  }

  store.subscribe((st, patch) => {
    if ("metrics" in patch && st.metrics && enabled) render(st.metrics);
    if ("conn" in patch) root.classList.toggle("stale", st.conn === "lost");
  });

  // First paint: a sample from the endpoint, so the strip is never empty while
  // the stream connects. A 404 means the server has no sampler.
  api.metrics().then((s) => { if (!store.get().metrics) store.set({ metrics: s }); }).catch((e) => {
    if (e.status === 404) {
      enabled = false;
      root.replaceChildren(el("p", { class: "sub", text: "Host metrics are not enabled on this server." }));
    }
  });
}
