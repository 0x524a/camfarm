# Media library and file validator — design

Date: 2026-10-11. Status: approved design, not yet implemented.

First of three sub-projects that came from one request to redesign the dashboard:

1. **Media library and validator** (this spec): directories of video the farm can serve, upload,
   and a one-time per-file validity report.
2. **Host metrics**: CPU, memory and GPU panels (separate spec).
3. **Dashboard redesign and UI**: the Library tab, upload, snapshot thumbnails on camera cards,
   the metrics panel and "use this file" when adding a camera (separate spec, written after 1 and 2
   so it builds on real endpoints).

## Purpose

A developer brings their own video into the farm and needs to trust it. Before a camera serves a
file they should see whether it is valid, and if it is risky (B-frames, reordered timestamps and
so on), why. Those quirks are often exactly what they want to test, so they must be visible, not
hidden.

Success:

- A file uploaded through the dashboard, or dropped in a configured directory, appears in the
  library with a status: `valid`, `warning` or `invalid`, and a list of issues in plain language.
- Validation happens once per file: on rescan or on upload. It never runs continuously.
- An `invalid` file cannot become a camera. A `warning` file can, and the warning stays visible.
- Nothing in the dashboard lets an unauthenticated caller read or list arbitrary parts of the
  filesystem unless the operator started the binary with `--allow-ui-paths`.

Constraints inherited from `CLAUDE.md`: `CGO_ENABLED=0` keeps building; determinism is the
product; real sockets and real files in tests; no coverage figure stated that CI did not produce;
no employer or client names anywhere.

## Decisions

| Question | Decision |
|---|---|
| How do directories reach the farm? | Startup roots always; UI-typed roots only behind `--allow-ui-paths` (off by default). |
| How are files validated? | Pure Go, through camfarm's own loader (`internal/media`), plus `mediacommon` SPS/slice parsing. No `ffprobe`. `rtspeek` is not used: it is an RTSP client with no file or B-frame logic. |
| What may become a camera? | `valid` and `warning` files. `invalid` files are blocked. |
| What happens to an invalid upload? | It is kept, flagged, with its reason, and can be deleted. |

## 1. Validator: `internal/media/analyze.go`

`Analyze` lives next to the parser because it inspects access units.

```go
type Severity string // "warning" | "invalid"

type Issue struct {
    Code     string   // stable, see table
    Severity Severity
    Message  string   // plain language, one sentence
    Detail   string   // numbers behind the message, may be empty
}

type Facts struct {
    Container     string
    Codec         Codec
    Width, Height int
    FPS           float64 // declared; 0 when the bitstream carries no timing
    MeasuredFPS   float64
    Duration      time.Duration
    Frames        int
    Keyframes     int
    MaxGOP        int     // longest keyframe-to-keyframe run, in frames
    ReorderDepth  int     // 0 when decode order equals presentation order
    BitrateBps    int64
    SizeBytes     int64
}

type Report struct {
    Status string // "valid" | "warning" | "invalid"
    Facts  Facts
    Issues []Issue
}

func AnalyzeFile(path string) Report            // sniff, load, then Analyze
func Analyze(m *Media, size int64) Report       // pure, for tests
```

`Status` is `invalid` if any issue is invalid, else `warning` if any issue exists, else `valid`.
`AnalyzeFile` recovers from a panic in the parser and reports it as `loader_error`: a hostile or
corrupt file must not take the dashboard down.

### Issue codes

Invalid (the loader would fail or serve nothing useful):

| Code | Condition |
|---|---|
| `unreadable` | open or read failed |
| `unsupported_container` | `sniff` does not recognise it (message names the first bytes and the supported containers) |
| `no_video` | container parsed, no H.264/H.265 video track |
| `unsupported_codec` | video track present, codec not H.264/H.265 |
| `missing_parameter_sets` | no SPS/PPS (H.264) or VPS/SPS/PPS (H.265) |
| `no_frames` | zero access units |
| `no_start_keyframe` | first access unit is not a random access point |
| `loader_error` | the loader returned an error or panicked; the error text is the detail |

Warning (serves, but behaves unlike a plain IPPP stream):

| Code | Condition |
|---|---|
| `b_frames` | an H.264 slice is type B (parsed from the slice header), or reordering exists in an H.265 stream |
| `pts_dts_reordering` | PTS ≠ DTS for some access units; detail carries the reorder depth |
| `dts_not_monotonic` | DTS goes backwards or repeats |
| `timestamp_jump` | a DTS step is over 4× the median step |
| `variable_frame_rate` | step spread over 10% of the median |
| `fps_mismatch` | declared fps differs from measured by more than 5% |
| `long_gop` | longest keyframe gap over 10 seconds |
| `short_clip` | duration under 1 second (it loops every second) |
| `odd_dimensions` | width or height is odd |
| `large_file` | size over 512 MiB: the whole parsed file is held in memory |

H.265 slice types are not parsed: the slice header depends on PPS state, so H.265 reordering is
reported from PTS≠DTS and the SPS reorder fields only. The `b_frames` message says which evidence
it used, so the report never claims more than it checked.

`MaxGOP`, `ReorderDepth`, step statistics and the thresholds above are constants in one file so a
test pins each one.

## 2. Library: `internal/library`

```go
type Library struct { /* roots, cache, worker pool, upload dir */ }

type Entry struct {
    ID       string    // stable: hex of sha256(root path + "\x00" + relative path)
    Root     string
    Path     string    // absolute
    Name     string    // relative to its root
    Modified time.Time
    Report   media.Report
}

func New(opts Options) (*Library, error)
func (l *Library) List() []Entry
func (l *Library) Scan(ctx context.Context, root string, force bool) error
func (l *Library) Upload(ctx context.Context, name string, r io.Reader) (Entry, error)
func (l *Library) Delete(id string) error
func (l *Library) AddRoot(path string) error   // errors unless AllowUIPaths
func (l *Library) Resolve(id string) (Entry, bool)
```

