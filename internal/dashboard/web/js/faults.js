import { el } from "./dom.js";

export function mount(drawer, ctx) {
  const { store, api, fail } = ctx;
  let opener = null; // the element that opened the drawer, for returning focus
  let busy = false; // an Apply or Clear is in flight; the focused button is never disabled
  const page = document.querySelector("main");
  let loadErr = ""; // why the catalogue could not be loaded, shown inside the drawer

  // The page-level #error sits under the inert <main> while the drawer is open
  // (and under the drawer on a narrow screen), so errors also appear in the drawer.
  function drawerFail(e) {
    const box = drawer.querySelector(".drawer-error");
    if (box) box.textContent = e && e.message ? e.message : String(e);
    fail(e);
  }
  function drawerOk() {
    const box = drawer.querySelector(".drawer-error");
    if (box) box.textContent = "";
  }

  async function ensureCatalogue() {
    if (store.get().catalogue.length) return;
    try { loadErr = ""; store.set({ catalogue: await api.catalogue() }); } catch (e) { loadErr = e && e.message ? e.message : String(e); fail(e); }
  }

  function paramInput(p) {
    let input;
    if (p.type === "enum") {
      input = el("select", { class: "fault-param", dataset: { name: p.name } },
        (p.options || []).map((o) => el("option", { value: o, text: o })));
      if (p.default !== undefined) input.value = p.default;
    } else if (p.type === "boolean") {
      input = el("input", { type: "checkbox", class: "fault-param", dataset: { name: p.name } });
      input.checked = p.default === true;
    } else {
      input = el("input", {
        type: "number", class: "fault-param", dataset: { name: p.name },
        step: p.type === "integer" ? "1" : "any",
        min: p.min !== undefined ? p.min : false, max: p.max !== undefined ? p.max : false,
      });
      // A default of 0 under a positive minimum means "unset" (onlyRequest); leave it empty.
      if (p.default !== undefined && !(p.default === 0 && p.min > 0)) input.value = p.default;
    }
    return el("label", { title: p.help || "" }, [p.name + (p.required ? " *" : ""), input]);
  }

  function close() {
    store.set({ faultsFor: null });
  }

  function activeNodes(cam) {
    return (cam.faults || []).length
      ? cam.faults.map((f) => el("div", { class: "fault-active", text: "active: " + JSON.stringify(f) }))
      : [el("div", { class: "sub", text: "No active faults." })];
  }

  function render(id) {
    const st = store.get();
    const cam = st.cameras.find((c) => c.id === id);
    if (!cam) { close(); return false; }
    const kinds = st.catalogue;
    const kind = el("select", { id: "fault-kind" }, kinds.map((k) => el("option", {
      value: k.kind, text: k.kind + (k.implemented ? "" : " (not implemented)"), disabled: !k.implemented,
    })));
    const first = kinds.find((k) => k.implemented);
    if (first) kind.value = first.kind;
    const params = el("div", { class: "params" });
    const models = el("p", { class: "sub" });
    const syncKind = () => {
      const k = kinds.find((x) => x.kind === kind.value);
      params.replaceChildren(...((k && k.params) || []).map(paramInput));
      models.textContent = k ? k.models : "";
    };
    kind.addEventListener("change", syncKind);
    syncKind();

    const active = el("div", { class: "active" }, activeNodes(cam));

    const apply = el("button", { type: "button", id: "fault-apply", text: "Apply" });
    const clear = el("button", { type: "button", id: "fault-clear", class: "ghost", text: "Clear faults" });
    const closeBtn = el("button", { type: "button", id: "drawer-close", class: "ghost", text: "Close" });
    closeBtn.addEventListener("click", close);

    apply.addEventListener("click", async () => {
      const spec = { kind: kind.value };
      params.querySelectorAll(".fault-param").forEach((input) => {
        const name = input.dataset.name;
        if (input.type === "checkbox") { if (input.checked) spec[name] = true; return; }
        if (input.value === "") return;
        spec[name] = input.type === "number" ? Number(input.value) : input.value;
      });
      if (busy) return;
      busy = true;
      apply.setAttribute("aria-disabled", "true");
      try {
        await api.injectFault(id, spec);
        store.set({ error: "", cameras: await api.cameras() });
        drawerOk();
      } catch (e) { drawerFail(e); } finally { busy = false; apply.removeAttribute("aria-disabled"); }
    });
    clear.addEventListener("click", async () => {
      if (busy) return;
      busy = true;
      clear.setAttribute("aria-disabled", "true");
      try {
        await api.clearFaults(id);
        store.set({ error: "", cameras: await api.cameras() });
        drawerOk();
      } catch (e) { drawerFail(e); } finally { busy = false; clear.removeAttribute("aria-disabled"); }
    });

    drawer.replaceChildren(
      el("div", { class: "row" }, [el("h2", { id: "drawer-title", text: "Faults on " + id }), el("span", { class: "grow" }), closeBtn]),
      el("div", { class: "drawer-error", role: "alert" }),
      active,
      el("label", {}, ["Kind", kind]),
      models, params,
      el("div", { class: "row" }, [apply, clear]),
    );
    return true;
  }

  // Escape is heard on the document while the drawer is open, wherever focus is.
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && !drawer.hidden) { e.preventDefault(); close(); }
  });

  // Keep Tab inside the drawer while it is open.
  drawer.addEventListener("keydown", (e) => {
    if (e.key !== "Tab") return;
    const f = [...drawer.querySelectorAll("button, select, input")].filter((n) => !n.disabled);
    if (!f.length) return;
    const first = f[0], last = f[f.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  });

  store.subscribe(async (st, patch) => {
    if ("faultsFor" in patch) {
      if (st.faultsFor) {
        const id = st.faultsFor;
        opener = document.activeElement;
        await ensureCatalogue();
        if (store.get().faultsFor !== id) return; // closed or switched while loading
        if (!render(id)) return;
        drawer.hidden = false;
        if (page) page.inert = true;
        if (loadErr) drawer.querySelector(".drawer-error").textContent = loadErr;
        const first = drawer.querySelector("#fault-kind");
        if (first) first.focus();
      } else {
        drawer.hidden = true;
        if (page) page.inert = false;
        drawer.replaceChildren();
        if (opener && opener.isConnected) opener.focus();
        opener = null;
      }
    } else if ("cameras" in patch && st.faultsFor && !drawer.hidden) {
      // Refresh only the active-fault list so a half-filled form is not lost.
      const cam = st.cameras.find((c) => c.id === st.faultsFor);
      const box = drawer.querySelector(".active");
      if (!cam) close();
      else if (box) box.replaceChildren(...activeNodes(cam));
    }
  });
}
