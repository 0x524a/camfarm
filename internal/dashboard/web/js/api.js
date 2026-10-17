// The only module that knows URLs. Every request goes through send().
const BASE = "/api/v1";

export class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

const enc = encodeURIComponent;

async function failure(r) {
  let msg = r.statusText || "HTTP " + r.status;
  try {
    const j = await r.json();
    if (j && j.error) msg = j.error;
  } catch (_) { /* body was not JSON */ }
  return new ApiError(msg, r.status);
}

async function send(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  let r;
  try {
    r = await fetch(BASE + path, opts);
  } catch (_) {
    throw new ApiError("Cannot reach the server.", 0);
  }
  if (!r.ok) throw await failure(r);
  return r;
}

const json = async (method, path, body) => (await send(method, path, body)).json();
const none = async (method, path, body) => { await send(method, path, body); };

export const api = {
  fleet: () => json("GET", "/fleet"),
  cameras: () => json("GET", "/cameras"),
  addCamera: (spec) => json("POST", "/cameras", spec),
  removeCamera: (id) => none("DELETE", "/cameras/" + enc(id)),
  async snapshot(id) {
    return (await send("GET", "/cameras/" + enc(id) + "/snapshot.jpg")).blob();
  },
  catalogue: () => json("GET", "/faults"),
  injectFault: (id, spec) => json("POST", "/cameras/" + enc(id) + "/faults", spec),
  clearFaults: (id) => none("DELETE", "/cameras/" + enc(id) + "/faults"),
  startQuality: (id, req) => json("POST", "/cameras/" + enc(id) + "/quality", req || {}),
  getQuality: (id) => json("GET", "/cameras/" + enc(id) + "/quality"),
  cancelQuality: (id) => none("DELETE", "/cameras/" + enc(id) + "/quality"),
  metrics: () => json("GET", "/metrics"),
  library: () => json("GET", "/library"),
  scan: (root, force) => json("POST", "/library/scan", { root: root || "", force: !!force }),
  deleteFile: (id) => none("DELETE", "/library/files/" + enc(id)),
  addRoot: (path) => json("POST", "/library/roots", { path }),
  // fetch has no upload progress, so an upload uses XMLHttpRequest.
  upload(file, onProgress) {
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open("POST", BASE + "/library/upload");
      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable && onProgress) onProgress(e.loaded / e.total);
      };
      xhr.onload = () => {
        let j = {};
        try { j = JSON.parse(xhr.responseText); } catch (_) { /* not JSON */ }
        if (xhr.status >= 200 && xhr.status < 300) resolve(j);
        else reject(new ApiError(j.error || xhr.statusText || "Upload failed", xhr.status));
      };
      xhr.onerror = () => reject(new ApiError("Cannot reach the server.", 0));
      const fd = new FormData();
      fd.append("file", file, file.name);
      xhr.send(fd);
    });
  },
};
