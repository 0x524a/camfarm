# Dashboard redesign

Status: implemented.
Plan: docs/superpowers/plans/2026-10-17-dashboard-redesign.md

Sub-project 3 of 3 of the dashboard work
(1: media library, merged; 2: host metrics, PR #17; this one: the UI that uses both,
plus the stream-quality API).

## Intent

The operator of a farm needs to see, on one page, which cameras exist and what each
one looks like, how loaded the host is, which video files are usable and which have
problems, and how a stream actually performs. The current page is a single table of
cameras and shows none of that. This sub-project replaces it.

Success: a user can identify every camera by a thumbnail, read host CPU, memory, GPU
and process load, see the validation status of every library file with each problem
spelled out, upload a file or share a directory, rescan on demand, create a camera
from a chosen file, and run a stream-quality test and read its result, all from the
browser.

Decisions taken with the user: vanilla JS with no build step (A); thumbnails fetched
on load and on add, with a manual refresh (A); fault controls in a side drawer (A); the
library refreshes on click only, with an explicit Refresh button (C); the old page is
replaced and its e2e tests rewritten (A); authentication is out of scope.

## Non-goals

- Authentication, sessions, roles. The dashboard stays unauthenticated and not
  protected against DNS rebinding; the README says so again. `api.js` is the only place
  that builds requests, so a credential can later be added in one place.
- Server API changes. Every endpoint used here exists. The server change is limited to
  serving the new static files.
- Server-side metric history, charts beyond a short in-page sparkline, per-camera CPU.
- Pagination of the library table; the filter chips are the mitigation.
- A JS toolchain, a JS unit-test runner, or any new dependency.
- A claim about GPU numbers on live hardware. The GPU tile is verified against captured
  `nvidia-smi` output only.

## Files

All under `internal/dashboard/web/`, embedded with `go:embed`, served as static files.

| File | Job |
|---|---|
| `index.html` | Shell: header, metrics strip, tabs, drawer mount point |
| `app.css` | Tokens (light and dark), layout, components |
| `js/api.js` | `fetch` wrapper and error normalisation; the only module that knows URLs |
| `js/state.js` | Small store (cameras, library, metrics, catalogue) with subscribe |
| `js/dom.js` | Element builder, number and byte formatters, sparkline, clipboard helper |
| `js/metrics.js` | Metrics strip |
| `js/cameras.js` | Camera grid, thumbnails, add form, quality button and result |
| `js/faults.js` | Fault drawer |
| `js/library.js` | Library tab |
| `js/main.js` | Wiring, tabs, connection pill |

Each feature module exports `mount(root, store, api)`. Feature modules do not import
each other; they communicate through the store. Server text is always inserted as text,
never as HTML.

## Data flow

- **Fleet:** SSE events `camera_added`, `camera_removed`, `fault_injected`,
  `fault_cleared` trigger a debounced re-list, as today. The connection pill reflects the
  stream state; polling failures go to the pill and never to the action error region.
- **Metrics:** the stream is opened as `/api/v1/events?metrics=1` and `event: metrics`
  updates the strip. First paint comes from `GET /api/v1/metrics`. When the stream is
  down the strip dims and says "stale"; it never shows old numbers as current.
- **Thumbnails:** one `GET /cameras/{id}/snapshot.jpg` per camera on load and on add,
  held as a blob URL and revoked on removal or replacement. "Refresh snapshot" fetches
  again. There is no timed refresh.
- **Library:** no events and no polling. `POST /library/scan`, `POST /library/upload`
  and `POST /library/roots` run to completion and return the updated library view, which
  replaces the store's copy. "Refresh" is `GET /library`; it re-reads the cache and does
  not validate. "Rescan" (per root, and all) is what validates new files.
- **Quality:** the button posts `POST /cameras/{id}/quality`; the result panel reads
  `GET .../quality` and follows the quality event stream for progress. Cancel is
  `DELETE .../quality`.
- **Faults:** the drawer reads `GET /faults` once and posts to the existing inject and
  clear endpoints.

## Screens

### Header and metrics strip

Tiles: CPU, memory, GPU busy, GPU memory (one pair per device when there are several,
labelled with the device name), and the camfarm process. Each has a sparkline of the last
40 samples, kept in the page only.

- A section with `available: false` shows its `reason` verbatim and an "unavailable"
  pill; other tiles are unaffected.
- A `null` value renders as "-", never 0.
- The GPU tile reads "GPU busy" with the note "kernel-busy time"; it never says SM
  occupancy.
