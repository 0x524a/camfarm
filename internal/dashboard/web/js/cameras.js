import { el, copyText } from "./dom.js";

// faultText renders one active fault as "kind key=value ..." for a chip.
function faultText(f) {
  const parts = [f.kind];
  for (const [k, v] of Object.entries(f)) {
    if (k === "kind" || v === false || v === 0 || v === "" || v === null) continue;
    parts.push(k + "=" + v);
  }
  return parts.join(" ");
}

function urlLine(label, value, ctx) {
  const copy = el("button", { type: "button", class: "ghost sm", text: "Copy", "aria-label": "Copy " + label });
  copy.addEventListener("click", async () => {
    const ok = await copyText(value);
    if (!ok) ctx.fail(new Error("Copy failed; select the text manually."));
    else {
      copy.textContent = "Copied";
      setTimeout(() => { copy.textContent = "Copy"; }, 1200);
    }
  });
  return el("div", { class: "url" }, [
    el("span", { class: "sub", text: label }),
    el("code", { text: value, title: value }),
    copy,
  ]);
}

export function mount(root, ctx) {
  const { store, api, fail } = ctx;
  const grid = root.querySelector("#cam-grid");
  const empty = root.querySelector("#cam-empty");
  const cards = new Map(); // id -> {el, update(cam), destroy()}
  const blobs = new Map(); // id -> object URL currently shown

  // Quality entries are keyed by camera id, and ids such as "constructor" are
  // legal, so every lookup is an own-property lookup.
  const qOf = (id) => {
    const q = store.get().quality;
    return Object.hasOwn(q, id) ? q[id] : undefined;
  };
  const setQ = (id, run) => store.set({ quality: { ...store.get().quality, [id]: run } });
  const dropQ = (id) => {
    if (!Object.hasOwn(store.get().quality, id)) return;
    const rest = { ...store.get().quality };
    delete rest[id];
    store.set({ quality: rest });
  };

  // Snapshots load one at a time: each costs the server a frame decode.
  let chain = Promise.resolve();
  function loadSnap(id) {
    chain = chain.then(() => doLoad(id)).catch(() => {});
    return chain;
  }
  async function doLoad(id) {
    const card = cards.get(id);
    if (!card) return;
    try {
      const blob = await api.snapshot(id);
      if (!cards.has(id)) return;
      const url = URL.createObjectURL(blob);
      const old = blobs.get(id);
      blobs.set(id, url);
      card.showSnap(url);
      if (old) URL.revokeObjectURL(old);
    } catch (e) {
      if (cards.has(id)) card.showSnapError(e.message || "Snapshot failed");
    }
  }

  // One probe decides whether the server runs quality tests: "not enabled" is a
  // 404 whose error text says so; "no result yet" is a 404 with other text. The
  // in-flight promise is cached so N cards cause a single request.
  let qualityProbe = null;
  function probeQuality(id) {
    if (!qualityProbe) {
      qualityProbe = api.getQuality(id).then(
        () => true,
        (e) => !/not enabled/i.test((e && e.message) || ""),
      );
    }
    return qualityProbe;
  }

  function buildCard(cam) {
    const id = cam.id;
    const img = el("img", { class: "snap", alt: "Snapshot of " + id, hidden: true });
    const ph = el("div", { class: "snap-ph", text: "loading snapshot…" });
    const title = el("h3", { text: id });
    const source = el("div", { class: "sub mono" });
    const urls = el("div", { class: "urls" });
    const chips = el("div", { class: "chips" });
    const qbox = el("div", { class: "qbox" });
    const snapBtn = el("button", { type: "button", class: "ghost sm btn-snap", text: "Refresh snapshot" });
    const faultsBtn = el("button", { type: "button", class: "ghost sm btn-faults", text: "Faults" });
    faultsBtn.addEventListener("click", () => store.set({ faultsFor: id }));
    const removeBtn = el("button", { type: "button", class: "danger sm btn-remove", text: "Remove" });
    const confirmBtn = el("button", { type: "button", class: "danger sm btn-remove-confirm", text: "Confirm remove", hidden: true });
    const cancelBtn = el("button", { type: "button", class: "ghost sm btn-remove-cancel", text: "Cancel", hidden: true });
    const prompt = el("span", { class: "rm-prompt", text: "Remove " + id + "?", hidden: true });
    const actions = el("div", { class: "row actions" }, [snapBtn, faultsBtn, el("span", { class: "grow" }), prompt, removeBtn, confirmBtn, cancelBtn]);
    const root = el("article", { class: "cam", dataset: { id } }, [
      el("div", { class: "thumb" }, [img, ph]),
      el("div", { class: "cam-body" }, [title, source, urls, chips, qbox, actions]),
    ]);

    snapBtn.addEventListener("click", () => {
      ph.textContent = "loading snapshot…";
      loadSnap(id);
    });
    const askRemove = (on) => {
      removeBtn.hidden = on;
      prompt.hidden = !on;
      confirmBtn.hidden = !on;
      cancelBtn.hidden = !on;
    };
    removeBtn.addEventListener("click", () => askRemove(true));
    cancelBtn.addEventListener("click", () => askRemove(false));
    confirmBtn.addEventListener("click", async () => {
      confirmBtn.disabled = true;
      try {
        await api.removeCamera(id);
        const left = store.get().cameras.filter((c) => c.id !== id);
        store.set({ cameras: left });
      } catch (e) {
        fail(e);
        confirmBtn.disabled = false;
        askRemove(false);
      }
    });

    const card = {
      el: root,
      actions,
      qbox,
      showSnap(url) {
        img.src = url;
        img.hidden = false;
        ph.hidden = true;
      },
      showSnapError(msg) {
        img.hidden = true;
        ph.hidden = false;
        ph.textContent = "Snapshot failed: " + msg;
      },
      update(c) {
        source.textContent = [c.libraryId ? "library file" : c.source, c.codec, c.width && c.height ? c.width + "×" + c.height : ""]
          .filter(Boolean).join(" · ");
        urls.replaceChildren(urlLine("RTSP", c.rtspUrl, ctx), urlLine("ONVIF", c.onvifEndpoint, ctx));
        const fs = c.faults || [];
        chips.replaceChildren(...(fs.length
          ? fs.map((f) => el("span", { class: "pill warn chip", text: faultText(f) }))
          : [el("span", { class: "pill chip", text: "no faults" })]));
        if (c.libraryId && (c.warnings || []).length) {
          chips.append(...c.warnings.map((w) => el("span", { class: "pill warn chip", text: w.message, title: w.detail || "" })));
        }
      },
      destroy() {
        clearInterval(ticker);
        dropQ(id); // a camera re-added under this id must not inherit the old run
        const url = blobs.get(id);
        if (url) URL.revokeObjectURL(url);
        blobs.delete(id);
      },
    };
    card.update(cam);

    // Stream quality. The buttons join the actions row only once the probe says
    // the server supports it; otherwise they never exist in the DOM.
    const qBtn = el("button", { type: "button", class: "sm btn-quality", text: "Test stream quality" });
    const qCancel = el("button", { type: "button", class: "ghost sm btn-quality-cancel", text: "Cancel test", hidden: true });
    let ticker = 0;

    function qRender() {
      const run = qOf(id);
      clearInterval(ticker);
      qbox.replaceChildren();
      qBtn.disabled = false;
      qCancel.hidden = true;
      if (!run) return;
      const head = (label, cls) => el("div", { class: "row" }, [
        el("strong", { text: "Stream quality" }), el("span", { class: "grow" }), el("span", { class: "pill " + cls, text: label }),
      ]);
      if (run.state === "running") {
        qBtn.disabled = true;
        qCancel.hidden = false;
        const fill = el("div", { class: "bar-fill" });
        qbox.append(el("div", { class: "qres" }, [head("testing…", "run"), el("div", { class: "bar-track" }, [fill])]));
        const total = ((run.request && run.request.durationSeconds) || 5) * 1000;
        const started = Date.parse(run.startedAt) || Date.now();
        const tick = () => { fill.style.width = Math.min(100, ((Date.now() - started) / total) * 100) + "%"; };
        tick();
        ticker = setInterval(tick, 250);
        return;
      }
      if (run.state === "failed") {
        qbox.append(el("div", { class: "qres failed" }, [head("failed", "bad"), el("div", { text: run.error || "The test failed." })]));
        return;
      }
      const a = run.analysis || { tracks: [], findings: [] };
      const v = (a.tracks || []).find((t) => t.type === "video") || (a.tracks || [])[0] || {};
      const fr = v.frames || {};
      const num = (x, d, unit) => (typeof x === "number" && Number.isFinite(x) ? x.toFixed(d) + unit : "-");
      const verdict = { ok: ["healthy", "ok"], warn: ["warnings", "warn"], error: ["errors", "bad"] }[run.verdict] || ["done", ""];
      const dl = el("dl", {}, [
        ["Frame rate", num(fr.fps, 1, " fps")],
        ["Bitrate", num(v.bitrate_kbps, 0, " kb/s")],
        ["Jitter", num(v.jitter_ms, 1, " ms")],
        ["Packet loss", num(v.loss_percent, 2, " %")],
        ["Keyframe every", fr.keyframe_interval_ms ? num(fr.keyframe_interval_ms / 1000, 1, " s") : "-"],
        ["Frames", typeof fr.frames === "number" ? String(fr.frames) : "-"],
      ].map(([k, val]) => el("div", {}, [el("dt", { text: k }), el("dd", { text: val })])));
      const findings = (a.findings || []).filter((f) => f.severity !== "info");
      qbox.append(el("div", { class: "qres done" }, [
        head(verdict[0], verdict[1]),
        dl,
        findings.length ? el("ul", {}, findings.map((f) => el("li", { text: f.message }))) : null,
      ]));
    }
    card.qRender = qRender;

    // qFetch reads the server's latest run into the store. One fetch runs at a
    // time per card; a request that arrives meanwhile triggers a follow-up, so
    // the last state seen is never lost. Failures are background noise.
    let fetching = false, again = false, localStart = 0;
    function qFetch() {
      if (fetching) { again = true; return; }
      fetching = true;
      api.getQuality(id).then((run) => {
        if (cards.get(id) === card) setQ(id, run);
      }).catch(() => {}).finally(() => {
        fetching = false;
        if (again) { again = false; if (cards.get(id) === card) qFetch(); }
      });
    }
    card.qFetch = qFetch;
    // qSync reconciles the camera's summary with the store entry on every render.
    card.qSync = (c) => {
      const sum = c.quality, entry = qOf(id);
      if (!sum) {
        // A run started here moments ago may not be in the camera list yet.
        const fresh = entry && entry.state === "running" && Date.now() - localStart < 3000;
        if (entry && !fresh) dropQ(id);
      } else if (!entry || entry.state !== sum.state) {
        qFetch();
      }
    };

    qBtn.addEventListener("click", async () => {
      qBtn.disabled = true;
      localStart = Date.now();
      try {
        const run = await api.startQuality(id, {});
        setQ(id, run);
      } catch (e) {
        fail(e);
        qBtn.disabled = false;
      }
    });
    qCancel.addEventListener("click", async () => {
      qCancel.disabled = true;
      try {
        await api.cancelQuality(id);
        const run = await api.getQuality(id);
        setQ(id, run);
      } catch (e) {
        fail(e);
      } finally {
        qCancel.disabled = false;
      }
    });
    probeQuality(id).then((on) => {
      if (!on) return;
      const spacer = actions.querySelector(".grow");
      actions.insertBefore(qBtn, spacer);
      actions.insertBefore(qCancel, spacer);
    });
    qRender();
    return card;
  }

  function render(cameras) {
    const seen = new Set();
    for (const cam of cameras) {
      seen.add(cam.id);
      let card = cards.get(cam.id);
      if (!card) {
        card = buildCard(cam);
        cards.set(cam.id, card);
        grid.append(card.el);
        loadSnap(cam.id);
      } else {
        card.update(cam);
      }
      card.qSync(cam);
    }
    for (const [id, card] of cards) {
      if (!seen.has(id)) {
        card.destroy();
        card.el.remove();
        cards.delete(id);
      }
    }
    empty.hidden = cameras.length > 0;
    document.getElementById("count-cameras").textContent = String(cameras.length);
  }

  // --- add form ---
  const form = root.querySelector("#add-form");
  const src = root.querySelector("#add-source");
  const libSel = root.querySelector("#add-lib");
  const libLabel = root.querySelector("#add-lib-label");
  const pathLabel = root.querySelector("#add-path-label");
  const idInput = root.querySelector("#add-id");

  function fillLibrary() {
    const lib = store.get().library;
    const usable = lib ? lib.files.filter((f) => f.report.status !== "invalid") : [];
    const opt = src.querySelector('option[value="library"]');
    opt.disabled = usable.length === 0;
    if (opt.disabled && src.value === "library") src.value = "fixture";
    libSel.replaceChildren(...usable.map((f) => el("option", {
      value: f.id,
      text: f.name + (f.report.status === "warning" ? "  (warnings)" : ""),
    })));
    return usable;
  }
  function syncSource() {
    libLabel.hidden = src.value !== "library";
    pathLabel.hidden = src.value !== "file";
  }
  src.addEventListener("change", syncSource);

  async function openForm(libraryId) {
    if (!store.get().library) {
      try { store.set({ library: await api.library() }); } catch (_) { /* library may be disabled */ }
    }
    const usable = fillLibrary();
    if (libraryId) {
      if (usable.some((f) => f.id === libraryId)) {
        src.value = "library";
        libSel.value = libraryId;
      } else {
        fail(new Error("That file is not available for a camera."));
      }
    }
    syncSource();
    form.hidden = false;
    idInput.focus();
  }
  root.querySelector("#add-open").addEventListener("click", () => openForm());
  root.querySelector("#add-cancel").addEventListener("click", () => { form.hidden = true; });
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    if (src.value === "library" && !libSel.value) {
      fail(new Error("Choose a library file."));
      return;
    }
    const spec = { id: idInput.value.trim() };
    const user = root.querySelector("#add-user").value;
    const pass = root.querySelector("#add-pass").value;
    if (user || pass) spec.auth = { username: user, password: pass };
    if (src.value === "library") spec.libraryId = libSel.value;
    else if (src.value === "file") spec.source = { kind: "file", path: root.querySelector("#add-path").value };
    const submit = root.querySelector("#add-submit");
    submit.disabled = true;
    try {
      await api.addCamera(spec);
      form.hidden = true;
      idInput.value = "";
      store.set({ error: "" });
      const cams = await api.cameras();
      store.set({ cameras: cams });
    } catch (e) {
      fail(e);
    } finally {
      submit.disabled = false;
    }
  });

  store.subscribe((st, patch) => {
    if ("cameras" in patch) render(st.cameras);
    if ("quality" in patch) for (const c of cards.values()) c.qRender();
    if ("qualityEvent" in patch && st.qualityEvent) {
      const ev = st.qualityEvent;
      const card = cards.get(ev.camera);
      if (card) card.qFetch();
    }
    if ("addRequest" in patch && st.addRequest) {
      const req = st.addRequest;
      store.set({ addRequest: null });
      document.querySelector('button.tab[data-tab="cameras"]').click();
      openForm(req.libraryId);
    }
  });
  render(store.get().cameras);

  // Later tasks extend each card through these hooks.
  return { cards };
}
