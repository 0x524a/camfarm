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
    const removeBtn = el("button", { type: "button", class: "danger sm btn-remove", text: "Remove" });
    const confirmBtn = el("button", { type: "button", class: "danger sm btn-remove-confirm", text: "Confirm remove", hidden: true });
    const cancelBtn = el("button", { type: "button", class: "ghost sm btn-remove-cancel", text: "Cancel", hidden: true });
    const prompt = el("span", { class: "rm-prompt", text: "Remove " + id + "?", hidden: true });
    const actions = el("div", { class: "row actions" }, [snapBtn, el("span", { class: "grow" }), prompt, removeBtn, confirmBtn, cancelBtn]);
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
        const url = blobs.get(id);
        if (url) URL.revokeObjectURL(url);
        blobs.delete(id);
      },
    };
    card.update(cam);
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