- Cgroup limits (`limitCPUs`, `limitBytes`), when present, appear beside usage.

### Cameras tab

A card per camera: thumbnail, id, source file with codec and resolution, RTSP and ONVIF
URLs with copy buttons, fault chips, and the actions Refresh snapshot, Faults, Test stream
quality and Remove.

- A failed snapshot shows a placeholder with the error text.
- Remove asks inline ("Remove x? Confirm / Cancel").
- The add form offers a library file (valid files; files with warnings marked), a plain
  path where the server allows it, or the bundled fixture. "Use for camera" in the
  Library tab opens this form with the file preselected, sending `libraryId`.
- The quality result panel shows verdict, frame rate, bitrate, jitter, packet loss, keyframe
  interval and frame count (the fields rtspeek v1.0.0 reports), or the error text of a failed run. While running it
  shows progress and a Cancel button.

### Library tab

- Four filter chips with counts: All, Valid, With warnings, Invalid.
- Sources: each root with its file count and a Rescan button; "Rescan all"; Refresh; an
  "Add shared path" field, shown only when the server reports `allowUIPaths`. The upload
  area is shown only when `uploadEnabled`, with a one-line reason otherwise.
- Upload shows progress; the response carries the validated report, and the file appears
  with its status.
- A table row per file: name, root and size, a status pill, stream facts (codec,
  resolution, frame rate, duration), findings, cameras using it, and actions. Findings
  are written out in words, one line per issue (invalid container, unsupported codec,
  B-frames, PTS out of order, long keyframe gap), so the table reads without colour. A
  left severity stripe and the pill add the at-a-glance signal.
- "Use for camera" is disabled for invalid files. Delete is disabled while a camera uses
  the file.
- `scanErrors` appear in a banner above the table.

### Fault drawer

A side drawer per camera: kind picker, the kind's parameters from the catalogue, the
active faults, a "Clear faults" button (the API clears all of a camera's faults at once), and Apply. It traps focus, returns it to the
opening button on close, and closes on Escape.

## Errors

- Action errors go to one `role="alert"` region and are never overwritten by a
  background failure.
- Background failures (stream down, refresh failed) go to the connection pill.
- A non-2xx response shows the server's `error` text; a network failure shows a fixed
  message.

## Accessibility and layout

Tabs use `role="tablist"` with arrow-key navigation. Both themes are defined at the token
level and follow `prefers-color-scheme` unless `data-theme` is set. `prefers-reduced-motion`
is respected. The layout works at 400px with no horizontal page scroll; only the library
table scrolls inside its own container. Digits in tiles and tables use tabular figures.

## Testing

Playwright tests, in the existing `e2e` build tag and CI job, driving the real dashboard
server:

- The existing tests (add, remove, fault apply and clear, snapshot, event-stream
  reconnect, connection pill) are ported to the new selectors with their intent intact.
- Library: a seeded media directory holding a valid file, a file with B-frames, a file
  with out-of-order PTS and an invalid file; each shows the right pill and finding text.
  Upload a good and a bad file. Drop a file into the directory: Refresh does not show it
  validated, Rescan does.
- Use for camera: the add form opens with the file preselected, and the camera streams.
- Metrics: tiles fill from the stream. With a stubbed runner that fails, the GPU tile
  shows the reason while CPU stays populated. The strip says "stale" when the stream
  drops.
- Every camera card has a thumbnail; the quality result appears after a run; Cancel
  works.
- Light, dark and a 400px viewport, with an assertion of no horizontal page scroll.

Go tests: an embed test asserting every file the HTML references exists in the embedded
filesystem, so a missing module fails the build and not the browser. Test fixtures are
generated in the test code; no invented figures ship in the UI.

No JS unit-test runner is added; behaviour is covered end to end.

## Delivery

One branch and PR, in this order: `api.js` and the store; cameras; library; metrics and
faults; removal of the old page together with the e2e port; README. The PR needs
`?metrics=1` from PR #17, so it branches after #17 merges or stacks on it. The README
carries a screenshot only if CI produces it, and no claim beyond what CI produced.

## Risks

- The GPU tile is checked against captured output, not live hardware; this development
  machine's GPU is broken.
- The dashboard is unauthenticated and the metrics endpoint exposes host details; the
  README states this.
- A very large library gives a long table.
- Rewriting every e2e test at once makes the port the riskiest step; it lands in the
  same PR as the page it tests, never ahead of it.
