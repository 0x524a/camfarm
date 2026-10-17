import { el, fmtBytes, fmtNum } from "./dom.js";

const STATUS = {
  valid: ["Valid", "ok"],
  warning: ["Valid, with warnings", "warn"],
  invalid: ["Invalid", "bad"],
};
const FILTERS = [["all", "All files"], ["valid", "Valid"], ["warning", "With warnings"], ["invalid", "Invalid"]];

export function mount(panel, ctx) {
  const { store, api, fail } = ctx;
  let filter = "all";
  let disabled = false;

  const stats = el("div", { class: "stats" });
  const notes = el("div", { class: "notes" });
  const banner = el("div", { id: "scan-errors", class: "banner", hidden: true });
  const rootsBox = el("div", { class: "roots" });
  const rootForm = el("form", { id: "root-form", class: "row" });
  const pathInput = el("input", { id: "root-path", placeholder: "/mnt/camera-clips", "aria-label": "Directory path", autocomplete: "off" });
  rootForm.append(pathInput, el("button", { type: "submit", class: "ghost sm", text: "Add shared path" }));
  const progress = el("span", { class: "sub", hidden: true });
  const drop = el("div", { id: "drop", class: "drop", tabindex: "0", role: "button", text: "Drop a video here, or click to choose one. It is validated as soon as it lands." });
  const fileInput = el("input", { id: "file-input", type: "file", hidden: true, accept: "video/*,.ts,.mp4,.mkv,.mov" });
  const tbody = el("tbody", { id: "files-body" });
  const empty = el("p", { id: "lib-empty", class: "empty", hidden: true });
  const disabledMsg = el("p", { class: "empty", hidden: true });

  const refreshBtn = el("button", { id: "lib-refresh", type: "button", class: "ghost sm", text: "Refresh" });
  const rescanBtn = el("button", { id: "lib-rescan", type: "button", class: "sm", text: "Rescan all" });

  const body = el("div", { class: "lib-body" }, [
    stats,
    el("div", { class: "panel" }, [
      el("div", { class: "row" }, [el("h2", { text: "Sources" }), el("span", { class: "grow" }), refreshBtn, rescanBtn]),
      rootsBox, rootForm, notes,
    ]),
    banner,
    drop, fileInput, progress,
    el("div", { class: "panel table-wrap" }, [
      el("table", { id: "files" }, [
        el("thead", {}, [el("tr", {}, ["File", "Status", "Stream", "Findings", "In use by", ""].map((h) => el("th", { text: h })))]),
        tbody,
      ]),
      empty,
    ]),
  ]);
  panel.replaceChildren(body, disabledMsg);

  function setView(view) {
    store.set({ library: view });
  }

  async function run(fn) {
    try {
      const view = await fn();
      if (view) setView(view);
      store.set({ error: "" });
    } catch (e) {
      fail(e);
    }
  }

  refreshBtn.addEventListener("click", () => run(() => api.library()));
  rescanBtn.addEventListener("click", () => run(() => api.scan("", false)));
  rootForm.addEventListener("submit", (ev) => {
    ev.preventDefault();
    const p = pathInput.value.trim();
    if (!p) return;
    run(async () => {
      const view = await api.addRoot(p);
      pathInput.value = "";
      return view;
    });
  });

  async function upload(file) {
    progress.hidden = false;
    progress.textContent = "Uploading " + file.name + "…";
    try {
      await api.upload(file, (f) => { progress.textContent = "Uploading " + file.name + " " + Math.round(f * 100) + "%"; });
      progress.textContent = "Checking " + file.name + "…";
      setView(await api.library());
      store.set({ error: "" });
    } catch (e) {
      fail(e);
    } finally {
      progress.hidden = true;
      fileInput.value = "";
    }
  }
  drop.addEventListener("click", () => fileInput.click());
  drop.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === " ") { e.preventDefault(); fileInput.click(); }
  });
  fileInput.addEventListener("change", () => { if (fileInput.files[0]) upload(fileInput.files[0]); });
  ["dragenter", "dragover"].forEach((n) => drop.addEventListener(n, (e) => { e.preventDefault(); drop.classList.add("over"); }));
  ["dragleave", "drop"].forEach((n) => drop.addEventListener(n, (e) => {
    e.preventDefault();
    drop.classList.remove("over");
    if (n === "drop" && e.dataTransfer.files[0]) upload(e.dataTransfer.files[0]);
  }));

  function factsText(f) {
    const r = f.report.facts || {};
    const size = r.width && r.height ? r.width + "×" + r.height : "-";
    return [
      [r.container, r.codec].filter(Boolean).join(" ") || "-",
      size + " · " + fmtNum(r.fps, 1) + " fps",
      fmtNum(r.durationSeconds, 1) + " s · " + fmtBytes(f.size),
    ];
  }

  function row(f) {
    const st = f.report.status;
    const [label, cls] = STATUS[st] || [st, ""];
    const issues = f.report.issues || [];
    const inUse = f.inUseBy || [];
    const useBtn = el("button", { type: "button", class: "sm btn-use", text: "Use for camera", disabled: st === "invalid" });
    useBtn.addEventListener("click", () => store.set({ addRequest: { libraryId: f.id } }));
    const delBtn = el("button", { type: "button", class: "danger sm btn-del", text: "Delete", disabled: inUse.length > 0, title: inUse.length ? "A camera is using this file" : "" });
    const confirm = el("button", { type: "button", class: "danger sm btn-del-confirm", text: "Confirm delete", hidden: true });
    const cancel = el("button", { type: "button", class: "ghost sm btn-del-cancel", text: "Cancel", hidden: true });
    const ask = (on) => { delBtn.hidden = on; confirm.hidden = !on; cancel.hidden = !on; };
    delBtn.addEventListener("click", () => ask(true));
    cancel.addEventListener("click", () => ask(false));
    confirm.addEventListener("click", () => run(async () => {
      await api.deleteFile(f.id);
      return api.library();
    }).then(() => ask(false)));
    return el("tr", { class: "sev-" + st, dataset: { id: f.id, name: f.name } }, [
      el("td", {}, [el("b", { text: f.name }), el("div", { class: "sub mono", text: f.root })]),
      el("td", {}, [el("span", { class: "pill status " + cls, text: label })]),
      el("td", { class: "mono" }, factsText(f).map((l) => el("div", { text: l }))),
      el("td", {}, issues.length
        ? issues.map((i) => el("p", { class: "finding " + i.severity }, [el("b", { text: i.message }), i.detail ? el("span", { text: i.detail }) : null]))
        : [el("span", { class: "sub", text: "No problems found." })]),
      el("td", {}, inUse.length ? inUse.map((c) => el("span", { class: "pill", text: c })) : [el("span", { class: "sub", text: "unused" })]),
      el("td", {}, [el("div", { class: "row" }, [useBtn, delBtn, confirm, cancel])]),
    ]);
  }

  function render(lib) {
    if (!lib) return;
    const files = lib.files || [];
    const count = { all: files.length, valid: 0, warning: 0, invalid: 0 };
    files.forEach((f) => { count[f.report.status] = (count[f.report.status] || 0) + 1; });
    document.getElementById("count-library").textContent = String(count.warning + count.invalid || "");

    stats.replaceChildren(...FILTERS.map(([k, label]) => {
      const b = el("button", { type: "button", class: "stat", "aria-pressed": String(filter === k), dataset: { f: k } },
        [el("b", { text: String(count[k] || 0) }), el("span", { text: label })]);
      b.addEventListener("click", () => { filter = k; render(store.get().library); });
      return b;
    }));

    rootsBox.replaceChildren(...(lib.roots || []).map((r) => {
      const n = files.filter((f) => f.root === r.path).length;
      const btn = el("button", { type: "button", class: "ghost sm btn-root-rescan", text: "Rescan" });
      btn.addEventListener("click", () => run(() => api.scan(r.path, false)));
      return el("div", { class: "root" }, [
        el("code", { text: r.path }),
        el("span", { class: "sub", text: n + (n === 1 ? " file" : " files") + (r.writable ? " · uploads go here" : "") }),
        el("span", { class: "grow" }), btn,
      ]);
    }));

    notes.replaceChildren(
      ...(lib.allowUIPaths ? [] : [el("p", { class: "sub lib-note", text: "Adding paths is off. Start the server with a media directory that allows it." })]),
      ...(lib.uploadEnabled ? [] : [el("p", { class: "sub lib-note", text: "Uploads are off. Start the server with a writable media directory to enable them." })]),
    );
    // The drop area and the path form are removed from the DOM, not just hidden,
    // when their feature is off, so a disabled control cannot be reached.
    if (!lib.uploadEnabled) { drop.remove(); fileInput.remove(); } else if (!drop.isConnected) { body.insertBefore(drop, banner.nextSibling); body.insertBefore(fileInput, drop.nextSibling); }
    if (!lib.allowUIPaths) rootForm.remove(); else if (!rootForm.isConnected) rootsBox.after(rootForm);

    const errs = lib.scanErrors || [];
    banner.hidden = errs.length === 0;
    banner.textContent = errs.length ? "The last scan could not read: " + errs.join(", ") : "";

    const shown = files.filter((f) => filter === "all" || f.report.status === filter);
    tbody.replaceChildren(...shown.map(row));
    empty.hidden = shown.length > 0;
    empty.textContent = files.length === 0
      ? "No video files yet. Upload one, add a shared path, or put files in a media directory and press Rescan."
      : "No files match this filter.";
  }

  async function load() {
    try {
      setView(await api.library());
    } catch (e) {
      if (e.status === 404) {
        disabled = true;
        body.hidden = true;
        disabledMsg.hidden = false;
        disabledMsg.textContent = "The media library is not enabled on this server.";
      } else {
        fail(e);
      }
    }
  }

  store.subscribe((st, patch) => {
    if ("library" in patch && !disabled) render(st.library);
  });
  load();
}