### Roots and scan

- `Options.Roots` come from `--media-dir` (repeatable). The first writable root is the upload
  target. With no writable root, upload and delete return `ErrReadOnly` and the UI hides them.
- `AddRoot` works only when `Options.AllowUIPaths` is set (flag `--allow-ui-paths`). The path must
  exist and be a directory.
- Scan walks at most 3 levels deep, lists regular files only, and never follows symlinks (a
  symlink is skipped, which also stops a link escaping the root).
- Only files with a video extension (`.ts .mp4 .m4v .mov .mkv .avi .h264 .h265 .mpg`) are listed.
  The container is still decided from content, so a `.mkv` is reported as
  `unsupported_container`, not skipped.
- Results are cached by path, size and mtime. A normal scan re-validates only changed files;
  `force` re-validates everything. Files that disappeared are dropped.
- A pool of 2 workers runs `AnalyzeFile`, with a 30 s timeout per file. A timeout is `loader_error`.
  `Scan` returns when the whole directory is done; entries appear in `List` as they finish.

### Upload and delete

- `Upload` streams to `<root>/.<name>.part`, enforcing `--max-upload` (default 2 GiB; exceeding it
  aborts and removes the part file with `ErrTooLarge`). It then validates the part file, renames
  it into place and records the entry.
- The name is reduced to its base, rejects empty names, `.`/`..`, control characters and path
  separators, and must carry a listed video extension. An existing file is never overwritten: a
  numeric suffix is added (`clip-1.mp4`).
- `Delete` removes only a file in the upload root; any other id returns `ErrNotDeletable`. It
  refuses a file a running camera is serving (the dashboard checks the fleet and passes the set of
  in-use paths; see §3).
- Part files left by a crash are removed at startup.

## 3. Dashboard API (`internal/dashboard`)

All handlers are thin: parse, call the library, encode. JSON field names are lower camel case.

| Method and path | Behaviour |
|---|---|
| `GET /api/v1/library` | `{roots: [{path, writable, ui}], uploadEnabled, allowUIPaths, files: [Entry…]}`. Each file also carries `inUseBy: [cameraId…]`. |
| `POST /api/v1/library/scan` | Body `{force?: bool, root?: string}`. Scans one root or all. Returns the same shape as GET. |
| `POST /api/v1/library/upload` | `multipart/form-data`, one `file` part. Returns the new entry (201), including an `invalid` one. 413 over `--max-upload`; 400 on a bad name; 403 when no root is writable. |
| `DELETE /api/v1/library/files/{id}` | 204; 404 unknown; 409 when a camera uses it; 403 outside the upload root. |
| `POST /api/v1/library/roots` | Body `{path}`. 403 unless `--allow-ui-paths`; 400 if not an existing directory. |

`POST /api/v1/cameras` accepts `source: {kind: "file", libraryId: "<id>"}`. The server resolves it
to the library path and returns **422** with the report when its status is `invalid`; a `warning`
file proceeds. The existing typed `source.path` keeps working unchanged. A camera's DTO gains
`libraryId` and `warnings: [Issue…]` when it came from the library, so the card can show them.

The upload body is read with `http.MaxBytesReader`. Responses never include the contents of a file
or any path outside a configured root.

## 4. Binary: `cmd/camfarm`

- `--media-dir <path>` (repeatable) adds a root. A directory that does not exist or is not
  readable stops startup with a clear message.
- `--allow-ui-paths` (bool, default false).
- `--max-upload <size>` (default `2GiB`).
- The scan at startup runs in the background so a large directory does not delay the dashboard;
  the UI shows "scanning" until it finishes.
- A Docker volume is just a `--media-dir` pointing at the mount. The README gains a short example.

## 5. Testing

- **`media.Analyze`**: table-driven over generated media for every issue code, with one fixture
  or synthetic `Media` per row; a row asserts both status and issue code. Boundary values for
  each threshold (just under, just over). The bundled H.264 and H.265 fixtures must report `valid`.
- **`AnalyzeFile`**: real files — truncated, empty, random bytes, an MP4 renamed `.ts`, a
  parser-panic input — each ending in `invalid` with the right code and no panic.
- **`library`**: scan depth limit, symlink skipped, extension filter, cache hit and `force`,
  disappearing file, upload cap, name rules (traversal, separators, control characters, no
  extension), no-overwrite suffix, `.part` cleanup, delete outside the upload root refused,
  `AddRoot` refused without the flag, worker timeout. Run under `-race`.
- **Dashboard**: a handler test per endpoint on a real listener, including 413, 422, 409 and 403,
  and a camera created from a library id.
- **E2E**: upload a file, scan, create a camera from it, read it over RTSP.
- Path-traversal and symlink-escape tests cover scan, upload and delete.
- Coverage is whatever CI reports. No figure is written into the README.

## 6. Out of scope

- Transcoding or repairing a file; the validator only reports.
- H.265 slice-level parsing; ffprobe integration; hot-watching a directory for changes (rescan is
  an explicit user action).
- Authentication for the dashboard. The mitigation here is that arbitrary paths are off by default.
- Containers other than MPEG-TS and MP4/MOV.

## 7. Risks

- Parsing an untrusted file with the existing parsers is the main exposure. The panic recovery,
  per-file timeout and the `large_file` warning contain it; the tests above cover each.
- A file that is valid but huge is held fully in memory once a camera uses it. The warning is
  advisory; a hard limit is deliberately not added here.
- `b_frames` for H.265 is weaker evidence than for H.264. The report says so.
