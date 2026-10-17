# Dashboard Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the single-table dashboard page with a tabbed UI (Cameras, Library) that shows camera snapshot thumbnails, host metrics, library validation status with every finding spelled out, upload and shared-path sources, a fault drawer, and stream-quality results.

**Architecture:** Plain ES modules under `internal/dashboard/web/`, embedded with `go:embed`, served by the existing static file handler. No server API changes. `js/api.js` is the only module that builds requests; feature modules receive `ctx = {store, api, fail}` and talk to each other only through the store. The new page is built as `app.html` beside the old `index.html` so the old e2e tests keep passing; the last task swaps them.

**Tech Stack:** Vanilla JavaScript (ES modules, no build step, no dependency), CSS custom properties, Go `embed`, `playwright-go` e2e tests behind the `e2e` build tag.

**Spec:** `docs/superpowers/specs/2026-10-17-dashboard-redesign-design.md`

## Global Constraints

- No build step, no JS toolchain, no JS dependency, no CDN, no web font. `CGO_ENABLED=0` still builds.
- No server API change. The only Go changes are tests and, in Task 7, the file swap.
- Server text is inserted with `textContent` / `el(...)` children only, never `innerHTML`.
- Metrics stream URL is exactly `/api/v1/events?metrics=1`.
- A `null` metric renders as `-`, never `0`. The GPU tile says "GPU busy" and notes "kernel-busy time"; it never says SM occupancy.
- Library: no polling and no library events. Every action's response replaces the view; Refresh is `GET /library`; Rescan is `POST /library/scan`.
- Thumbnails are fetched once on load and on add, and again only from the "Refresh snapshot" button.
- No authentication, no new endpoint. Authentication stays out of scope.
- Both themes are defined on tokens (`:root`, `@media (prefers-color-scheme: dark)` guarded by `:root:not([data-theme="light"])`, and `:root[data-theme="dark"]`). `body` sets a token background.
- Phone width (400px): no horizontal page scroll; only the library table scrolls inside its own container.
- No employer or client name anywhere. No coverage number or claim in the README that CI did not produce. Commit only under the repo's configured noreply identity.
- The GPU tile is verified against captured `nvidia-smi` output only, not live hardware.

## Review Focus

Input classes the spec implies but a straight-line implementation would miss. Each line names the test that pins it.

1. **Camera id that collides with an object prototype name or contains markup** (`constructor`, `__proto__`, `<b>x</b>`): must render as text and be removable. Pinned in Task 2 (`TestAppCameraIDsAreTextAndPrototypeNamesWork`).
2. **A snapshot that fails (camera mid-restart, server 5xx):** the card shows the error text in place of the picture and the rest of the grid still works. Pinned in Task 2 (`TestAppFailedSnapshotShowsTheErrorOnTheCard`).
3. **Library with every file invalid, with scan errors, or empty; upload disabled; UI paths disabled:** each shows its one-line reason, not a broken control. Pinned in Task 5 (`TestAppLibraryHidesDisabledControlsWithAReason`, `TestAppLibraryEmptyAndInvalidOnly`).
4. **Metrics section unavailable or value `null` (GPU absent, first sample):** the tile shows the reason or `-`, other tiles keep their numbers. Pinned in Task 6.
5. **Event stream drops:** the connection pill changes, the metrics strip says stale, and on reconnect the camera list is re-fetched. Pinned in Task 6 (`TestAppMetricsStripGoesStaleWhenTheStreamDrops`) and Task 1.

---

## File Structure

| Path | Responsibility |
|---|---|
| `internal/dashboard/web/app.html` | Shell: header, error region, metrics strip mount, tab list, panels, drawer mount (renamed to `index.html` in Task 7) |
| `internal/dashboard/web/app.css` | Tokens and every component style |
| `internal/dashboard/web/js/api.js` | All `fetch`/XHR; `ApiError` with the server's `error` text; the only module that knows URLs |
| `internal/dashboard/web/js/state.js` | `createStore(initial)` with `get`, `set(patch)`, `subscribe(fn)` |
| `internal/dashboard/web/js/dom.js` | `el()`, byte/percent formatters, `sparkline()`, `copyText()` |
| `internal/dashboard/web/js/main.js` | Store, event stream, tabs, connection pill, error region, mounts the feature modules |
| `internal/dashboard/web/js/metrics.js` | Metrics strip |
| `internal/dashboard/web/js/cameras.js` | Camera grid, thumbnails, add form, quality button and result |
| `internal/dashboard/web/js/faults.js` | Fault drawer |
| `internal/dashboard/web/js/library.js` | Library tab |
| `internal/dashboard/embed_test.go` | Every file the page references exists in the embedded FS |
| `internal/dashboard/e2e_app_test.go` | New Playwright tests (build tag `e2e`) |

`dom.js` is a small shared helper that the spec's file table does not list; Task 1 adds it to the spec table in the same commit.

## Running the e2e tests

The existing suite is the reference. CI assembles the Playwright driver from npm (see `.github/workflows/ci.yml`, step "install Playwright driver and Chromium"). Locally set `PLAYWRIGHT_DRIVER_PATH` to a driver assembled the same way and `CAMFARM_E2E_CHROMIUM=/usr/bin/google-chrome`, then:

```bash
go test -tags e2e -count=1 -run '<TestName>' ./internal/dashboard/
```

If the browser cannot start, the tests fail (they never skip). Task 1 Step 1 checks that the existing suite runs before anything is changed.

## DOM contract (selectors the tests and the code share)

| Selector | Meaning |
|---|---|
| `#conn` | Connection pill text: `connecting…`, `connected`, `disconnected, retrying` |
| `#replay` | Replay line from `GET /fleet` |
| `#error` | `role="alert"` region for action errors |
| `button.tab[data-tab="cameras"\|"library"]`, `#panel-cameras`, `#panel-library` | Tabs and panels |
| `#metrics`, `.tile[data-tile]`, `.tile-value`, `.tile-reason` | Metrics strip (`data-tile` = `cpu`, `memory`, `gpu-<index>`, `process`); `#metrics.stale` when the stream is down |
| `#cam-grid .cam[data-id]` | One camera card; inside: `img.snap`, `.snap-ph`, `.btn-snap`, `.btn-faults`, `.btn-quality`, `.btn-remove`, `.btn-remove-confirm`, `.btn-remove-cancel`, `.chip`, `.qbox` |
| `#add-open`, `#add-form`, `#add-id`, `#add-source` (`fixture`\|`library`\|`file`), `#add-lib`, `#add-path`, `#add-user`, `#add-pass`, `#add-submit`, `#add-cancel` | Add-camera form |
| `#drawer`, `#drawer-title`, `#fault-kind`, `.fault-param[data-name]`, `#fault-apply`, `#fault-clear`, `#drawer-close`, `.fault-active` | Fault drawer |
| `.stat[data-f]` (`all`\|`valid`\|`warning`\|`invalid`), `#files tbody tr[data-id]`, `.status`, `.finding`, `.btn-use`, `.btn-del`, `.btn-del-confirm`, `#lib-refresh`, `#lib-rescan`, `.root`, `.btn-root-rescan`, `#root-form`, `#root-path`, `#drop`, `#file-input`, `#scan-errors`, `.lib-note` | Library tab |

---

### Task 1: Shell, API client, store, event stream, tabs

**Files:**
- Create: `internal/dashboard/web/app.html`, `app.css`, `js/api.js`, `js/state.js`, `js/dom.js`, `js/main.js`
- Create: `internal/dashboard/embed_test.go`, `internal/dashboard/e2e_app_test.go`
- Modify: `docs/superpowers/specs/2026-10-17-dashboard-redesign-design.md` (add `dom.js` to the file table)

**Interfaces:**
- Produces (used by every later task):
  - `api` object (`js/api.js`): `fleet()`, `cameras()`, `addCamera(spec)`, `removeCamera(id)`, `snapshot(id)` → `Blob`, `catalogue()`, `injectFault(id, spec)`, `clearFaults(id)`, `startQuality(id, req)`, `getQuality(id)`, `cancelQuality(id)`, `metrics()`, `library()`, `scan(root, force)`, `upload(file, onProgress)`, `deleteFile(id)`, `addRoot(path)`. `ApiError` has `.status` and `.message` (the server's `error` text, or `Cannot reach the server.` with status 0).
  - `createStore(initial)` → `{get(), set(patch), subscribe(fn(state, patch))}`.
  - `el(tag, props, children)`, `fmtBytes(n)`, `fmtPct(v, digits)`, `fmtNum(v, digits)`, `sparkline(values, max)`, `copyText(text)` → `Promise<boolean>`.
  - Store keys: `cameras[]`, `catalogue[]`, `library|null`, `metrics|null`, `conn` (`connecting`|`ok`|`lost`), `error`, `quality{id: Run}`, `qualityEvent|null`, `addRequest|null` (`{libraryId}`), `faultsFor|null` (camera id).
  - `main.js` exports nothing; it builds `ctx = {store, api, fail}` and calls each feature module's `mount(root, ctx)`.

- [ ] **Step 1: Confirm the existing e2e suite runs here**

Run: `go test -tags e2e -count=1 -run 'TestE2EAddAndRemoveACamera' ./internal/dashboard/`
Expected: PASS. If the driver or browser is missing, stop and fix the local environment (see "Running the e2e tests") before continuing; every later task depends on it.

- [ ] **Step 2: Write the embed test**

Create `internal/dashboard/embed_test.go`:

```go
package dashboard

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

var (
	htmlRef   = regexp.MustCompile(`(?:src|href)="(\./)?([^"#:]+\.(?:js|css))"`)
	importRef = regexp.MustCompile(`from\s+"\./([^"]+\.js)"`)
)

// Every script, stylesheet and module the page loads must be in the embedded
// filesystem, so a missing file fails the build and not the browser.
func TestEmbeddedPageReferencesExist(t *testing.T) {
	web, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []string{"app.html", "index.html"} {
		b, err := fs.ReadFile(web, page)
		if err != nil {
			continue // app.html is renamed to index.html in the last task
		}
		for _, m := range htmlRef.FindAllStringSubmatch(string(b), -1) {
			if _, err := fs.Stat(web, m[2]); err != nil {
				t.Errorf("%s references %q, which is not embedded", page, m[2])
			}
		}
	}
	err = fs.WalkDir(web, "js", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
			return err
		}
		b, rerr := fs.ReadFile(web, p)
		if rerr != nil {
			return rerr
		}
		for _, m := range importRef.FindAllStringSubmatch(string(b), -1) {
			if _, serr := fs.Stat(web, "js/"+m[1]); serr != nil {
				t.Errorf("%s imports %q, which is not embedded", p, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 3: Write the e2e harness and the first failing tests**

Create `internal/dashboard/e2e_app_test.go`:

```go
//go:build e2e

// Real-browser tests of the redesigned dashboard (app.html until the swap in the
// last task). They reuse launch, newSession, expect and requireClean from
// e2e_test.go.
package dashboard

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/playwright-community/playwright-go"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/hostmetrics"
	"github.com/0x524a/camfarm/internal/library"
	"github.com/0x524a/camfarm/internal/quality"
)

// appPath is the redesigned page. The last task changes it to "/".
const appPath = "/app.html"

type appOpts struct {
	cameras []camfarm.CameraSpec
	lib     *library.Library
	quality bool
	metrics *hostmetrics.Sampler
}

func appServer(t *testing.T, o appOpts) (string, *camfarm.Fleet) {
	t.Helper()
	if o.cameras == nil {
		o.cameras = []camfarm.CameraSpec{{ID: "front-door"}}
	}
	f := camfarm.StartT(t, camfarm.Spec{Seed: 0xE2E, Cameras: o.cameras})
	opt := Options{Library: o.lib, Metrics: o.metrics}
	if o.quality {
		q := quality.New(quality.Options{})
		t.Cleanup(q.Close)
		opt.Quality = q
	}
	ts := httptest.NewServer(Handler(f, opt))
	t.Cleanup(ts.Close)
	return ts.URL, f
}

func (s *session) openApp(t *testing.T, url string) {
	t.Helper()
	if _, err := s.page.Goto(url + appPath); err != nil {
		t.Fatalf("Goto: %v", err)
	}
}

func (s *session) camCard(id string) playwright.Locator {
	return s.page.Locator(fmt.Sprintf(`#cam-grid .cam[data-id=%q]`, id))
}

func mustClick(t *testing.T, l playwright.Locator) {
	t.Helper()
	if err := l.Click(); err != nil {
		t.Fatal(err)
	}
}

func mustFill(t *testing.T, l playwright.Locator, v string) {
	t.Helper()
	if err := l.Fill(v); err != nil {
		t.Fatal(err)
	}
}

func TestAppShellShowsConnectionAndReplayLine(t *testing.T) {
	url, f := appServer(t, appOpts{})
	s := newSession(t)
	s.openApp(t, url)
	if err := expect().Locator(s.page.Locator("#conn")).ToHaveText("connected"); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(s.page.Locator("#replay")).ToContainText(f.ReplayLine()); err != nil {
		t.Fatalf("replay line not shown: %v", err)
	}
	s.requireClean(t)
}

func TestAppTabsSwitchAndFollowArrowKeys(t *testing.T) {
	url, _ := appServer(t, appOpts{})
	s := newSession(t)
	s.openApp(t, url)
	cams, lib := s.page.Locator(`button.tab[data-tab="cameras"]`), s.page.Locator(`button.tab[data-tab="library"]`)
	if err := expect().Locator(cams).ToHaveAttribute("aria-selected", "true"); err != nil {
		t.Fatal(err)
	}
	if err := cams.Focus(); err != nil {
		t.Fatal(err)
	}
	if err := s.page.Keyboard().Press("ArrowRight"); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(lib).ToHaveAttribute("aria-selected", "true"); err != nil {
		t.Fatalf("ArrowRight did not select the Library tab: %v", err)
	}
	if err := expect().Locator(s.page.Locator("#panel-library")).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(s.page.Locator("#panel-cameras")).ToBeHidden(); err != nil {
		t.Fatal(err)
	}
	s.requireClean(t)
}

func TestAppActionErrorsAreAnnouncedAndNotOverwrittenByBackgroundFailures(t *testing.T) {
	url, _ := appServer(t, appOpts{})
	s := newSession(t)
	s.openApp(t, url)
	if err := expect().Locator(s.page.Locator("#conn")).ToHaveText("connected"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.page.Evaluate(`window.__camfarmFail(new Error("boom from a test"))`); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(s.page.Locator("#error")).ToHaveText("boom from a test"); err != nil {
		t.Fatal(err)
	}
	role, err := s.page.Locator("#error").GetAttribute("role")
	if err != nil || role != "alert" {
		t.Fatalf("role = %q err=%v", role, err)
	}
	s.requireClean(t)
}

func TestAppNarrowViewportDoesNotScrollTheWholePage(t *testing.T) {
	url, _ := appServer(t, appOpts{})
	s := newSession(t)
	if err := s.page.SetViewportSize(400, 800); err != nil {
		t.Fatal(err)
	}
	s.openApp(t, url)
	if err := expect().Locator(s.page.Locator("#conn")).ToHaveText("connected"); err != nil {
		t.Fatal(err)
	}
	fits, err := s.page.Evaluate(`document.documentElement.scrollWidth <= document.documentElement.clientWidth`)
	if err != nil || fits != true {
		t.Fatalf("the page scrolls horizontally at 400px (fits=%v err=%v)", fits, err)
	}
	s.requireClean(t)
}

func TestAppBothColourSchemesRender(t *testing.T) {
	url, _ := appServer(t, appOpts{})
	bg := func(scheme *playwright.ColorScheme) string {
		s := newSession(t, playwright.BrowserNewContextOptions{ColorScheme: scheme})
		s.openApp(t, url)
		if err := expect().Locator(s.page.Locator("#conn")).ToHaveText("connected"); err != nil {
			t.Fatal(err)
		}
		v, err := s.page.Evaluate(`getComputedStyle(document.body).backgroundColor`)
		if err != nil {
			t.Fatal(err)
		}
		s.requireClean(t)
		str, _ := v.(string)
		return str
	}
	light, dark := bg(playwright.ColorSchemeLight), bg(playwright.ColorSchemeDark)
	if light == "" || dark == "" || light == dark {
		t.Fatalf("light (%q) and dark (%q) backgrounds should be set and differ", light, dark)
	}
}
```

- [ ] **Step 4: Run to verify the tests fail**

Run: `go test -tags e2e -count=1 -run 'TestApp' ./internal/dashboard/ && go test -count=1 -run TestEmbeddedPageReferencesExist ./internal/dashboard/`
Expected: e2e tests FAIL (404 for `/app.html`, so no `#conn`). The embed test passes vacuously now and starts guarding once the files exist.

- [ ] **Step 5: Write `js/api.js`**

```js
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
```

- [ ] **Step 6: Write `js/state.js` and `js/dom.js`**

`js/state.js`:

```js
// A tiny store. Modules read with get(), write with set(patch) and listen with
// subscribe(fn(state, patch)); they never import each other.
export function createStore(initial) {
  let state = { ...initial };
  const subs = new Set();
  return {
    get: () => state,
    set(patch) {
      state = { ...state, ...patch };
      subs.forEach((fn) => fn(state, patch));
    },
    subscribe(fn) {
      subs.add(fn);
      return () => subs.delete(fn);
    },
  };
}
```

`js/dom.js`:

```js
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
```

- [ ] **Step 7: Write `app.html`**

```html
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>CamFarm</title>
<link rel="icon" href="data:,">
<link rel="stylesheet" href="app.css">
</head>
<body>
<main>
  <header class="top">
    <div class="titles">
      <h1>CamFarm</h1>
      <p class="sub">A simulated camera fleet.</p>
    </div>
    <div class="status">
      <span id="conn" class="pill" role="status">connecting…</span>
      <code id="replay" class="mono">loading…</code>
    </div>
  </header>

  <div id="error" role="alert"></div>

  <section id="metrics" aria-label="Host metrics"></section>

  <div class="tabs" role="tablist" aria-label="Sections">
    <button class="tab" role="tab" id="tab-cameras" data-tab="cameras" aria-selected="true" aria-controls="panel-cameras">Cameras <span class="count" id="count-cameras"></span></button>
    <button class="tab" role="tab" id="tab-library" data-tab="library" aria-selected="false" aria-controls="panel-library" tabindex="-1">Library <span class="count" id="count-library"></span></button>
  </div>

  <section id="panel-cameras" role="tabpanel" aria-labelledby="tab-cameras">
    <div class="bar">
      <h2>Fleet</h2>
      <span class="grow"></span>
      <button id="add-open" type="button">Add camera</button>
    </div>
    <form id="add-form" class="panel add-form" hidden>
      <label>ID<input id="add-id" required autocomplete="off"></label>
      <label>Source
        <select id="add-source">
          <option value="fixture">Bundled fixture (H.264)</option>
          <option value="library">Library file</option>
          <option value="file">File path on this machine</option>
        </select>
      </label>
      <label id="add-lib-label" hidden>Library file<select id="add-lib"></select></label>
      <label id="add-path-label" hidden>File path<input id="add-path" autocomplete="off"></label>
      <label>Username (optional)<input id="add-user" autocomplete="off"></label>
      <label>Password (optional)<input id="add-pass" autocomplete="off"></label>
      <div class="row">
        <button id="add-submit" type="submit">Add</button>
        <button id="add-cancel" type="button" class="ghost">Cancel</button>
      </div>
    </form>
    <div id="cam-grid" class="grid"></div>
    <p id="cam-empty" class="empty" hidden>No cameras. Add one with the button above.</p>
  </section>

  <section id="panel-library" role="tabpanel" aria-labelledby="tab-library" hidden></section>
</main>

<aside id="drawer" class="drawer" role="dialog" aria-modal="true" aria-labelledby="drawer-title" hidden></aside>

<script type="module" src="js/main.js"></script>
</body>
</html>
```

- [ ] **Step 8: Write `app.css`**

```css
/* Layout: an instrument panel. Metrics strip on top, tabs below, dense cards and a findings table. */
:root {
  color-scheme: light;
  --bg: #f3f5f7; --panel: #ffffff; --panel2: #f8fafb; --text: #171c22; --muted: #5a6673; --line: #d8dee5;
  --accent: #1e5fd6; --accent-text: #ffffff; --accent-soft: #e6eefc;
  --ok: #17803a; --ok-soft: #e1f4e8; --warn: #9a5b00; --warn-soft: #fdf0d5; --bad: #b3261e; --bad-soft: #fbe4e2;
  --sans: system-ui, -apple-system, "Segoe UI", sans-serif;
  --mono: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
}
@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) {
    color-scheme: dark;
    --bg: #0e1216; --panel: #161b21; --panel2: #1b2128; --text: #e7ecf1; --muted: #95a1ae; --line: #2a323b;
    --accent: #6ea0ff; --accent-text: #0b1220; --accent-soft: #1a2740;
    --ok: #5ac17a; --ok-soft: #14301f; --warn: #f0b24a; --warn-soft: #352810; --bad: #ff8a80; --bad-soft: #3a1a18;
  }
}
:root[data-theme="dark"] {
  color-scheme: dark;
  --bg: #0e1216; --panel: #161b21; --panel2: #1b2128; --text: #e7ecf1; --muted: #95a1ae; --line: #2a323b;
  --accent: #6ea0ff; --accent-text: #0b1220; --accent-soft: #1a2740;
  --ok: #5ac17a; --ok-soft: #14301f; --warn: #f0b24a; --warn-soft: #352810; --bad: #ff8a80; --bad-soft: #3a1a18;
}
* { box-sizing: border-box; }
[hidden] { display: none !important; }
body {
  margin: 0; background: var(--bg); color: var(--text); font: 14px/1.45 var(--sans);
  padding-left: max(16px, env(safe-area-inset-left, 0px));
  padding-right: max(16px, env(safe-area-inset-right, 0px));
  padding-block: 16px 40px;
}
main { max-width: 1180px; margin-inline: auto; display: flex; flex-direction: column; gap: 14px; }
h1 { font-size: 19px; margin: 0; }
h2 { font-size: 13px; margin: 0; text-transform: uppercase; letter-spacing: .06em; color: var(--muted); }
h3 { margin: 0; font-size: 15px; }
.sub { color: var(--muted); font-size: 12px; margin: 2px 0 0; }
.mono, code { font-family: var(--mono); font-size: 12px; overflow-wrap: anywhere; }
button, input, select { font: inherit; color: inherit; }
button { background: var(--accent); color: var(--accent-text); border: 0; border-radius: 5px; padding: 6px 11px; cursor: pointer; font-weight: 500; }
button.ghost { background: transparent; color: var(--text); border: 1px solid var(--line); }
button.ghost:hover { border-color: var(--accent); color: var(--accent); }
button.danger { background: transparent; color: var(--bad); border: 1px solid var(--bad); }
button.sm { padding: 3px 8px; font-size: 12px; }
button:disabled { opacity: .5; cursor: default; }
:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
input, select { background: var(--panel2); border: 1px solid var(--line); border-radius: 5px; padding: 6px 8px; min-width: 0; }
label { display: flex; flex-direction: column; gap: 2px; font-size: 12px; color: var(--muted); }
.row { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; }
.grow { flex: 1; }
.panel { background: var(--panel); border: 1px solid var(--line); border-radius: 8px; padding: 12px; min-width: 0; }
.empty { color: var(--muted); padding: 12px 4px; }
.top { display: flex; flex-wrap: wrap; gap: 10px 16px; align-items: center; justify-content: space-between; }
.status { display: flex; flex-wrap: wrap; gap: 8px 12px; align-items: center; min-width: 0; }
.pill { display: inline-flex; align-items: center; gap: 5px; border-radius: 99px; padding: 1px 9px; font-size: 12px; font-weight: 500; white-space: nowrap; background: var(--panel2); color: var(--muted); border: 1px solid var(--line); }
.pill::before { content: ""; width: 7px; height: 7px; border-radius: 50%; background: currentColor; }
.pill.ok { background: var(--ok-soft); color: var(--ok); border-color: transparent; }
.pill.warn { background: var(--warn-soft); color: var(--warn); border-color: transparent; }
.pill.bad { background: var(--bad-soft); color: var(--bad); border-color: transparent; }
.pill.run { background: var(--accent-soft); color: var(--accent); border-color: transparent; }
#error { color: var(--bad); }
#error:empty { position: absolute; width: 1px; height: 1px; margin: 0; overflow: hidden; clip-path: inset(50%); white-space: nowrap; }
/* tabs */
.tabs { display: flex; gap: 2px; border-bottom: 1px solid var(--line); overflow-x: auto; }
.tab { background: none; color: var(--muted); border-radius: 0; border-bottom: 2px solid transparent; padding: 9px 14px; white-space: nowrap; }
.tab[aria-selected="true"] { color: var(--text); border-bottom-color: var(--accent); }
.count { margin-inline-start: 4px; color: var(--muted); font-variant-numeric: tabular-nums; }
.bar { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; margin-bottom: 10px; }
/* metrics */
#metrics { display: grid; grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); gap: 8px; }
#metrics.stale .tile { opacity: .55; }
.tile { background: var(--panel); border: 1px solid var(--line); border-radius: 8px; padding: 9px 11px; min-width: 0; }
.tile-head { display: flex; justify-content: space-between; gap: 6px; color: var(--muted); font-size: 12px; }
.tile-value { font: 500 21px/1.2 var(--mono); font-variant-numeric: tabular-nums; margin-top: 2px; }
.tile-value small { font-size: 12px; color: var(--muted); font-weight: 400; }
.tile-note { color: var(--muted); font-size: 12px; margin-top: 2px; overflow-wrap: anywhere; }
.tile-reason { font-size: 13px; color: var(--muted); margin-top: 6px; overflow-wrap: anywhere; }
.tile svg { display: block; width: 100%; height: 26px; margin-top: 4px; }
.spark-area { fill: var(--accent-soft); stroke: none; }
.spark-line { fill: none; stroke: var(--accent); stroke-width: 1.5; }
/* cameras */
.grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(310px, 1fr)); gap: 12px; }
.cam { background: var(--panel); border: 1px solid var(--line); border-radius: 8px; overflow: hidden; display: flex; flex-direction: column; min-width: 0; }
.thumb { aspect-ratio: 16 / 9; background: #111; display: grid; place-items: center; }
.thumb img { width: 100%; height: 100%; object-fit: cover; display: block; }
.snap-ph { color: #c9d1da; font-size: 12px; padding: 8px; text-align: center; overflow-wrap: anywhere; }
.cam-body { padding: 10px 12px; display: flex; flex-direction: column; gap: 7px; min-width: 0; }
.cam-body h3 { overflow-wrap: anywhere; }
.url { display: flex; gap: 6px; align-items: center; min-width: 0; }
.url code { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--muted); }
.chips { display: flex; flex-wrap: wrap; gap: 5px; }
.qbox:empty { display: none; }
.qres { background: var(--panel2); border: 1px solid var(--line); border-radius: 6px; padding: 8px 10px; display: flex; flex-direction: column; gap: 6px; }
.qres dl { display: grid; grid-template-columns: repeat(3, 1fr); gap: 6px 10px; margin: 0; }
.qres dt { font-size: 11px; color: var(--muted); }
.qres dd { margin: 0; font: 500 13px var(--mono); font-variant-numeric: tabular-nums; }
.qres ul { margin: 0; padding-left: 16px; font-size: 12px; }
.bar-track { height: 6px; background: var(--line); border-radius: 3px; overflow: hidden; }
.bar-fill { height: 100%; background: var(--accent); width: 0; }
.add-form { display: grid; grid-template-columns: repeat(auto-fit, minmax(170px, 1fr)); gap: 8px; align-items: end; margin-bottom: 12px; }
/* drawer */
.drawer { position: fixed; top: 0; right: 0; bottom: 0; width: min(420px, 100vw); background: var(--panel); border-left: 1px solid var(--line); padding: 16px max(16px, env(safe-area-inset-right, 0px)) 16px 16px; overflow-y: auto; z-index: 30; display: flex; flex-direction: column; gap: 12px; }
.drawer .params { display: grid; gap: 8px; }
.fault-active { color: var(--ok); font-size: 12px; }
/* library */
.stats { display: flex; flex-wrap: wrap; gap: 8px; }
.stat { background: var(--panel); border: 1px solid var(--line); border-radius: 8px; padding: 7px 12px; text-align: left; color: var(--text); min-width: 110px; }
.stat[aria-pressed="true"] { border-color: var(--accent); box-shadow: 0 0 0 1px var(--accent); }
.stat b { display: block; font: 500 20px var(--mono); }
.stat span { font-size: 12px; color: var(--muted); font-weight: 400; }
.roots { display: flex; flex-direction: column; gap: 6px; margin-top: 8px; }
.root { display: flex; flex-wrap: wrap; gap: 6px 12px; align-items: center; background: var(--panel2); border: 1px solid var(--line); border-radius: 6px; padding: 6px 10px; }
.drop { border: 1.5px dashed var(--line); border-radius: 8px; padding: 14px; text-align: center; color: var(--muted); cursor: pointer; background: var(--panel2); }
.drop:hover, .drop.over { border-color: var(--accent); color: var(--accent); }
.banner { background: var(--warn-soft); color: var(--warn); border-radius: 6px; padding: 8px 10px; }
.table-wrap { overflow-x: auto; }
table { border-collapse: collapse; width: 100%; min-width: 820px; }
th, td { text-align: left; vertical-align: top; padding: 9px 8px; border-top: 1px solid var(--line); }
th { font-size: 12px; color: var(--muted); font-weight: 600; border-top: 0; }
tr.sev-invalid td:first-child { box-shadow: inset 3px 0 var(--bad); }
tr.sev-warning td:first-child { box-shadow: inset 3px 0 var(--warn); }
tr.sev-valid td:first-child { box-shadow: inset 3px 0 var(--ok); }
.finding { margin: 3px 0 0; font-size: 12px; display: flex; gap: 6px; align-items: baseline; }
.finding b { font-weight: 600; }
.finding.invalid b { color: var(--bad); }
.finding.warning b { color: var(--warn); }
.finding span { color: var(--muted); }
@media (prefers-reduced-motion: reduce) { * { transition: none !important; animation: none !important; } }
```

- [ ] **Step 9: Write `js/main.js`**

```js
import { api } from "./api.js";
import { createStore } from "./state.js";

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
    store.set({ cameras: cams });
  } catch (_) {
    store.set({ conn: "lost" });
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
```

- [ ] **Step 10: Add `dom.js` to the spec table**

In `docs/superpowers/specs/2026-10-17-dashboard-redesign-design.md`, in the Files table, add this row after the `js/state.js` row:

```
| `js/dom.js` | Element builder, number and byte formatters, sparkline, clipboard helper |
```

- [ ] **Step 11: Run the tests**

Run: `go test -count=1 -run TestEmbeddedPageReferencesExist ./internal/dashboard/ && go test -tags e2e -count=1 -run 'TestApp' ./internal/dashboard/`
Expected: PASS. Also `go vet ./internal/dashboard/ && go vet -tags e2e ./internal/dashboard/`.

- [ ] **Step 12: Commit**

```bash
git add internal/dashboard/web internal/dashboard/embed_test.go internal/dashboard/e2e_app_test.go docs/superpowers/specs/2026-10-17-dashboard-redesign-design.md
git commit -m "dashboard: app shell, API client, store and event stream"
```

---

### Task 2: Cameras tab

**Files:**
- Create: `internal/dashboard/web/js/cameras.js`
- Modify: `internal/dashboard/web/js/main.js` (mount line), `internal/dashboard/e2e_app_test.go`

**Interfaces:**
- Consumes: `ctx = {store, api, fail}`, `el`, `copyText`, store keys `cameras`, `library`, `addRequest`.
- Produces: `mount(root, ctx)` in `cameras.js`. Card DOM per the DOM contract. The card builder is one function, `buildCard(cam)`, returning `{el, update(cam)}`; Tasks 3 and 4 add their button and result box to it. Store writes: `store.set({faultsFor: id})` (Task 3 listens), `store.set({addRequest: null})` after the form consumes it.

- [ ] **Step 1: Write the failing e2e tests**

Append to `internal/dashboard/e2e_app_test.go`:

```go
func TestAppCameraCardShowsThumbnailAndURLs(t *testing.T) {
	url, _ := appServer(t, appOpts{})
	s := newSession(t)
	s.openApp(t, url)
	card := s.camCard("front-door")
	if err := expect().Locator(card).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.page.WaitForFunction(`() => {
		const i = document.querySelector('.cam[data-id="front-door"] img.snap');
		return !!i && !i.hidden && i.complete && i.naturalWidth === 320 && i.naturalHeight === 240;
	}`, nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(assertTimeoutMS)}); err != nil {
		t.Fatalf("thumbnail never loaded as a 320x240 image: %v", err)
	}
	if err := expect().Locator(card).ToContainText("rtsp://"); err != nil {
		t.Fatalf("RTSP URL missing: %v", err)
	}
	s.requireClean(t)
}

func TestAppAddAndRemoveACameraWithInlineConfirm(t *testing.T) {
	url, _ := appServer(t, appOpts{})
	s := newSession(t)
	s.openApp(t, url)
	mustClick(t, s.page.Locator("#add-open"))
	mustFill(t, s.page.Locator("#add-id"), "lobby")
	mustClick(t, s.page.Locator("#add-submit"))
	lobby := s.camCard("lobby")
	if err := expect().Locator(lobby).ToBeVisible(); err != nil {
		t.Fatalf("added camera did not appear: %v", err)
	}
	// Remove asks first; Cancel keeps the camera.
	mustClick(t, lobby.Locator(".btn-remove"))
	mustClick(t, lobby.Locator(".btn-remove-cancel"))
	if err := expect().Locator(lobby).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	mustClick(t, lobby.Locator(".btn-remove"))
	mustClick(t, lobby.Locator(".btn-remove-confirm"))
	if err := expect().Locator(lobby).ToHaveCount(0); err != nil {
		t.Fatalf("removed camera still shown: %v", err)
	}
	if err := expect().Locator(s.camCard("front-door")).ToBeVisible(); err != nil {
		t.Fatal("the other camera disappeared")
	}
	s.requireClean(t)
}

func TestAppDuplicateCameraShowsAnError(t *testing.T) {
	url, _ := appServer(t, appOpts{})
	s := newSession(t)
	s.openApp(t, url)
	mustClick(t, s.page.Locator("#add-open"))
	mustFill(t, s.page.Locator("#add-id"), "front-door")
	mustClick(t, s.page.Locator("#add-submit"))
	if err := expect().Locator(s.page.Locator("#error")).ToContainText("duplicate"); err != nil {
		t.Fatalf("no duplicate error: %v", err)
	}
	s.requireClean(t, 409)
}

func TestAppFleetChangeAppearsWithoutReload(t *testing.T) {
	url, f := appServer(t, appOpts{})
	s := newSession(t)
	s.openApp(t, url)
	if err := expect().Locator(s.page.Locator("#conn")).ToHaveText("connected"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.AddCamera(camfarm.CameraSpec{ID: "from-library"}); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(s.camCard("from-library")).ToBeVisible(); err != nil {
		t.Fatalf("a library-side AddCamera was not pushed to the page: %v", err)
	}
	s.requireClean(t)
}

// Review Focus 1: ids that collide with Object.prototype names, or that contain
// markup, render as text and can be removed.
func TestAppCameraIDsAreTextAndPrototypeNamesWork(t *testing.T) {
	url, f := appServer(t, appOpts{})
	for _, id := range []string{"constructor", "__proto__", "toString", "<b>x</b>"} {
		if _, err := f.AddCamera(camfarm.CameraSpec{ID: id}); err != nil {
			t.Fatalf("AddCamera(%q): %v", id, err)
		}
	}
	s := newSession(t)
	s.openApp(t, url)
	for _, id := range []string{"constructor", "__proto__", "toString", "<b>x</b>"} {
		if err := expect().Locator(s.camCard(id)).ToBeVisible(); err != nil {
			t.Fatalf("%q did not render: %v", id, err)
		}
	}
	n, err := s.page.Locator("#cam-grid b").Count()
	if err != nil || n != 0 {
		t.Fatalf("markup in an id was parsed as HTML (%d <b> elements, err=%v)", n, err)
	}
	card := s.camCard("constructor")
	mustClick(t, card.Locator(".btn-remove"))
	mustClick(t, card.Locator(".btn-remove-confirm"))
	if err := expect().Locator(card).ToHaveCount(0); err != nil {
		t.Fatalf("camera named constructor was not removed: %v", err)
	}
	s.requireClean(t)
}

// Review Focus 2: a failed snapshot shows its error on the card.
func TestAppFailedSnapshotShowsTheErrorOnTheCard(t *testing.T) {
	url, _ := appServer(t, appOpts{})
	s := newSession(t)
	if err := s.page.Route("**/snapshot.jpg", func(r playwright.Route) {
		_ = r.Fulfill(playwright.RouteFulfillOptions{
			Status: playwright.Int(503), ContentType: playwright.String("application/json"),
			Body: `{"error":"camera is restarting"}`,
		})
	}); err != nil {
		t.Fatal(err)
	}
	s.openApp(t, url)
	if err := expect().Locator(s.camCard("front-door").Locator(".snap-ph")).ToContainText("camera is restarting"); err != nil {
		t.Fatalf("snapshot error not shown on the card: %v", err)
	}
	if err := expect().Locator(s.camCard("front-door").Locator(".btn-remove")).ToBeVisible(); err != nil {
		t.Fatal("the rest of the card stopped working")
	}
	s.requireClean(t, 503)
}

func TestAppRefreshSnapshotButtonFetchesAgain(t *testing.T) {
	url, _ := appServer(t, appOpts{})
	s := newSession(t)
	var hits int32
	if err := s.page.Route("**/snapshot.jpg", func(r playwright.Route) {
		atomic.AddInt32(&hits, 1)
		_ = r.Continue()
	}); err != nil {
		t.Fatal(err)
	}
	s.openApp(t, url)
	if err := expect().Locator(s.camCard("front-door").Locator("img.snap")).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	before := atomic.LoadInt32(&hits)
	mustClick(t, s.camCard("front-door").Locator(".btn-snap"))
	deadline := time.Now().Add(10 * time.Second)
	for atomic.LoadInt32(&hits) <= before && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if atomic.LoadInt32(&hits) <= before {
		t.Fatal("Refresh snapshot did not fetch again")
	}
	// And nothing else fetches on its own: wait and confirm the count is stable.
	time.Sleep(1500 * time.Millisecond)
	after := atomic.LoadInt32(&hits)
	time.Sleep(1500 * time.Millisecond)
	if atomic.LoadInt32(&hits) != after {
		t.Fatal("snapshots refresh on a timer; the design fetches only on load, add and click")
	}
	s.requireClean(t)
}
```

Add `"sync/atomic"` and `"time"` to the file's imports.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -tags e2e -count=1 -run 'TestApp(CameraCard|Add|Duplicate|Fleet|CameraIDs|Failed|Refresh)' ./internal/dashboard/`
Expected: FAIL (no `.cam` elements).

- [ ] **Step 3: Write `js/cameras.js`**

```js
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
    const actions = el("div", { class: "row actions" }, [snapBtn, el("span", { class: "grow" }), removeBtn, confirmBtn, cancelBtn]);
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
    if (libraryId && usable.some((f) => f.id === libraryId)) {
      src.value = "library";
      libSel.value = libraryId;
    }
    syncSource();
    form.hidden = false;
    idInput.focus();
  }
  root.querySelector("#add-open").addEventListener("click", () => openForm());
  root.querySelector("#add-cancel").addEventListener("click", () => { form.hidden = true; });
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
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
```

- [ ] **Step 4: Mount it in `main.js`**

In `internal/dashboard/web/js/main.js`, replace the line `// --- feature modules (each task adds its line) ---` with:

```js
// --- feature modules (each task adds its line) ---
import { mount as mountCameras } from "./cameras.js";
mountCameras($("#panel-cameras"), ctx);
```

Then move that `import` line to the top of the file, beside the other imports (ES imports are hoisted, but keep them together):

```js
import { api } from "./api.js";
import { createStore } from "./state.js";
import { mount as mountCameras } from "./cameras.js";
```

and leave only `mountCameras($("#panel-cameras"), ctx);` under the comment.

- [ ] **Step 5: Run the tests**

Run: `go test -count=1 -run TestEmbeddedPageReferencesExist ./internal/dashboard/ && go test -tags e2e -count=1 -run 'TestApp' ./internal/dashboard/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/dashboard/web internal/dashboard/e2e_app_test.go
git commit -m "dashboard: camera grid with snapshot thumbnails and the add form"
```

---

### Task 3: Fault drawer

**Files:**
- Create: `internal/dashboard/web/js/faults.js`
- Modify: `internal/dashboard/web/js/cameras.js` (Faults button), `internal/dashboard/web/js/main.js`, `internal/dashboard/e2e_app_test.go`, `docs/superpowers/specs/2026-10-17-dashboard-redesign-design.md`

**Interfaces:**
- Consumes: `api.catalogue()`, `api.injectFault(id, spec)`, `api.clearFaults(id)`, store `cameras`, `faultsFor`. The catalogue entry is `{kind, implemented, models, clock, params: [{name, type: "number"|"integer"|"boolean"|"enum", min?, max?, options?, default?, required, help}]}`. `param.name` is the JSON field name of the `FaultSpec`.
- Produces: `mount(root, ctx)` in `faults.js`, where `root` is `#drawer`. `store.set({faultsFor: id})` opens it; `store.set({faultsFor: null})` closes it.

- [ ] **Step 1: Correct the spec**

The fault API clears all of a camera's faults at once (`DELETE /cameras/{id}/faults`), so "individual clear buttons" in the spec cannot be built. In `docs/superpowers/specs/2026-10-17-dashboard-redesign-design.md`, in the "Fault drawer" section, replace

```
the active faults with individual clear buttons, and Apply.
```

with

```
the active faults, a "Clear faults" button (the API clears all of a camera's faults at once), and Apply.
```

- [ ] **Step 2: Write the failing e2e tests**

Append to `internal/dashboard/e2e_app_test.go`:

```go
func TestAppFaultDrawerAppliesAndClearsFrameDrop(t *testing.T) {
	url, f := appServer(t, appOpts{})
	s := newSession(t)
	s.openApp(t, url)
	card := s.camCard("front-door")
	mustClick(t, card.Locator(".btn-faults"))
	drawer := s.page.Locator("#drawer")
	if err := expect().Locator(drawer).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(s.page.Locator("#drawer-title")).ToContainText("front-door"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.page.Locator("#fault-kind").SelectOption(playwright.SelectOptionValues{Values: &[]string{"frame_drop"}}); err != nil {
		t.Fatal(err)
	}
	mustFill(t, s.page.Locator(`.fault-param[data-name="rate"]`), "0.5")
	mustClick(t, s.page.Locator("#fault-apply"))
	if err := expect().Locator(card.Locator(".chip").First()).ToContainText("frame_drop"); err != nil {
		t.Fatalf("fault chip missing on the card: %v", err)
	}
	cam, _ := f.Camera("front-door")
	if fs := cam.Faults(); len(fs) != 1 || fs[0].Kind != "frame_drop" || fs[0].Rate != 0.5 {
		t.Fatalf("faults = %+v", fs)
	}
	if err := expect().Locator(drawer.Locator(".fault-active")).ToContainText("frame_drop"); err != nil {
		t.Fatalf("active fault not listed in the drawer: %v", err)
	}
	mustClick(t, s.page.Locator("#fault-clear"))
	if err := expect().Locator(card.Locator(".chip").First()).ToContainText("no faults"); err != nil {
		t.Fatalf("faults not cleared on the card: %v", err)
	}
	if len(cam.Faults()) != 0 {
		t.Fatal("clear did not clear")
	}
	s.requireClean(t)
}

func TestAppFaultDrawerFocusAndEscape(t *testing.T) {
	url, _ := appServer(t, appOpts{})
	s := newSession(t)
	s.openApp(t, url)
	open := s.camCard("front-door").Locator(".btn-faults")
	mustClick(t, open)
	if err := expect().Locator(s.page.Locator("#drawer")).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	inside, err := s.page.Evaluate(`document.getElementById("drawer").contains(document.activeElement)`)
	if err != nil || inside != true {
		t.Fatalf("focus did not move into the drawer (inside=%v err=%v)", inside, err)
	}
	if err := s.page.Keyboard().Press("Escape"); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(s.page.Locator("#drawer")).ToBeHidden(); err != nil {
		t.Fatal(err)
	}
	back, err := s.page.Evaluate(`document.activeElement && document.activeElement.classList.contains("btn-faults")`)
	if err != nil || back != true {
		t.Fatalf("focus did not return to the Faults button (back=%v err=%v)", back, err)
	}
	s.requireClean(t)
}

func TestAppFaultDrawerShowsTheServerError(t *testing.T) {
	url, _ := appServer(t, appOpts{})
	s := newSession(t)
	s.openApp(t, url)
	mustClick(t, s.camCard("front-door").Locator(".btn-faults"))
	if _, err := s.page.Locator("#fault-kind").SelectOption(playwright.SelectOptionValues{Values: &[]string{"frame_drop"}}); err != nil {
		t.Fatal(err)
	}
	mustFill(t, s.page.Locator(`.fault-param[data-name="rate"]`), "7")
	mustClick(t, s.page.Locator("#fault-apply"))
	if err := expect().Locator(s.page.Locator("#error")).Not().ToBeEmpty(); err != nil {
		t.Fatalf("server validation error not shown: %v", err)
	}
	s.requireClean(t, 422, 400)
}

func TestAppUnimplementedFaultKindsAreDisabled(t *testing.T) {
	url, _ := appServer(t, appOpts{})
	s := newSession(t)
	s.openApp(t, url)
	mustClick(t, s.camCard("front-door").Locator(".btn-faults"))
	n, err := s.page.Locator("#fault-kind option").Count()
	if err != nil || n == 0 {
		t.Fatalf("no kinds listed (n=%d err=%v)", n, err)
	}
	// Every option the server marks unimplemented must be disabled.
	res, err := s.page.Evaluate(`fetch("/api/v1/faults").then(r => r.json()).then(c => c.filter(k => !k.implemented).every(k => {
		const o = document.querySelector('#fault-kind option[value="' + k.kind + '"]');
		return !o || o.disabled;
	}))`)
	if err != nil || res != true {
		t.Fatalf("an unimplemented kind is selectable (res=%v err=%v)", res, err)
	}
	s.requireClean(t)
}
```

- [ ] **Step 3: Run to verify they fail**

Run: `go test -tags e2e -count=1 -run 'TestApp(FaultDrawer|Unimplemented)' ./internal/dashboard/`
Expected: FAIL (no `.btn-faults`).

- [ ] **Step 4: Add the Faults button to the card**

In `cameras.js`, inside `buildCard`, after the `snapBtn` definition add:

```js
    const faultsBtn = el("button", { type: "button", class: "ghost sm btn-faults", text: "Faults" });
    faultsBtn.addEventListener("click", () => store.set({ faultsFor: id }));
```

and change the actions row to:

```js
    const actions = el("div", { class: "row actions" }, [snapBtn, faultsBtn, el("span", { class: "grow" }), removeBtn, confirmBtn, cancelBtn]);
```

- [ ] **Step 5: Write `js/faults.js`**

```js
import { el } from "./dom.js";

export function mount(drawer, ctx) {
  const { store, api, fail } = ctx;
  let opener = null; // the element that opened the drawer, for returning focus

  async function ensureCatalogue() {
    if (store.get().catalogue.length) return;
    try { store.set({ catalogue: await api.catalogue() }); } catch (e) { fail(e); }
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
      if (p.default !== undefined) input.value = p.default;
    }
    return el("label", { title: p.help || "" }, [p.name + (p.required ? " *" : ""), input]);
  }

  function close() {
    store.set({ faultsFor: null });
  }

  function render(id) {
    const st = store.get();
    const cam = st.cameras.find((c) => c.id === id);
    if (!cam) { close(); return; }
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

    const active = el("div", { class: "active" },
      (cam.faults || []).length
        ? cam.faults.map((f) => el("div", { class: "fault-active", text: "active: " + JSON.stringify(f) }))
        : [el("div", { class: "sub", text: "No active faults." })]);

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
      apply.disabled = true;
      try {
        await api.injectFault(id, spec);
        store.set({ error: "", cameras: await api.cameras() });
      } catch (e) { fail(e); } finally { apply.disabled = false; }
    });
    clear.addEventListener("click", async () => {
      clear.disabled = true;
      try {
        await api.clearFaults(id);
        store.set({ error: "", cameras: await api.cameras() });
      } catch (e) { fail(e); } finally { clear.disabled = false; }
    });

    drawer.replaceChildren(
      el("div", { class: "row" }, [el("h2", { id: "drawer-title", text: "Faults on " + id }), el("span", { class: "grow" }), closeBtn]),
      active,
      el("label", {}, ["Kind", kind]),
      models, params,
      el("div", { class: "row" }, [apply, clear]),
    );
  }

  // Keep Tab inside the drawer while it is open.
  drawer.addEventListener("keydown", (e) => {
    if (e.key === "Escape") { e.preventDefault(); close(); return; }
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
        opener = document.activeElement;
        await ensureCatalogue();
        render(st.faultsFor);
        drawer.hidden = false;
        const first = drawer.querySelector("#fault-kind");
        if (first) first.focus();
      } else {
        drawer.hidden = true;
        drawer.replaceChildren();
        if (opener && opener.isConnected) opener.focus();
        opener = null;
      }
    } else if ("cameras" in patch && st.faultsFor && !drawer.hidden) {
      // Refresh only the active-fault list so a half-filled form is not lost.
      const cam = st.cameras.find((c) => c.id === st.faultsFor);
      const box = drawer.querySelector(".active");
      if (!cam) close();
      else if (box) {
        box.replaceChildren(...((cam.faults || []).length
          ? cam.faults.map((f) => el("div", { class: "fault-active", text: "active: " + JSON.stringify(f) }))
          : [el("div", { class: "sub", text: "No active faults." })]));
      }
    }
  });
}
```

- [ ] **Step 6: Mount it**

In `main.js`, add `import { mount as mountFaults } from "./faults.js";` to the imports and, under the feature-module comment:

```js
mountFaults($("#drawer"), ctx);
```

- [ ] **Step 7: Run the tests**

Run: `go test -count=1 -run TestEmbeddedPageReferencesExist ./internal/dashboard/ && go test -tags e2e -count=1 -run 'TestApp' ./internal/dashboard/`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/dashboard/web internal/dashboard/e2e_app_test.go docs/superpowers/specs/2026-10-17-dashboard-redesign-design.md
git commit -m "dashboard: fault drawer driven by the catalogue"
```

---

### Task 4: Stream-quality button and result

**Files:**
- Modify: `internal/dashboard/web/js/cameras.js`, `internal/dashboard/e2e_app_test.go`

**Interfaces:**
- Consumes: `api.startQuality(id, {})` → `Run`, `api.getQuality(id)` → `Run`, `api.cancelQuality(id)`, store `qualityEvent` (`{camera, state, verdict}`), `quality` map. A `Run` is `{camera, state: "running"|"done"|"failed", request: {durationSeconds, ...}, startedAt, finishedAt?, verdict?: "ok"|"warn"|"error", analysis?: {tracks: [{type, jitter_ms, loss_percent, bitrate_kbps, frames?: {fps, keyframe_interval_ms}}], findings: [{severity, code, message}], error?}, error?}`. A camera DTO may carry `quality: {state, verdict}`; when it says `done` or `failed` and `store.quality[id]` is empty, fetch the run once with `api.getQuality`.
- Produces: no new exports. Each card gets a `.btn-quality` and fills `.qbox`.

- [ ] **Step 1: Write the failing e2e tests**

Append to `internal/dashboard/e2e_app_test.go`:

```go
func TestAppStreamQualityRunsAndShowsTheResult(t *testing.T) {
	url, _ := appServer(t, appOpts{quality: true})
	s := newSession(t)
	s.openApp(t, url)
	card := s.camCard("front-door")
	mustClick(t, card.Locator(".btn-quality"))
	if err := expect().Locator(card.Locator(".qbox")).ToContainText("testing"); err != nil {
		t.Fatalf("no running state: %v", err)
	}
	// The default window is 5 s.
	res := card.Locator(".qres.done")
	if err := expect().Locator(res).ToBeVisible(playwright.LocatorAssertionsToBeVisibleOptions{Timeout: playwright.Float(30000)}); err != nil {
		t.Fatalf("result never appeared: %v", err)
	}
	for _, label := range []string{"Frame rate", "Bitrate", "Jitter", "Packet loss", "Keyframe every"} {
		if err := expect().Locator(res).ToContainText(label); err != nil {
			t.Errorf("result lacks %q: %v", label, err)
		}
	}
	if err := expect().Locator(res.Locator(".pill").First()).ToHaveText(regexp.MustCompile(`(?i)healthy|warnings|errors`)); err != nil {
		t.Errorf("no verdict pill: %v", err)
	}
	s.requireClean(t)
}

func TestAppStreamQualityCanBeCancelled(t *testing.T) {
	url, _ := appServer(t, appOpts{quality: true})
	s := newSession(t)
	s.openApp(t, url)
	card := s.camCard("front-door")
	mustClick(t, card.Locator(".btn-quality"))
	cancel := card.Locator(".btn-quality-cancel")
	if err := expect().Locator(cancel).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	mustClick(t, cancel)
	if err := expect().Locator(card.Locator(".qbox")).Not().ToContainText("testing"); err != nil {
		t.Fatalf("still shows testing after cancel: %v", err)
	}
	if err := expect().Locator(card.Locator(".btn-quality")).ToBeEnabled(); err != nil {
		t.Fatal(err)
	}
	s.requireClean(t)
}

func TestAppStreamQualityButtonIsAbsentWhenNotEnabled(t *testing.T) {
	url, _ := appServer(t, appOpts{quality: false})
	s := newSession(t)
	s.openApp(t, url)
	if err := expect().Locator(s.camCard("front-door")).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	n, err := s.camCard("front-door").Locator(".btn-quality").Count()
	if err != nil || n != 0 {
		t.Fatalf("quality button shown although the server has it disabled (n=%d err=%v)", n, err)
	}
	s.requireClean(t)
}
```

Add `"regexp"` to the imports.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -tags e2e -count=1 -run 'TestAppStreamQuality' ./internal/dashboard/`
Expected: FAIL.

- [ ] **Step 3: Detect whether quality is enabled**

A server without quality answers `GET /cameras/{id}/quality` with 404 `stream quality tests are not enabled`, but a camera DTO only carries a `quality` field after a run. The UI needs a cheap probe: in `cameras.js` `mount`, before the first render, ask once.

Add near the top of `mount`:

```js
  // qualityEnabled is learned from one probe: "not enabled" is a 404 whose error
  // text says so; "no result yet" is a 404 with different text.
  let qualityEnabled = null; // null = unknown
  async function probeQuality(id) {
    if (qualityEnabled !== null) return qualityEnabled;
    try {
      await api.getQuality(id);
      qualityEnabled = true;
    } catch (e) {
      qualityEnabled = !/not enabled/i.test(e.message || "");
    }
    return qualityEnabled;
  }
```

- [ ] **Step 4: Add the quality UI to `buildCard`**

Inside `buildCard`, after the `faultsBtn` definition add:

```js
    const qBtn = el("button", { type: "button", class: "sm btn-quality", text: "Test stream quality", hidden: true });
    const qCancel = el("button", { type: "button", class: "ghost sm btn-quality-cancel", text: "Cancel test", hidden: true });
    let ticker = 0;

    function qRender() {
      const run = store.get().quality[id];
      clearInterval(ticker);
      qbox.replaceChildren();
      qBtn.disabled = false;
      qCancel.hidden = true;
      if (!run) return;
      if (run.state === "running") {
        qBtn.disabled = true;
        qCancel.hidden = false;
        const fill = el("div", { class: "bar-fill" });
        qbox.append(el("div", { class: "qres" }, [
          el("div", { class: "row" }, [el("b", { text: "Stream quality" }), el("span", { class: "grow" }), el("span", { class: "pill run", text: "testing…" })]),
          el("div", { class: "bar-track" }, [fill]),
        ]));
        const total = ((run.request && run.request.durationSeconds) || 5) * 1000;
        const started = Date.parse(run.startedAt) || Date.now();
        const tick = () => { fill.style.width = Math.min(100, ((Date.now() - started) / total) * 100) + "%"; };
        tick();
        ticker = setInterval(tick, 250);
        return;
      }
      if (run.state === "failed") {
        qbox.append(el("div", { class: "qres failed" }, [
          el("div", { class: "row" }, [el("b", { text: "Stream quality" }), el("span", { class: "grow" }), el("span", { class: "pill bad", text: "failed" })]),
          el("div", { text: run.error || "The test failed." }),
        ]));
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
        el("div", { class: "row" }, [el("b", { text: "Stream quality" }), el("span", { class: "grow" }), el("span", { class: "pill " + verdict[1], text: verdict[0] })]),
        dl,
        findings.length ? el("ul", {}, findings.map((f) => el("li", { text: f.message }))) : null,
      ]));
    }
    card.qRender = qRender;
```

This block references `card` before it exists. To keep the code straight, put the block *after* the `const card = {...}` definition instead, and have it use `card.qbox`. Concretely: place the `qBtn`, `qCancel`, `ticker`, `qRender` code after `card.update(cam);` and before `return card;`, and write `card.qRender = qRender;` there. Then add the buttons to the actions row:

```js
    card.actions.insertBefore(qBtn, card.actions.querySelector(".grow"));
    card.actions.insertBefore(qCancel, card.actions.querySelector(".grow"));
```

and wire the handlers (also after `card` exists):

```js
    qBtn.addEventListener("click", async () => {
      qBtn.disabled = true;
      try {
        const run = await api.startQuality(id, {});
        store.set({ quality: { ...store.get().quality, [id]: run } });
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
        store.set({ quality: { ...store.get().quality, [id]: run } });
      } catch (e) {
        fail(e);
      } finally {
        qCancel.disabled = false;
      }
    });
    card.qBtn = qBtn;
    probeQuality(id).then((on) => { qBtn.hidden = !on; });
    qRender();
```

Replace the earlier `actions` construction's reference: `actions` is already exposed on `card` as `actions`.

- [ ] **Step 5: React to events and store changes**

In `cameras.js` `mount`, extend the `store.subscribe` callback:

```js
    if ("quality" in patch) for (const c of cards.values()) c.qRender();
    if ("qualityEvent" in patch && st.qualityEvent) {
      const ev = st.qualityEvent;
      if (cards.has(ev.camera) && ev.state !== "running") {
        api.getQuality(ev.camera).then((run) => {
          store.set({ quality: { ...store.get().quality, [ev.camera]: run } });
        }).catch(fail);
      }
    }
```

And in `render(cameras)`, after `card.update(cam)` in both branches, restore a finished result that happened before this page loaded:

```js
      const q = cam.quality;
      if (q && q.state !== "running" && !store.get().quality[cam.id]) {
        api.getQuality(cam.id).then((run) => {
          store.set({ quality: { ...store.get().quality, [cam.id]: run } });
        }).catch(() => {});
      }
```

- [ ] **Step 6: Run the tests**

Run: `go test -count=1 -run TestEmbeddedPageReferencesExist ./internal/dashboard/ && go test -tags e2e -count=1 -run 'TestApp' ./internal/dashboard/`
Expected: PASS. If `TestAppStreamQualityRunsAndShowsTheResult` fails on a missing label, read the actual `GET /api/v1/cameras/front-door/quality` output (`curl` against a running binary) and fix the field names in `qRender`, not the test.

- [ ] **Step 7: Commit**

```bash
git add internal/dashboard/web internal/dashboard/e2e_app_test.go
git commit -m "dashboard: stream-quality test button and inline result"
```

---

### Task 5: Library tab

**Files:**
- Create: `internal/dashboard/web/js/library.js`
- Modify: `internal/dashboard/web/js/main.js`, `internal/dashboard/e2e_app_test.go`

**Interfaces:**
- Consumes: `api.library()`, `api.scan(root, force)`, `api.upload(file, onProgress)`, `api.deleteFile(id)`, `api.addRoot(path)`, store `library`, `addRequest`. The library view is `{roots: [{path, writable, ui}], uploadEnabled, allowUIPaths, maxUpload, scanning, files: [{id, root, path, name, size, modified, report: {status: "valid"|"warning"|"invalid", facts: {container, codec, width, height, fps, durationSeconds, ...}, issues: [{code, severity: "warning"|"invalid", message, detail?}]}, inUseBy: []}], scanErrors: []}`. A scan, add-root and upload-then-`GET` all return or yield this view.
- Produces: `mount(root, ctx)` in `library.js`; sets `store.set({addRequest: {libraryId}})` for "Use for camera", and the tab count `#count-library` (issues = files with status other than `valid`).

- [ ] **Step 1: Write the failing e2e tests**

Append to `internal/dashboard/e2e_app_test.go`:

```go
// libraryFixture builds a library over a temp dir seeded with a valid file, a
// file that reports B-frames and out-of-order timestamps, and an invalid file.
// The Analyze seam gives the second file its issues; the other two are
// analysed for real, so "Use for camera" really serves the valid file.
func libraryFixture(t *testing.T, opt library.Options) (*library.Library, string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("clean.ts", fixture.Bytes())
	write("bframes.ts", fixture.Bytes())
	write("broken.mp4", []byte("this is not a video"))
	opt.Roots = append([]string{dir}, opt.Roots...)
	opt.Analyze = func(path string) media.Report {
		r := media.AnalyzeFile(path)
		if filepath.Base(path) == "bframes.ts" {
			r.Issues = append(r.Issues,
				media.Issue{Code: "b_frames", Severity: media.SeverityWarning, Message: "The video uses B-frames.", Detail: "Decode order differs from display order."},
				media.Issue{Code: "dts_not_monotonic", Severity: media.SeverityWarning, Message: "Decode timestamps go backwards or repeat."})
			r.Status = media.StatusWarning
		}
		return r
	}
	lib, err := library.New(opt)
	if err != nil {
		t.Fatal(err)
	}
	return lib, dir
}

func libRow(s *session, name string) playwright.Locator {
	return s.page.Locator(fmt.Sprintf(`#files tbody tr[data-name=%q]`, name))
}

func openLibrary(t *testing.T, s *session, url string) {
	t.Helper()
	s.openApp(t, url)
	mustClick(t, s.page.Locator(`button.tab[data-tab="library"]`))
}

func TestAppLibraryShowsEveryFilesStatusAndFindings(t *testing.T) {
	lib, _ := libraryFixture(t, library.Options{})
	if err := lib.Scan(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	url, _ := appServer(t, appOpts{lib: lib})
	s := newSession(t)
	openLibrary(t, s, url)

	if err := expect().Locator(libRow(s, "clean.ts").Locator(".status")).ToHaveText("Valid"); err != nil {
		t.Fatal(err)
	}
	b := libRow(s, "bframes.ts")
	if err := expect().Locator(b.Locator(".status")).ToHaveText("Valid, with warnings"); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(b).ToContainText("The video uses B-frames."); err != nil {
		t.Fatalf("B-frames finding not spelled out: %v", err)
	}
	if err := expect().Locator(b).ToContainText("Decode timestamps go backwards"); err != nil {
		t.Fatalf("timestamp finding not spelled out: %v", err)
	}
	if err := expect().Locator(libRow(s, "broken.mp4").Locator(".status")).ToHaveText("Invalid"); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(libRow(s, "broken.mp4").Locator(".btn-use")).ToBeDisabled(); err != nil {
		t.Fatal("an invalid file can be used for a camera")
	}
	// Filter chips with counts.
	if err := expect().Locator(s.page.Locator(`.stat[data-f="all"] b`)).ToHaveText("3"); err != nil {
		t.Fatal(err)
	}
	mustClick(t, s.page.Locator(`.stat[data-f="invalid"]`))
	if err := expect().Locator(s.page.Locator("#files tbody tr")).ToHaveCount(1); err != nil {
		t.Fatalf("invalid filter: %v", err)
	}
	if err := expect().Locator(s.page.Locator("#count-library")).ToHaveText("2"); err != nil {
		t.Fatalf("tab count of files needing attention: %v", err)
	}
	s.requireClean(t)
}

func TestAppLibraryRescanFindsNewFilesAndRefreshDoesNotValidate(t *testing.T) {
	lib, dir := libraryFixture(t, library.Options{})
	if err := lib.Scan(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	url, _ := appServer(t, appOpts{lib: lib})
	s := newSession(t)
	openLibrary(t, s, url)
	if err := expect().Locator(libRow(s, "clean.ts")).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dropped.ts"), fixture.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	mustClick(t, s.page.Locator("#lib-refresh"))
	time.Sleep(500 * time.Millisecond)
	if n, _ := libRow(s, "dropped.ts").Count(); n != 0 {
		t.Fatal("Refresh validated a new file; only Rescan should")
	}
	mustClick(t, s.page.Locator("#lib-rescan"))
	if err := expect().Locator(libRow(s, "dropped.ts").Locator(".status")).ToHaveText("Valid"); err != nil {
		t.Fatalf("Rescan did not show the new file: %v", err)
	}
	s.requireClean(t)
}

func TestAppLibraryUploadShowsTheValidatedFile(t *testing.T) {
	// The first root is writable, so uploads are enabled.
	lib, err := library.New(library.Options{Roots: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	url, _ := appServer(t, appOpts{lib: lib})
	s := newSession(t)
	openLibrary(t, s, url)
	good := filepath.Join(t.TempDir(), "uploaded-good.ts")
	bad := filepath.Join(t.TempDir(), "uploaded-bad.mp4")
	if err := os.WriteFile(good, fixture.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.page.Locator("#file-input").SetInputFiles([]string{good}); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(libRow(s, "uploaded-good.ts").Locator(".status")).ToHaveText("Valid"); err != nil {
		t.Fatalf("good upload: %v", err)
	}
	if err := s.page.Locator("#file-input").SetInputFiles([]string{bad}); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(libRow(s, "uploaded-bad.mp4").Locator(".status")).ToHaveText("Invalid"); err != nil {
		t.Fatalf("bad upload: %v", err)
	}
	s.requireClean(t)
}

func TestAppLibraryUseForCameraPreselectsTheFileAndStreams(t *testing.T) {
	lib, _ := libraryFixture(t, library.Options{})
	if err := lib.Scan(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	url, f := appServer(t, appOpts{lib: lib})
	s := newSession(t)
	openLibrary(t, s, url)
	mustClick(t, libRow(s, "clean.ts").Locator(".btn-use"))
	if err := expect().Locator(s.page.Locator("#add-form")).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(s.page.Locator("#add-source")).ToHaveValue("library"); err != nil {
		t.Fatal(err)
	}
	opt, err := s.page.Locator("#add-lib option:checked").TextContent()
	if err != nil || !strings.Contains(opt, "clean.ts") {
		t.Fatalf("file not preselected: %q err=%v", opt, err)
	}
	mustFill(t, s.page.Locator("#add-id"), "from-file")
	mustClick(t, s.page.Locator("#add-submit"))
	if err := expect().Locator(s.camCard("from-file")).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Camera("from-file"); err != nil {
		t.Fatalf("camera not in the fleet: %v", err)
	}
	// The file now shows who uses it, and cannot be deleted.
	mustClick(t, s.page.Locator(`button.tab[data-tab="library"]`))
	mustClick(t, s.page.Locator("#lib-refresh"))
	if err := expect().Locator(libRow(s, "clean.ts")).ToContainText("from-file"); err != nil {
		t.Fatalf("in-use camera not shown: %v", err)
	}
	if err := expect().Locator(libRow(s, "clean.ts").Locator(".btn-del")).ToBeDisabled(); err != nil {
		t.Fatal("a file in use can be deleted")
	}
	s.requireClean(t)
}

func TestAppLibraryAddSharedPath(t *testing.T) {
	lib, _ := libraryFixture(t, library.Options{AllowUIPaths: true})
	extra := t.TempDir()
	if err := os.WriteFile(filepath.Join(extra, "extra.ts"), fixture.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	url, _ := appServer(t, appOpts{lib: lib})
	s := newSession(t)
	openLibrary(t, s, url)
	mustFill(t, s.page.Locator("#root-path"), extra)
	mustClick(t, s.page.Locator(`#root-form button[type="submit"]`))
	if err := expect().Locator(libRow(s, "extra.ts").Locator(".status")).ToHaveText("Valid"); err != nil {
		t.Fatalf("shared path not scanned: %v", err)
	}
	mustFill(t, s.page.Locator("#root-path"), "/does/not/exist")
	mustClick(t, s.page.Locator(`#root-form button[type="submit"]`))
	if err := expect().Locator(s.page.Locator("#error")).ToContainText("does not exist"); err != nil {
		t.Fatalf("bad path error not shown: %v", err)
	}
	s.requireClean(t, 400)
}

// Review Focus 3.
func TestAppLibraryHidesDisabledControlsWithAReason(t *testing.T) {
	lib, _ := libraryFixture(t, library.Options{}) // no writable root, no UI paths
	url, _ := appServer(t, appOpts{lib: lib})
	s := newSession(t)
	openLibrary(t, s, url)
	if err := expect().Locator(s.page.Locator("#drop")).ToHaveCount(0); err != nil {
		t.Fatal("upload area shown although uploads are disabled")
	}
	if err := expect().Locator(s.page.Locator("#root-form")).ToHaveCount(0); err != nil {
		t.Fatal("add-path form shown although UI paths are disabled")
	}
	if err := expect().Locator(s.page.Locator(".lib-note")).ToContainText("Uploads are off"); err != nil {
		t.Fatalf("no reason for the missing upload area: %v", err)
	}
	if err := expect().Locator(s.page.Locator(".lib-note")).ToContainText("Adding paths is off"); err != nil {
		t.Fatalf("no reason for the missing path form: %v", err)
	}
	s.requireClean(t)
}

func TestAppLibraryEmptyAndInvalidOnly(t *testing.T) {
	dir := t.TempDir()
	lib, err := library.New(library.Options{Roots: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	url, _ := appServer(t, appOpts{lib: lib})
	s := newSession(t)
	openLibrary(t, s, url)
	if err := expect().Locator(s.page.Locator("#lib-empty")).ToContainText("No video files"); err != nil {
		t.Fatalf("empty library not explained: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustClick(t, s.page.Locator("#lib-rescan"))
	if err := expect().Locator(libRow(s, "a.mp4").Locator(".status")).ToHaveText("Invalid"); err != nil {
		t.Fatal(err)
	}
	mustClick(t, s.page.Locator(`button.tab[data-tab="cameras"]`))
	mustClick(t, s.page.Locator("#add-open"))
	mustSelect(t, s.page.Locator("#add-source"), "library")
	n, err := s.page.Locator("#add-lib option").Count()
	if err != nil || n != 0 {
		t.Fatalf("an invalid file is offered in the add form (n=%d err=%v)", n, err)
	}
	s.requireClean(t)
}

func TestAppLibraryDisabledServerShowsNoLibraryTab(t *testing.T) {
	url, _ := appServer(t, appOpts{})
	s := newSession(t)
	openLibrary(t, s, url)
	if err := expect().Locator(s.page.Locator("#panel-library")).ToContainText("not enabled"); err != nil {
		t.Fatalf("a server without a library should say so: %v", err)
	}
	s.requireClean(t, 404)
}

func TestAppLibraryDeleteAsksFirst(t *testing.T) {
	up := t.TempDir()
	lib, err := library.New(library.Options{Roots: []string{up}})
	if err != nil {
		t.Fatal(err)
	}
	url, _ := appServer(t, appOpts{lib: lib})
	s := newSession(t)
	openLibrary(t, s, url)
	f := filepath.Join(t.TempDir(), "gone.ts")
	if err := os.WriteFile(f, fixture.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.page.Locator("#file-input").SetInputFiles([]string{f}); err != nil {
		t.Fatal(err)
	}
	row := libRow(s, "gone.ts")
	if err := expect().Locator(row).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	mustClick(t, row.Locator(".btn-del"))
	if err := expect().Locator(row).ToBeVisible(); err != nil {
		t.Fatal("deleted without confirming")
	}
	mustClick(t, row.Locator(".btn-del-confirm"))
	if err := expect().Locator(row).ToHaveCount(0); err != nil {
		t.Fatalf("file still listed after delete: %v", err)
	}
	s.requireClean(t)
}
```

Add a helper and imports. Helper:

```go
func mustSelect(t *testing.T, l playwright.Locator, v string) {
	t.Helper()
	if _, err := l.SelectOption(playwright.SelectOptionValues{Values: &[]string{v}}); err != nil {
		t.Fatal(err)
	}
}
```

Imports to add: `"context"`, `"os"`, `"path/filepath"`, `"strings"`, `"github.com/0x524a/camfarm/internal/media"`, `"github.com/0x524a/camfarm/internal/media/fixture"`.

Note: table rows carry `data-name` (file name) as well as `data-id`, because the library file id is a hash the tests cannot predict.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -tags e2e -count=1 -run 'TestAppLibrary' ./internal/dashboard/`
Expected: FAIL (empty panel).

- [ ] **Step 3: Write `js/library.js`**

```js
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

    rootForm.hidden = !lib.allowUIPaths;
    drop.hidden = !lib.uploadEnabled;
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
```

The empty-state sentence contains "No video files", which the empty-library test asserts. The disabled-server message contains "not enabled", which `TestAppLibraryDisabledServerShowsNoLibraryTab` asserts. The note strings contain "Uploads are off" and "Adding paths is off", which `TestAppLibraryHidesDisabledControlsWithAReason` asserts.

- [ ] **Step 4: Mount it**

In `main.js`, add `import { mount as mountLibrary } from "./library.js";` to the imports and, under the feature-module comment:

```js
mountLibrary($("#panel-library"), ctx);
```

- [ ] **Step 5: Run the tests**

Run: `go test -count=1 -run TestEmbeddedPageReferencesExist ./internal/dashboard/ && go test -tags e2e -count=1 -run 'TestApp' ./internal/dashboard/`
Expected: PASS. If `TestAppLibraryAddSharedPath` fails because the typed path error text differs, read the actual 400 body and match the assertion to the server's wording in `internal/dashboard/library.go` (`that path does not exist or is not readable`).

- [ ] **Step 6: Commit**

```bash
git add internal/dashboard/web internal/dashboard/e2e_app_test.go
git commit -m "dashboard: library tab with validation findings, upload and rescan"
```

---

### Task 6: Metrics strip

**Files:**
- Create: `internal/dashboard/web/js/metrics.js`
- Modify: `internal/dashboard/web/js/main.js`, `internal/dashboard/e2e_app_test.go`

**Interfaces:**
- Consumes: `api.metrics()` → `Sample`, store `metrics`, `conn`. A `Sample` is `{time, cpu: {available, reason?, usagePercent|null, logicalCPUs, load1, load5, load15, limitCPUs|null}, memory: {available, reason?, totalBytes, usedBytes, availableBytes, swapTotalBytes, swapUsedBytes, limitBytes|null}, gpu: {available, reason?, devices: [{index, name, uuid, gpuUtilPercent|null, memUtilPercent|null, memUsedBytes|null, memTotalBytes|null, encoderUtilPercent|null, decoderUtilPercent|null, temperatureC|null, powerWatts|null, powerLimitWatts|null}]}, process: {available, reason?, cpuPercent|null, rssBytes, goroutines, openFDs}}`.
- Produces: `mount(root, ctx)` in `metrics.js`, where `root` is `#metrics`. Adds class `stale` to `#metrics` while `conn !== "ok"`.

- [ ] **Step 1: Write the failing e2e tests**

Append to `internal/dashboard/e2e_app_test.go`:

```go
const gpuRowApp = "0, NVIDIA GeForce RTX 4090, GPU-aaaa-1111, 37, 12, 1024, 24564, 5, 0, 54, 78.43, 450.00\n"

// sampler builds a hostmetrics sampler over a fake /proc with a fake nvidia-smi
// and a manual tick channel, so a test controls exactly when a sample lands.
func sampler(t *testing.T, run hostmetrics.Runner) (*hostmetrics.Sampler, chan time.Time) {
	t.Helper()
	root := procFixture(t)
	ticks := make(chan time.Time)
	s := hostmetrics.New(hostmetrics.Options{
		ProcRoot: filepath.Join(root, "proc"), CgroupRoot: filepath.Join(root, "cg"),
		Run: run, Ticks: ticks,
	})
	t.Cleanup(s.Close)
	return s, ticks
}

func gpuOKApp(context.Context, string, ...string) ([]byte, []byte, error) {
	return []byte(gpuRowApp), nil, nil
}

func gpuBrokenApp(context.Context, string, ...string) ([]byte, []byte, error) {
	return nil, []byte("Failed to initialize NVML: Driver/library version mismatch\n"), errors.New("exit status 18")
}

func TestAppMetricsStripShowsHostAndGPU(t *testing.T) {
	s0, _ := sampler(t, gpuOKApp)
	url, _ := appServer(t, appOpts{metrics: s0})
	s := newSession(t)
	s.openApp(t, url)
	cpu := s.page.Locator(`#metrics .tile[data-tile="cpu"]`)
	mem := s.page.Locator(`#metrics .tile[data-tile="memory"]`)
	gpu := s.page.Locator(`#metrics .tile[data-tile="gpu-0"]`)
	proc := s.page.Locator(`#metrics .tile[data-tile="process"]`)
	for name, l := range map[string]playwright.Locator{"cpu": cpu, "memory": mem, "gpu": gpu, "process": proc} {
		if err := expect().Locator(l).ToBeVisible(); err != nil {
			t.Fatalf("%s tile missing: %v", name, err)
		}
	}
	if err := expect().Locator(gpu).ToContainText("GPU busy"); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(gpu).ToContainText("37"); err != nil {
		t.Fatalf("GPU utilisation not shown: %v", err)
	}
	if err := expect().Locator(gpu).ToContainText("RTX 4090"); err != nil {
		t.Fatalf("GPU name not shown: %v", err)
	}
	if err := expect().Locator(mem).ToContainText("GB"); err != nil {
		t.Fatalf("memory not shown in GB: %v", err)
	}
	s.requireClean(t)
}

// Review Focus 4: an unavailable GPU shows its reason; a null value shows "-";
// the other tiles keep their numbers.
func TestAppMetricsUnavailableGPUShowsReasonAndLeavesTheRestAlone(t *testing.T) {
	s0, _ := sampler(t, gpuBrokenApp)
	url, _ := appServer(t, appOpts{metrics: s0})
	s := newSession(t)
	s.openApp(t, url)
	gpu := s.page.Locator(`#metrics .tile[data-tile="gpu"]`)
	if err := expect().Locator(gpu.Locator(".tile-reason")).ToContainText("Driver/library version mismatch"); err != nil {
		t.Fatalf("GPU reason not shown: %v", err)
	}
	if err := expect().Locator(s.page.Locator(`#metrics .tile[data-tile="memory"] .tile-value`)).ToContainText("GB"); err != nil {
		t.Fatalf("memory tile affected by the GPU failure: %v", err)
	}
	// The first sample has no CPU baseline: null renders as "-", never "0%".
	val, err := s.page.Locator(`#metrics .tile[data-tile="cpu"] .tile-value`).TextContent()
	if err != nil || strings.Contains(val, "0%") && !strings.Contains(val, "-") {
		t.Fatalf("a null CPU reading rendered as zero: %q err=%v", val, err)
	}
	s.requireClean(t)
}

func TestAppMetricsStripNeverSaysSMOccupancy(t *testing.T) {
	s0, _ := sampler(t, gpuOKApp)
	url, _ := appServer(t, appOpts{metrics: s0})
	s := newSession(t)
	s.openApp(t, url)
	if err := expect().Locator(s.page.Locator(`#metrics .tile[data-tile="gpu-0"]`)).ToContainText("kernel-busy time"); err != nil {
		t.Fatal(err)
	}
	txt, err := s.page.Locator("#metrics").TextContent()
	if err != nil || strings.Contains(strings.ToLower(txt), "sm ") {
		t.Fatalf("the strip claims SM occupancy: %q err=%v", txt, err)
	}
	s.requireClean(t)
}

// Review Focus 5: when the stream drops the strip dims and says stale.
func TestAppMetricsStripGoesStaleWhenTheStreamDrops(t *testing.T) {
	s0, _ := sampler(t, gpuOKApp)
	f := camfarm.StartT(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "front-door"}}})
	ts := httptest.NewServer(Handler(f, Options{Metrics: s0}))
	defer ts.Close()
	s := newSession(t)
	s.openApp(t, ts.URL)
	if err := expect().Locator(s.page.Locator("#metrics.stale")).ToHaveCount(0); err != nil {
		t.Fatal(err)
	}
	ts.CloseClientConnections()
	ts.Close()
	if err := expect().Locator(s.page.Locator("#conn")).ToHaveText("disconnected, retrying"); err != nil {
		t.Fatalf("pill did not change: %v", err)
	}
	if err := expect().Locator(s.page.Locator("#metrics.stale")).ToHaveCount(1); err != nil {
		t.Fatalf("strip not marked stale: %v", err)
	}
}

func TestAppMetricsAbsentWhenTheServerHasNoSampler(t *testing.T) {
	url, _ := appServer(t, appOpts{})
	s := newSession(t)
	s.openApp(t, url)
	if err := expect().Locator(s.camCard("front-door")).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(s.page.Locator("#metrics")).ToContainText("not enabled"); err != nil {
		t.Fatalf("a server without a sampler should say so: %v", err)
	}
	s.requireClean(t, 404)
}
```

Imports to add: `"context"`, `"errors"`, `"path/filepath"`, `"strings"`, `"time"` (some already present from earlier tasks; keep each once). `procFixture` is defined in `metrics_test.go` (no build tag), so it is available to the e2e build.

Note on the GPU-broken test: the `gpu` tile has `data-tile="gpu"` when the whole section is unavailable (no devices to index), and `gpu-<index>` per device when available.

- [ ] **Step 2: Run to verify they fail**

Run: `go test -tags e2e -count=1 -run 'TestAppMetrics' ./internal/dashboard/`
Expected: FAIL.

- [ ] **Step 3: Write `js/metrics.js`**

```js
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

  function setStale(on) {
    root.classList.toggle("stale", on);
  }

  store.subscribe((st, patch) => {
    if ("metrics" in patch && st.metrics && enabled) render(st.metrics);
    if ("conn" in patch) setStale(st.conn === "lost");
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
```

- [ ] **Step 4: Mount it**

In `main.js`, add `import { mount as mountMetrics } from "./metrics.js";` to the imports and, under the feature-module comment:

```js
mountMetrics($("#metrics"), ctx);
```

- [ ] **Step 5: Run the tests**

Run: `go test -count=1 -run TestEmbeddedPageReferencesExist ./internal/dashboard/ && go test -tags e2e -count=1 -run 'TestApp' ./internal/dashboard/`
Expected: PASS. In `TestAppMetricsStripShowsHostAndGPU` the GPU tile text contains `37`. In `TestAppMetricsStripGoesStaleWhenTheStreamDrops`, if the pill takes longer than the assertion timeout to flip because `EventSource` keeps retrying quietly, check that `es.onerror` fires on a closed server; the browser calls `onerror` on each failed reconnect.

- [ ] **Step 6: Commit**

```bash
git add internal/dashboard/web internal/dashboard/e2e_app_test.go
git commit -m "dashboard: host metrics strip with unavailable and stale states"
```

---

### Task 7: Cutover, port the old e2e tests, README

**Files:**
- Modify: `internal/dashboard/e2e_app_test.go` (`appPath`), `internal/dashboard/e2e_test.go` (port or remove old-page tests), `internal/dashboard/embed_test.go`, `README.md`, `docs/superpowers/specs/2026-10-17-dashboard-redesign-design.md` (status)
- Rename: `internal/dashboard/web/app.html` → `internal/dashboard/web/index.html` (replaces the old file)

**Interfaces:**
- Consumes: everything from Tasks 1 to 6.
- Produces: the redesigned page served at `/`; no old-page selectors remain.

- [ ] **Step 1: List which old tests have a new equivalent**

The old tests in `internal/dashboard/e2e_test.go` map as follows. Keep this table while editing.

| Old test | Disposition |
|---|---|
| `TestE2ECameraTableRendersURLsAndReplayLine` | Covered by `TestAppShellShowsConnectionAndReplayLine` and `TestAppCameraCardShowsThumbnailAndURLs`; delete |
| `TestE2EAddAndRemoveACamera` | Covered by `TestAppAddAndRemoveACameraWithInlineConfirm`; delete |
| `TestE2EDuplicateCameraShowsAnError` | Covered by `TestAppDuplicateCameraShowsAnError`; delete |
| `TestE2ESnapshotShowsA320x240Image` | Covered by `TestAppCameraCardShowsThumbnailAndURLs`; delete |
| `TestE2EFrameDropControlInjectsAndClears` | Covered by `TestAppFaultDrawerAppliesAndClearsFrameDrop`; delete |
| `TestE2EUnimplementedFaultKindsAreDisabled` | Covered by `TestAppUnimplementedFaultKindsAreDisabled`; delete |
| `TestE2ECopyButtonCopiesTheRTSPURL` | Port (below) |
| `TestE2ENarrowViewportDoesNotScrollTheWholePage` | Covered; delete |
| `TestE2EBothColourSchemesRender` | Covered; delete |
| `TestE2EFleetChangeAppearsWithoutReloadOrPoll` | Covered by `TestAppFleetChangeAppearsWithoutReload`; delete |
| `TestE2EPrototypeNamedCameraIDsRenderAndRemove` | Covered by `TestAppCameraIDsAreTextAndPrototypeNamesWork`; delete |
| `TestE2EFaultFormFollowsTheCatalogue` | Port (below) |
| `TestE2EApplyAndClearPacketLossAndKeyframeStarvation` | Port (below) |
| `TestE2EEnumParameterAppliesRtspError` | Port (below) |
| `TestE2EApplyAndClearJitterThroughTheGenericForm` | Port (below) |
| `TestE2EPTSDelayedWithoutParametersShowsTheServerError` | Port (below) |
| `TestE2EEnumSelectKindsRenderInBothColourSchemes` | Port (below) |

Before deleting, read each "Port" test in `e2e_test.go` (lines 456 onward) and keep its server-side assertions (what the fleet reports after the action) unchanged.

- [ ] **Step 2: Swap the files**

```bash
git mv -f internal/dashboard/web/app.html internal/dashboard/web/index.html
```

Run `git status` and confirm `index.html` shows as modified (the old content replaced) and `app.html` is gone. In `internal/dashboard/e2e_app_test.go` change:

```go
const appPath = "/"
```

In `internal/dashboard/embed_test.go` replace the page loop's list with `[]string{"index.html"}` and remove the `continue // app.html ...` fallback (a missing `index.html` must now fail the test):

```go
	b, err := fs.ReadFile(web, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range htmlRef.FindAllStringSubmatch(string(b), -1) {
		if _, err := fs.Stat(web, m[2]); err != nil {
			t.Errorf("index.html references %q, which is not embedded", m[2])
		}
	}
```

- [ ] **Step 3: Port the remaining old tests**

For each "Port" row, rewrite the test in `internal/dashboard/e2e_app_test.go` against the new selectors, using this mapping:

| Old | New |
|---|---|
| `s.open(t, url)` | `s.openApp(t, url)` |
| `s.row(id)` | `s.camCard(id)` |
| `#error` | `#error` (unchanged) |
| fault kind select in the row | open the drawer with `.btn-faults`, then `#fault-kind` |
| `.fault-param` inputs in the row | `.fault-param[data-name="…"]` in `#drawer` |
| apply / clear buttons in the row | `#fault-apply` / `#fault-clear` |
| copy button | the `Copy` button in the card's first `.url` |

The ported copy test, as the pattern (grant clipboard permissions as the old one did):

```go
func TestAppCopyButtonCopiesTheRTSPURL(t *testing.T) {
	url, f := appServer(t, appOpts{})
	s := newSession(t, playwright.BrowserNewContextOptions{
		Permissions: []string{"clipboard-read", "clipboard-write"},
	})
	s.openApp(t, url)
	card := s.camCard("front-door")
	mustClick(t, card.Locator(".url button").First())
	got, err := s.page.Evaluate(`navigator.clipboard.readText()`)
	if err != nil {
		t.Fatal(err)
	}
	cam, _ := f.Camera("front-door")
	if got != cam.RTSPURL() {
		t.Fatalf("clipboard = %q, want %q", got, cam.RTSPURL())
	}
	s.requireClean(t)
}
```

Each remaining ported test keeps the old test's name with `E2E` replaced by `App`, and its original assertions about fleet state.

- [ ] **Step 4: Delete the old tests and helpers**

Delete the "covered" tests and the ported originals from `internal/dashboard/e2e_test.go`. Keep `launch`, `TestMain`, `session`, `newSession`, `requireClean`, `expect`, `assertTimeoutMS`. Delete `e2eServer`, `(*session).open` and `(*session).row` once nothing uses them (`go vet -tags e2e ./internal/dashboard/` reports unused imports; remove them).

- [ ] **Step 5: Run everything**

Run:

```bash
go build ./... && CGO_ENABLED=0 go build ./...
go vet ./... && go vet -tags e2e ./internal/dashboard/
gofmt -l .
go test -count=1 ./...
go test -tags e2e -count=1 ./internal/dashboard/...
```

Expected: no output from `gofmt -l`, all tests PASS. Then start the binary and fetch the page and a module:

```bash
go run ./cmd/camfarm --help
```

and confirm the dashboard serves: run the binary the way the README does, then `curl -s -o /dev/null -w '%{http_code} %{content_type}\n' http://127.0.0.1:<port>/js/main.js` prints `200 text/javascript; charset=utf-8`.

- [ ] **Step 6: README**

In `README.md`, in the Status "Works" list, replace the dashboard bullet(s) with one that states what the dashboard does now: camera snapshot thumbnails, the host metrics strip, the library tab with validation findings, upload and shared paths, the fault drawer and the stream-quality test. Do not add any coverage figure or screenshot. Keep the existing sentences that the README's own tests (`readme_test.go`) check. The README's one-line statement is `**Simulated fleet for pipeline testing. Not an ONVIF conformance tool.**` (already added under the badges; leave it as is). The dashboard page carries no conformance statement; do not add one. Leave the README's "Scope / what this does not do" section and the ONVIF® notice in Status unchanged. Add to the "Scope / what this does not do" or security notes one sentence: the dashboard has no authentication and exposes host details through the metrics strip. Then run `go test -count=1 -run README ./...` (the repo's `readme_test.go`) and fix anything it flags.

- [ ] **Step 7: Update the spec status line**

In `docs/superpowers/specs/2026-10-17-dashboard-redesign-design.md` change `Status: design, awaiting review.` to `Status: implemented.` and add the line `Plan: docs/superpowers/plans/2026-10-17-dashboard-redesign.md` under it.

- [ ] **Step 8: Commit**

```bash
git add -A internal/dashboard README.md docs/superpowers
git commit -m "dashboard: serve the redesigned page at / and port the e2e suite"
```
