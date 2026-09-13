# Dynamic camera lifecycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give a running `camfarm` fleet `Fleet.AddCamera`/`Fleet.RemoveCamera`, so a camera can be
started or stopped without restarting the process — the primitive a later dashboard needs for
"load" and "off-load."

**Architecture:** Both `internal/rtsp.Server` and `internal/onvif.Server` already dispatch by
looking a camera ID up in a map under a lock, so add/remove is a lifecycle problem, not a routing
rewrite. `internal/onvif` needs only locked map insert/delete (no goroutines). `internal/rtsp`
needs every camera's pump — one present since `Start()`, one added later — to run on its own
cancellable child context with its own completion signal, so one camera can be stopped without
touching any other. `Fleet` orchestrates both servers and owns a fleet-lifetime media-source cache
and a monotonically increasing seed-index counter.

**Tech Stack:** Go 1.26, `gortsplib/v5`, `onvif-go/server`. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-13-dynamic-camera-lifecycle-design.md`

## Global Constraints

- No cgo, no runtime system packages (`CLAUDE.md`, `README.md` Requirements).
- Every fault decision and seed derivation must be a pure function of the root seed and stable
  call order — never wall-clock time, goroutine scheduling, or map iteration order (`CLAUDE.md`,
  "Determinism is the product").
- `go test -race` is non-negotiable for every new test touching `internal/rtsp`: mid-test mutation
  of a running fleet is a core feature here, so races are a real risk, not a formality (spec §5,
  master design §10.7).
- A refused request must name what was refused and why (`ErrUnsupported`/`ErrUnknownCamera`,
  never a silent no-op) — reuse the existing error taxonomy in `errors.go`, never duplicate it.
- Real sockets and real clients in tests, never mocks of `gortsplib`/`onvif-go` — matches every
  existing test in `internal/rtsp/server_test.go` and `internal/onvif/server_test.go`.

---

### Task 1: `internal/rtsp` — per-camera pump lifecycle + `AddCamera`

**Files:**
- Modify: `internal/rtsp/server.go`
- Test: `internal/rtsp/server_test.go`

**Interfaces:**
- Consumes: existing `Pump`/`NewPump`/`newFormat` (`internal/rtsp/pump.go`), unchanged.
- Produces: `func (s *Server) AddCamera(cc CameraConfig) error`. Every `camera` value (including
  ones built during `Start()`) now carries `cancel context.CancelFunc` and `done chan struct{}`,
  and `Server` retains `rootCtx context.Context` after `Start()`. Task 2 (`RemoveCamera`) relies on
  both fields being populated for *every* camera, not only ones added via `AddCamera`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/rtsp/server_test.go`:

```go
// TestAddCameraRunningServerServesNewCamera proves a camera added after Start
// is immediately dialable, and that the camera present since Start is
// unaffected by the addition.
func TestAddCameraRunningServerServesNewCamera(t *testing.T) {
	s := startServer(t, 1)
	m := testMedia(t)

	if err := s.AddCamera(CameraConfig{ID: "extra", Media: m, Seed: seed.Seed(99)}); err != nil {
		t.Fatalf("AddCamera: %v", err)
	}

	for _, id := range []string{"cam-00", "extra"} {
		u, err := base.ParseURL(s.URL(id))
		if err != nil {
			t.Fatalf("parse %s: %v", id, err)
		}
		c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
		if err := c.Start(); err != nil {
			t.Fatalf("client start %s: %v", id, err)
		}
		desc, _, err := c.Describe(u)
		if err != nil {
			c.Close()
			t.Fatalf("DESCRIBE %s: %v", id, err)
		}
		if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
			c.Close()
			t.Fatalf("SETUP %s: %v", id, err)
		}
		got := make(chan struct{}, 1)
		c.OnPacketRTPAny(func(*description.Media, format.Format, *rtp.Packet) {
			select {
			case got <- struct{}{}:
			default:
			}
		})
		if _, err := c.Play(nil); err != nil {
			c.Close()
			t.Fatalf("PLAY %s: %v", id, err)
		}
		select {
		case <-got:
		case <-time.After(10 * time.Second):
			c.Close()
			t.Fatalf("no RTP from %s within 10s", id)
		}
		c.Close()
	}
}

func TestAddCameraBeforeStartRefused(t *testing.T) {
	m := testMedia(t)
	s, err := New(Config{Host: "127.0.0.1", Obs: obs.New(10), Cameras: []CameraConfig{
		{ID: "a", Media: m, Seed: seed.Seed(1)},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.AddCamera(CameraConfig{ID: "b", Media: m, Seed: seed.Seed(2)}); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("AddCamera before Start: err = %v, want ErrNotStarted", err)
	}
}

func TestAddCameraRejectsBadConfig(t *testing.T) {
	s := startServer(t, 1)
	m := testMedia(t)
	cases := map[string]CameraConfig{
		"empty id":     {ID: "", Media: m},
		"nil media":    {ID: "new"},
		"duplicate id": {ID: "cam-00", Media: m},
	}
	for name, cc := range cases {
		if err := s.AddCamera(cc); err == nil {
			t.Errorf("%s: AddCamera succeeded, want an error", name)
		}
	}
}
```

Add `"errors"` to the test file's import block if not already present (it is not — check the
current imports at the top of `internal/rtsp/server_test.go` and add it alongside the existing
`"strings"` import).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/rtsp/... -run TestAddCamera -race -v`
Expected: FAIL to compile — `s.AddCamera` and `ErrNotStarted` usage undefined (`ErrNotStarted`
itself already exists in `internal/rtsp/server.go`; `AddCamera` does not).

- [ ] **Step 3: Implement**

In `internal/rtsp/server.go`:

1. Add fields to `camera` and `Server`:

```go
type camera struct {
	id     string
	cfg    CameraConfig
	stream *gortsplib.ServerStream
	medi   *description.Media
	// forma is the interface rather than a concrete type: a fleet may mix codecs,
	// so this is per camera.
	forma format.Format
	// cancel and done give this camera's pump its own lifecycle, independent of
	// every other camera's. Populated for every camera, whether built during
	// Start() or later by AddCamera, so RemoveCamera works uniformly on either.
	cancel context.CancelFunc
	done   chan struct{}
}
```

```go
// Server serves a fleet of synthetic RTSP cameras.
type Server struct {
	cfg Config
	log *slog.Logger
	ck  clock.Clock

	mu      sync.RWMutex
	srv     *gortsplib.Server
	cams    map[string]*camera
	order   []string
	readers map[*gortsplib.ServerSession]string
	rootCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}
```

2. Factor the per-camera validation out of `New()`'s loop into a shared helper, so `New()` and
   `AddCamera` cannot enforce different rules:

```go
// validateCameraConfig checks one camera's config against the server's existing
// cameras. Shared by New (which validates a whole batch against a map it is
// still building) and AddCamera (which validates one camera against the
// server's current, running set), so the two paths cannot drift.
func validateCameraConfig(cc CameraConfig, existing map[string]*camera) error {
	if cc.ID == "" {
		return errors.New("rtsp: camera with an empty ID")
	}
	if strings.ContainsAny(cc.ID, "/?#") {
		return fmt.Errorf("rtsp: camera ID %q contains a character reserved in a URL path", cc.ID)
	}
	if cc.Media == nil {
		return fmt.Errorf("rtsp: camera %q has no media", cc.ID)
	}
	if len(cc.Media.SPS) == 0 || len(cc.Media.PPS) == 0 {
		return fmt.Errorf("rtsp: camera %q media carries no SPS/PPS", cc.ID)
	}
	// H.265 needs three parameter sets, not two. Refused here rather than
	// serving a stream no client can decode.
	if cc.Media.Codec == media.CodecH265 && len(cc.Media.VPS) == 0 {
		return fmt.Errorf("rtsp: camera %q H265 media carries no VPS", cc.ID)
	}
	if _, dup := existing[cc.ID]; dup {
		return fmt.Errorf("rtsp: duplicate camera ID %q", cc.ID)
	}
	return nil
}
```

3. Replace `New()`'s per-camera validation loop body with a call to the helper:

```go
	for _, cc := range cfg.Cameras {
		if err := validateCameraConfig(cc, s.cams); err != nil {
			return nil, err
		}
		s.cams[cc.ID] = &camera{id: cc.ID, cfg: cc}
		s.order = append(s.order, cc.ID)
	}
```

4. In `Start()`, retain the root context and give every camera's pump its own child context. Find:

```go
	ctx, cancel := context.WithCancel(context.Background())
	pumps, failedPumpID, pumpErr := s.buildPumps()
```

Change to:

```go
	ctx, cancel := context.WithCancel(context.Background())
	s.rootCtx = ctx
	pumps, failedPumpID, pumpErr := s.buildPumps()
```

Then find the goroutine-starting loop later in `Start()`:

```go
	for _, id := range s.order {
		pump := pumps[id]
		camID := id
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			pump.run(ctx, s.ck, func(err error) {
				s.log.Warn("pump write failed", "camera", camID, "err", err)
			})
		}()
	}
```

Replace with:

```go
	for _, id := range s.order {
		pump := pumps[id]
		camID := id
		camCtx, camCancel := context.WithCancel(ctx)
		done := make(chan struct{})
		cam := s.cams[camID]
		cam.cancel = camCancel
		cam.done = done

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer close(done)
			pump.run(camCtx, s.ck, func(err error) {
				s.log.Warn("pump write failed", "camera", camID, "err", err)
			})
		}()
	}
```

`Close()` needs no change: it still cancels the root (`s.cancel()`), which cancels every child by
ordinary context propagation, then `s.wg.Wait()` still waits for every camera's goroutine.

5. Add `AddCamera` (place after `buildPumps`, before `Start`, or anywhere else at file scope —
   after `Start`/`Close` reads most naturally):

```go
// AddCamera registers and starts a new camera on an already-running server.
// It runs the same validation New applies to a batch, so a spec that would be
// refused at Start is refused here too.
func (s *Server) AddCamera(cc CameraConfig) error {
	s.mu.Lock()
	if s.srv == nil {
		s.mu.Unlock()
		return ErrNotStarted
	}
	if err := validateCameraConfig(cc, s.cams); err != nil {
		s.mu.Unlock()
		return err
	}

	forma, err := newFormat(cc.Media)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	medi := &description.Media{
		Type:    description.MediaTypeVideo,
		Formats: []format.Format{forma},
	}
	stream := &gortsplib.ServerStream{
		Server: s.srv,
		Desc:   &description.Session{Medias: []*description.Media{medi}},
	}
	if err := stream.Initialize(); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("rtsp: initializing stream for %q: %w", cc.ID, err)
	}

	pump, err := NewPump(PumpConfig{
		CameraID: cc.ID,
		Media:    cc.Media,
		Medi:     medi,
		Writer:   stream,
		Seed:     cc.Seed,
		Faults:   cc.Faults,
		Obs:      s.cfg.Obs,
	})
	if err != nil {
		stream.Close()
		s.mu.Unlock()
		return fmt.Errorf("rtsp: building pump for %q: %w", cc.ID, err)
	}

	camCtx, camCancel := context.WithCancel(s.rootCtx)
	done := make(chan struct{})
	cam := &camera{id: cc.ID, cfg: cc, stream: stream, medi: medi, forma: forma, cancel: camCancel, done: done}
	s.cams[cc.ID] = cam
	s.order = append(s.order, cc.ID)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(done)
		pump.run(camCtx, s.ck, func(err error) {
			s.log.Warn("pump write failed", "camera", cc.ID, "err", err)
		})
	}()
	s.mu.Unlock()

	s.log.Info("camera added", "camera", cc.ID)
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/rtsp/... -race -v`
Expected: PASS — every existing test plus the three new ones.

- [ ] **Step 5: Commit**

```bash
git add internal/rtsp/server.go internal/rtsp/server_test.go
git commit -m "Give every RTSP camera its own pump lifecycle, add AddCamera"
```

---

### Task 2: `internal/rtsp` — `RemoveCamera`

**Files:**
- Modify: `internal/rtsp/server.go`
- Test: `internal/rtsp/server_test.go`

**Interfaces:**
- Consumes: `cam.cancel`/`cam.done` from Task 1, populated for every camera.
- Produces: `func (s *Server) RemoveCamera(id string) error`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/rtsp/server_test.go`:

```go
// TestRemoveCameraStopsServingOthersUnaffected proves removal takes effect
// immediately (new DESCRIBE 404s) and leaves every other camera untouched.
func TestRemoveCameraStopsServingOthersUnaffected(t *testing.T) {
	s := startServer(t, 2)

	if err := s.RemoveCamera("cam-00"); err != nil {
		t.Fatalf("RemoveCamera: %v", err)
	}

	u, err := base.ParseURL(s.URL("cam-00"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, _ := newTestClient(u)
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer c.Close()
	if _, _, err := c.Describe(u); err == nil {
		t.Fatal("DESCRIBE of a removed camera succeeded")
	} else if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want a 404", err)
	}

	// cam-01 must still work.
	u2, err := base.ParseURL(s.URL("cam-01"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c2, _ := newTestClient(u2)
	if err := c2.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer c2.Close()
	if _, _, err := c2.Describe(u2); err != nil {
		t.Fatalf("DESCRIBE cam-01 after removing cam-00: %v", err)
	}
}

// TestRemoveCameraPresentSinceStart proves removal is symmetric: a camera
// configured at Start, not only one added later via AddCamera, can be
// removed. This is the fix that makes "off-load a stream" work for any
// camera the dashboard shows, not only ones it added itself.
func TestRemoveCameraPresentSinceStart(t *testing.T) {
	s := startServer(t, 1)
	if err := s.RemoveCamera("cam-00"); err != nil {
		t.Fatalf("RemoveCamera(camera present since Start): %v", err)
	}
	if s.Has("cam-00") {
		t.Fatal("cam-00 still present after RemoveCamera")
	}
}

// TestRemoveCameraWithActiveSessionDoesNotPanic proves removing a camera a
// client is actively playing does not panic or race; the session simply stops
// receiving new packets.
func TestRemoveCameraWithActiveSessionDoesNotPanic(t *testing.T) {
	s := startServer(t, 1)
	u, err := base.ParseURL(s.URL("cam-00"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer c.Close()
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("DESCRIBE: %v", err)
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatalf("SETUP: %v", err)
	}
	if _, err := c.Play(nil); err != nil {
		t.Fatalf("PLAY: %v", err)
	}

	if err := s.RemoveCamera("cam-00"); err != nil {
		t.Fatalf("RemoveCamera with an active session: %v", err)
	}
}

func TestRemoveUnknownCameraErrors(t *testing.T) {
	s := startServer(t, 1)
	if err := s.RemoveCamera("nope"); err == nil {
		t.Fatal("RemoveCamera of an unknown camera succeeded, want an error")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/rtsp/... -run TestRemoveCamera -race -v`
Expected: FAIL to compile — `s.RemoveCamera` undefined.

- [ ] **Step 3: Implement**

In `internal/rtsp/server.go`, add:

```go
// RemoveCamera stops and removes a camera from an already-running server. It
// removes the camera from the dispatch table first, so a DESCRIBE/SETUP
// arriving after this call 404s immediately, then stops its pump and closes
// its stream. A client already mid-session on the camera is not forcibly
// disconnected: it simply stops receiving new packets.
func (s *Server) RemoveCamera(id string) error {
	s.mu.Lock()
	cam, ok := s.cams[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("rtsp: unknown camera %q", id)
	}
	delete(s.cams, id)
	for i, camID := range s.order {
		if camID == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	stream := cam.stream
	cancel := cam.cancel
	done := cam.done
	s.mu.Unlock()

	cancel()
	<-done
	stream.Close()

	s.log.Info("camera removed", "camera", id)
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/rtsp/... -race -v`
Expected: PASS — every existing test plus the four new ones.

- [ ] **Step 5: Commit**

```bash
git add internal/rtsp/server.go internal/rtsp/server_test.go
git commit -m "Add RemoveCamera to internal/rtsp.Server"
```

---

### Task 3: `internal/onvif` — `AddCamera`/`RemoveCamera`

**Files:**
- Modify: `internal/onvif/server.go`
- Test: `internal/onvif/server_test.go`

**Interfaces:**
- Consumes: nothing from Tasks 1-2; independent of the RTSP-side change.
- Produces: `func (s *Server) AddCamera(cc CameraConfig) error`,
  `func (s *Server) RemoveCamera(id string) error`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/onvif/server_test.go`:

```go
func TestAddCameraRunningServerAnswersSOAP(t *testing.T) {
	srv := startTestServer(t, CameraConfig{ID: "a", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/a", Seed: seed.Seed(1)})

	if err := srv.AddCamera(CameraConfig{ID: "b", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/b", Seed: seed.Seed(2)}); err != nil {
		t.Fatalf("AddCamera: %v", err)
	}

	for _, id := range []string{"a", "b"} {
		resp := soapRequest(t, srv.Endpoint(id)+"/device", "GetDeviceInformation")
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: status = %d, want 200", id, resp.StatusCode)
		}
	}
}

func TestAddCameraDuplicateIDRefused(t *testing.T) {
	srv := startTestServer(t, CameraConfig{ID: "a", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/a", Seed: seed.Seed(1)})
	if err := srv.AddCamera(CameraConfig{ID: "a", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/a2", Seed: seed.Seed(2)}); err == nil {
		t.Fatal("AddCamera with a duplicate ID succeeded, want an error")
	}
}

func TestRemoveCameraStops404OthersUnaffected(t *testing.T) {
	srv := startTestServer(t,
		CameraConfig{ID: "a", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/a", Seed: seed.Seed(1)},
		CameraConfig{ID: "b", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/b", Seed: seed.Seed(2)},
	)

	if err := srv.RemoveCamera("a"); err != nil {
		t.Fatalf("RemoveCamera: %v", err)
	}

	resp, err := http.Get("http://" + srv.Addr().String() + "/onvif/a/device")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("removed camera: status = %d, want 404", resp.StatusCode)
	}

	resp2 := soapRequest(t, srv.Endpoint("b")+"/device", "GetDeviceInformation")
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Errorf("surviving camera: status = %d, want 200", resp2.StatusCode)
	}
}

func TestRemoveUnknownCameraErrorsONVIF(t *testing.T) {
	srv := startTestServer(t, CameraConfig{ID: "a", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/a", Seed: seed.Seed(1)})
	if err := srv.RemoveCamera("nope"); err == nil {
		t.Fatal("RemoveCamera of an unknown camera succeeded, want an error")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/onvif/... -run 'TestAddCamera|TestRemoveCamera' -race -v`
Expected: FAIL to compile — `AddCamera`/`RemoveCamera` undefined on `*Server`.

- [ ] **Step 3: Implement**

In `internal/onvif/server.go`, factor the per-camera construction out of `New()`'s loop into a
shared helper, and factor the validation the same way:

```go
// validateCameraConfig checks one camera's config against the server's
// existing cameras. Shared by New and AddCamera so the two paths cannot
// enforce different rules.
func validateCameraConfig(cc CameraConfig, existing map[string]*camera) error {
	if cc.ID == "" {
		return fmt.Errorf("onvif: camera with an empty ID")
	}
	if cc.Media == nil {
		return fmt.Errorf("onvif: camera %q has no media", cc.ID)
	}
	if _, dup := existing[cc.ID]; dup {
		return fmt.Errorf("onvif: duplicate camera ID %q", cc.ID)
	}
	return nil
}

// buildCamera constructs the onvif-go server and digest state for one
// camera. Shared by New and AddCamera.
func buildCamera(cc CameraConfig) (*camera, error) {
	srv, err := onvifserver.New(&onvifserver.Config{
		DeviceInfo: onvifserver.DeviceInfo{
			Manufacturer:    "camfarm",
			Model:           "synthetic",
			FirmwareVersion: "0.0.0",
			SerialNumber:    cc.ID,
			HardwareID:      cc.ID,
		},
		SupportPTZ:     false,
		SupportImaging: false,
		SupportEvents:  false,
		Output:         io.Discard,
		Profiles: []onvifserver.ProfileConfig{
			{
				Token: profileToken,
				Name:  cc.ID,
				VideoSource: onvifserver.VideoSourceConfig{
					Token:      profileToken + "_source",
					Name:       cc.ID,
					Resolution: onvifserver.Resolution{Width: cc.Media.Width, Height: cc.Media.Height},
					Framerate:  int(cc.Media.FPS),
					Bounds:     onvifserver.Bounds{Width: cc.Media.Width, Height: cc.Media.Height},
				},
				VideoEncoder: onvifserver.VideoEncoderConfig{
					Encoding:   string(cc.Media.Codec),
					Resolution: onvifserver.Resolution{Width: cc.Media.Width, Height: cc.Media.Height},
					Framerate:  int(cc.Media.FPS),
				},
				Snapshot: onvifserver.SnapshotConfig{Enabled: false},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("onvif: building server for %q: %w", cc.ID, err)
	}
	if err := srv.UpdateStreamURI(profileToken, cc.RTSPURL); err != nil {
		return nil, fmt.Errorf("onvif: wiring stream URI for %q: %w", cc.ID, err)
	}

	cam := &camera{id: cc.ID, srv: srv}
	if cc.Username != "" && cc.Password != "" {
		cam.digest = newDigestAuth(cc.Username, cc.Password, cc.Seed.Stream("onvif-auth"))
	}
	return cam, nil
}
```

Replace `New()`'s per-camera loop body:

```go
	for _, cc := range cfg.Cameras {
		if err := validateCameraConfig(cc, s.cams); err != nil {
			return nil, err
		}
		cam, err := buildCamera(cc)
		if err != nil {
			return nil, err
		}
		s.cams[cc.ID] = cam
	}
```

Add `AddCamera` and `RemoveCamera`:

```go
// AddCamera registers a new camera's ONVIF endpoint. Safe to call whether or
// not Start has been called yet: unlike internal/rtsp, nothing here depends
// on a live listener, only on the in-memory dispatch map.
func (s *Server) AddCamera(cc CameraConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := validateCameraConfig(cc, s.cams); err != nil {
		return err
	}
	cam, err := buildCamera(cc)
	if err != nil {
		return err
	}
	s.cams[cc.ID] = cam

	s.log.Info("camera added", "camera", cc.ID)
	return nil
}

// RemoveCamera removes a camera's ONVIF endpoint. There is no goroutine or
// stream to tear down on this side, so this is just the dispatch removal.
func (s *Server) RemoveCamera(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.cams[id]; !ok {
		return fmt.Errorf("onvif: unknown camera %q", id)
	}
	delete(s.cams, id)

	s.log.Info("camera removed", "camera", id)
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/onvif/... -race -v`
Expected: PASS — every existing test plus the four new ones.

- [ ] **Step 5: Commit**

```bash
git add internal/onvif/server.go internal/onvif/server_test.go
git commit -m "Add AddCamera/RemoveCamera to internal/onvif.Server"
```

---

### Task 4: `camfarm` — promote the media-source cache to fleet-lifetime state

**Files:**
- Modify: `camfarm.go`

**Interfaces:**
- Produces: `Fleet.sources map[SourceSpec]*media.Media` and `Fleet.nextIndex int`, both fleet-
  lifetime fields Task 6 (`Fleet.AddCamera`) needs. No new exported API; this is a pure refactor
  with no observable behavior change, verified by the existing test suite passing unchanged.

- [ ] **Step 1: Establish the baseline**

Run: `go test ./... -race`
Expected: PASS (this is the pre-change baseline the refactor must not break).

- [ ] **Step 2: Refactor**

In `camfarm.go`, add two fields to `Fleet`:

```go
// Fleet is a running set of synthetic cameras.
type Fleet struct {
	spec     Spec
	specHash string
	log      *slog.Logger
	rec      *obs.Recorder
	srv      *rtsp.Server
	onvifSrv *onvif.Server

	mu        sync.Mutex
	closed    bool
	cameras   map[string]*Camera
	order     []string
	// sources caches each distinct SourceSpec's parsed media for the fleet's
	// entire lifetime, so AddCamera can share a source the same way Start
	// already shares one across cameras present from the beginning. Never
	// evicted, even after the last camera using an entry is removed: the
	// bundled fixtures are small, and eviction has no proven need yet.
	sources map[SourceSpec]*media.Media
	// nextIndex is the next stable index handed to root.Camera for seed
	// derivation. It only ever increases, even across RemoveCamera calls, so a
	// dynamically added camera's seed is a pure function of call order.
	nextIndex int
	stopWatch func()
}
```

In `Start()`, find:

```go
	f := &Fleet{
		spec:     valid,
		specHash: spec.Hash(),
		log:      log,
		rec:      obs.New(maxEvents),
		cameras:  make(map[string]*Camera, len(valid.Cameras)),
	}

	// Parse each distinct source once and share it. This is the scale lever:
	// M cameras on one source cost one copy of the media plus M encoder states.
	loaded := make(map[SourceSpec]*media.Media)
	root := seed.Seed(valid.Seed)
```

Replace with:

```go
	f := &Fleet{
		spec:     valid,
		specHash: spec.Hash(),
		log:      log,
		rec:      obs.New(maxEvents),
		cameras:  make(map[string]*Camera, len(valid.Cameras)),
		sources:  make(map[SourceSpec]*media.Media),
	}

	// Parse each distinct source once and share it. This is the scale lever:
	// M cameras on one source cost one copy of the media plus M encoder states.
	root := seed.Seed(valid.Seed)
```

A few lines later, find the loop that uses `loaded`:

```go
	for i, cs := range valid.Cameras {
		m, ok := loaded[cs.Source]
		if !ok {
			src, err := newSource(cs.Source)
			if err != nil {
				return nil, err
			}
			m, err = src.Load()
			if err != nil {
				return nil, fmt.Errorf("camfarm: camera %q: %w", cs.ID, err)
			}
			loaded[cs.Source] = m
		}
```

Replace the two `loaded[...]` references with `f.sources[...]`:

```go
	for i, cs := range valid.Cameras {
		m, ok := f.sources[cs.Source]
		if !ok {
			src, err := newSource(cs.Source)
			if err != nil {
				return nil, err
			}
			m, err = src.Load()
			if err != nil {
				return nil, fmt.Errorf("camfarm: camera %q: %w", cs.ID, err)
			}
			f.sources[cs.Source] = m
		}
```

Finally, just before the `log.Info("fleet started", ...)` call at the end of `Start()`, add:

```go
	f.nextIndex = len(valid.Cameras)
```

- [ ] **Step 3: Confirm no regression**

Run: `go test ./... -race`
Expected: PASS, identical to Step 1 — this task changes no observable behavior.

- [ ] **Step 4: Commit**

```bash
git add camfarm.go
git commit -m "Promote the media-source cache to fleet-lifetime state"
```

---

### Task 5: `camfarm` — factor `validateCameraSpec` out of `Spec.validate()`

**Files:**
- Modify: `spec.go`

**Interfaces:**
- Produces: `func validateCameraSpec(c CameraSpec, seen map[string]bool) (CameraSpec, error)`,
  package-private, called by both `Spec.validate()` and Task 6's `Fleet.AddCamera`.

- [ ] **Step 1: Establish the baseline**

Run: `go test ./... -run TestSpecValidation -v` (and the related fault/video-spec refusal tests:
`TestFaultsAreRefusedNotIgnored`, `TestVideoSpecRescaleRefusedNamingFutureBehaviour`,
`TestVideoSpecTranscodeRefusedNamingFutureBehaviour`, `TestSpecRejectsUnknownCodec`)
Expected: PASS (pre-change baseline).

- [ ] **Step 2: Refactor**

In `spec.go`, replace the body of the `for i := range cameras` loop inside `validate` with a call
to a new standalone function. Find:

```go
	for i := range cameras {
		c := &cameras[i]
		if c.ID == "" {
			return s, fmt.Errorf("camfarm: camera %d has an empty ID", i)
		}
		if strings.ContainsAny(c.ID, "/?# ") {
			return s, fmt.Errorf("camfarm: camera ID %q contains a character reserved in a URL path", c.ID)
		}
		if seen[c.ID] {
			return s, fmt.Errorf("camfarm: duplicate camera ID %q", c.ID)
		}
		seen[c.ID] = true

		if c.Source.Kind == "" {
			c.Source.Kind = SourceFixture
		}
		switch c.Source.Kind {
		case SourceFixture:
			if c.Source.Path != "" {
				return s, fmt.Errorf("camfarm: camera %q uses the bundled fixture but also sets a path", c.ID)
			}
		case SourceFile:
			if c.Source.Path == "" {
				return s, fmt.Errorf("camfarm: camera %q has source kind %q but no path", c.ID, SourceFile)
			}
		default:
			return s, fmt.Errorf("camfarm: camera %q has unknown source kind %q", c.ID, c.Source.Kind)
		}

		// Only the string is checked here; comparing it against the loaded
		// source's actual codec needs the source, so checkAdvertised in
		// camfarm.go does that.
		if c.Video.Codec != "" {
			switch media.Codec(c.Video.Codec) {
			case media.CodecH264, media.CodecH265:
			default:
				return s, fmt.Errorf("%w: camera %q requests codec %q; this version implements %q and %q",
					ErrUnsupported, c.ID, c.Video.Codec, media.CodecH264, media.CodecH265)
			}
		}

		for _, f := range c.Faults {
			if !fault.Known(fault.Kind(f.Kind)) {
				return s, fmt.Errorf("camfarm: camera %q requests unknown fault kind %q", c.ID, f.Kind)
			}
			if f.Rate < 0 || f.Rate > 1 {
				return s, fmt.Errorf("camfarm: camera %q fault %q has rate %v outside [0,1]", c.ID, f.Kind, f.Rate)
			}
			// Catalogued, validated, and refused. Accepting it silently would
			// make a test that asked for a fault pass while nothing misbehaved,
			// which is worse than refusing.
			return s, fmt.Errorf("%w: fault %q is catalogued but its effect is not implemented in this version",
				ErrUnsupported, f.Kind)
		}

		if (c.Auth.Username == "") != (c.Auth.Password == "") {
			return s, fmt.Errorf("camfarm: camera %q sets one of auth username/password but not both", c.ID)
		}
	}

	s.Cameras = cameras
	return s, nil
}
```

Replace with:

```go
	for i := range cameras {
		c, err := validateCameraSpec(cameras[i], seen)
		if err != nil {
			return s, err
		}
		cameras[i] = c
	}

	s.Cameras = cameras
	return s, nil
}

// validateCameraSpec checks one camera's spec and fills defaults on a copy,
// marking its ID as seen. Shared by Spec.validate (which builds seen fresh
// for a whole spec) and Fleet.AddCamera (which seeds it from the fleet's
// current camera IDs), so the two paths cannot enforce different rules for
// what a valid CameraSpec is.
func validateCameraSpec(c CameraSpec, seen map[string]bool) (CameraSpec, error) {
	if c.ID == "" {
		return c, fmt.Errorf("camfarm: camera has an empty ID")
	}
	if strings.ContainsAny(c.ID, "/?# ") {
		return c, fmt.Errorf("camfarm: camera ID %q contains a character reserved in a URL path", c.ID)
	}
	if seen[c.ID] {
		return c, fmt.Errorf("camfarm: duplicate camera ID %q", c.ID)
	}
	seen[c.ID] = true

	if c.Source.Kind == "" {
		c.Source.Kind = SourceFixture
	}
	switch c.Source.Kind {
	case SourceFixture:
		if c.Source.Path != "" {
			return c, fmt.Errorf("camfarm: camera %q uses the bundled fixture but also sets a path", c.ID)
		}
	case SourceFile:
		if c.Source.Path == "" {
			return c, fmt.Errorf("camfarm: camera %q has source kind %q but no path", c.ID, SourceFile)
		}
	default:
		return c, fmt.Errorf("camfarm: camera %q has unknown source kind %q", c.ID, c.Source.Kind)
	}

	// Only the string is checked here; comparing it against the loaded
	// source's actual codec needs the source, so checkAdvertised in
	// camfarm.go does that.
	if c.Video.Codec != "" {
		switch media.Codec(c.Video.Codec) {
		case media.CodecH264, media.CodecH265:
		default:
			return c, fmt.Errorf("%w: camera %q requests codec %q; this version implements %q and %q",
				ErrUnsupported, c.ID, c.Video.Codec, media.CodecH264, media.CodecH265)
		}
	}

	for _, f := range c.Faults {
		if !fault.Known(fault.Kind(f.Kind)) {
			return c, fmt.Errorf("camfarm: camera %q requests unknown fault kind %q", c.ID, f.Kind)
		}
		if f.Rate < 0 || f.Rate > 1 {
			return c, fmt.Errorf("camfarm: camera %q fault %q has rate %v outside [0,1]", c.ID, f.Kind, f.Rate)
		}
		// Catalogued, validated, and refused. Accepting it silently would make a
		// test that asked for a fault pass while nothing misbehaved, which is
		// worse than refusing.
		return c, fmt.Errorf("%w: fault %q is catalogued but its effect is not implemented in this version",
			ErrUnsupported, f.Kind)
	}

	if (c.Auth.Username == "") != (c.Auth.Password == "") {
		return c, fmt.Errorf("camfarm: camera %q sets one of auth username/password but not both", c.ID)
	}

	return c, nil
}
```

- [ ] **Step 3: Confirm no regression**

Run: `go test ./... -race`
Expected: PASS, in particular every test named in Step 1 unchanged in behavior (the "camera %d has
an empty ID" message lost its index number — no existing test asserts on that exact text, only on
`err != nil`, so this is safe).

- [ ] **Step 4: Commit**

```bash
git add spec.go
git commit -m "Factor validateCameraSpec out of Spec.validate for reuse by AddCamera"
```

---

### Task 6: `camfarm` — `Fleet.AddCamera`

**Files:**
- Modify: `camfarm.go`
- Test: `camfarm_test.go`

**Interfaces:**
- Consumes: `f.sources`/`f.nextIndex` (Task 4), `validateCameraSpec` (Task 5),
  `rtsp.Server.AddCamera`/`onvif.Server.AddCamera` (Tasks 1 and 3).
- Produces: `func (f *Fleet) AddCamera(spec CameraSpec) (*Camera, error)`.

- [ ] **Step 1: Write the failing tests**

Add to `camfarm_test.go`:

```go
func TestAddCameraAppearsInList(t *testing.T) {
	f := StartT(t, minimalSpec())

	cam, err := f.AddCamera(CameraSpec{ID: "new-cam"})
	if err != nil {
		t.Fatalf("AddCamera: %v", err)
	}
	if cam.ID() != "new-cam" {
		t.Errorf("ID = %q", cam.ID())
	}

	list := f.List()
	if len(list) != 3 {
		t.Fatalf("List = %d cameras, want 3", len(list))
	}
	found := false
	for _, st := range list {
		if st.ID == "new-cam" {
			found = true
			if !strings.HasSuffix(st.RTSPURL, "/new-cam") {
				t.Errorf("RTSPURL = %q", st.RTSPURL)
			}
		}
	}
	if !found {
		t.Fatal("new-cam not present in List()")
	}

	got, err := f.Camera("new-cam")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	if got.Stats().Seed == 0 || got.Stats().Seed == f.Seed() {
		t.Error("added camera's seed is zero or equals the root seed; it must be derived")
	}
}

func TestAddCameraDuplicateIDRefused(t *testing.T) {
	f := StartT(t, minimalSpec())
	if _, err := f.AddCamera(CameraSpec{ID: "front-door"}); err == nil {
		t.Fatal("AddCamera with a duplicate ID succeeded, want an error")
	}
}

// Spec section 7.7's fault refusal must hold through AddCamera too, not only
// through Start -- otherwise a dashboard could ask for a fault and silently
// get a camera that never misbehaves.
func TestAddCameraFaultRefused(t *testing.T) {
	f := StartT(t, minimalSpec())
	_, err := f.AddCamera(CameraSpec{ID: "new-cam", Faults: []FaultSpec{{Kind: "frame_drop", Rate: 0.5}}})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

// TestAddCameraH265Source proves AddCamera can add a camera whose source
// kind differs from every camera present since Start, reusing the fleet's
// media cache the same way Start shares a source across cameras.
func TestAddCameraH265Source(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h265.ts")
	if err := os.WriteFile(path, h265FixtureBytes(t), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	f := StartT(t, minimalSpec())
	cam, err := f.AddCamera(CameraSpec{ID: "hevc", Source: SourceSpec{Kind: SourceFile, Path: path}})
	if err != nil {
		t.Fatalf("AddCamera: %v", err)
	}
	_ = cam

	for _, st := range f.List() {
		if st.ID == "hevc" && st.Codec != "H265" {
			t.Errorf("codec = %q, want H265", st.Codec)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test . -run TestAddCamera -race -v`
Expected: FAIL to compile — `f.AddCamera` undefined on `*Fleet`.

- [ ] **Step 3: Implement**

In `camfarm.go`, add:

```go
// AddCamera starts a new camera on a running fleet. It applies the same
// validation Start does, so a spec that would be refused at Start is refused
// here too.
//
// Its seed is derived from a monotonically increasing index that never
// resets, even across RemoveCamera calls, so a dynamically added camera's
// seed is a pure function of call order -- reproducible if that order is
// logged, per CLAUDE.md's "Determinism is the product."
func (f *Fleet) AddCamera(spec CameraSpec) (*Camera, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	seen := make(map[string]bool, len(f.cameras))
	for id := range f.cameras {
		seen[id] = true
	}
	cs, err := validateCameraSpec(spec, seen)
	if err != nil {
		return nil, err
	}

	m, ok := f.sources[cs.Source]
	if !ok {
		src, err := newSource(cs.Source)
		if err != nil {
			return nil, err
		}
		m, err = src.Load()
		if err != nil {
			return nil, fmt.Errorf("camfarm: camera %q: %w", cs.ID, err)
		}
		f.sources[cs.Source] = m
	}
	if err := checkAdvertised(cs, m); err != nil {
		return nil, err
	}

	root := seed.Seed(f.spec.Seed)
	camSeed := root.Camera(f.nextIndex)
	f.nextIndex++

	if err := f.srv.AddCamera(rtsp.CameraConfig{ID: cs.ID, Media: m, Seed: camSeed}); err != nil {
		return nil, err
	}

	if f.onvifSrv != nil {
		if err := f.onvifSrv.AddCamera(onvif.CameraConfig{
			ID:       cs.ID,
			Media:    m,
			RTSPURL:  f.srv.URL(cs.ID),
			Username: cs.Auth.Username,
			Password: cs.Auth.Password,
			Seed:     camSeed,
		}); err != nil {
			if rmErr := f.srv.RemoveCamera(cs.ID); rmErr != nil {
				f.log.Warn("rollback: could not remove camera from rtsp after onvif AddCamera failed",
					"camera", cs.ID, "err", rmErr)
			}
			return nil, err
		}
	}

	cam := &Camera{
		fleet:  f,
		id:     cs.ID,
		seed:   camSeed,
		source: sourceDescription(cs.Source),
		media:  m,
	}
	f.cameras[cs.ID] = cam
	f.order = append(f.order, cs.ID)

	f.log.Info("camera added", "camera", cs.ID, "seed", fmt.Sprintf("%#x", camSeed))
	return cam, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./... -race`
Expected: PASS — every existing test plus the four new ones.

- [ ] **Step 5: Commit**

```bash
git add camfarm.go camfarm_test.go
git commit -m "Add Fleet.AddCamera"
```

---

### Task 7: `camfarm` — `Fleet.RemoveCamera`

**Files:**
- Modify: `camfarm.go`
- Test: `camfarm_test.go`

**Interfaces:**
- Consumes: `rtsp.Server.RemoveCamera`/`onvif.Server.RemoveCamera` (Tasks 2 and 3).
- Produces: `func (f *Fleet) RemoveCamera(id string) error`.

- [ ] **Step 1: Write the failing tests**

Add to `camfarm_test.go`:

```go
func TestRemoveCameraRemovesFromList(t *testing.T) {
	f := StartT(t, minimalSpec())

	if err := f.RemoveCamera("front-door"); err != nil {
		t.Fatalf("RemoveCamera: %v", err)
	}

	list := f.List()
	if len(list) != 1 {
		t.Fatalf("List = %d cameras, want 1", len(list))
	}
	if list[0].ID != "lobby" {
		t.Errorf("surviving camera = %q, want lobby", list[0].ID)
	}
	if _, err := f.Camera("front-door"); !errors.Is(err, ErrUnknownCamera) {
		t.Errorf("Camera(removed): err = %v, want ErrUnknownCamera", err)
	}
}

func TestRemoveUnknownCameraErrorsFleet(t *testing.T) {
	f := StartT(t, minimalSpec())
	if err := f.RemoveCamera("nope"); !errors.Is(err, ErrUnknownCamera) {
		t.Errorf("err = %v, want ErrUnknownCamera", err)
	}
}

// TestAddThenRemoveCamera proves the full lifecycle: a camera added at
// runtime can also be removed at runtime.
func TestAddThenRemoveCamera(t *testing.T) {
	f := StartT(t, minimalSpec())

	if _, err := f.AddCamera(CameraSpec{ID: "temp"}); err != nil {
		t.Fatalf("AddCamera: %v", err)
	}
	if len(f.List()) != 3 {
		t.Fatalf("List = %d cameras after add, want 3", len(f.List()))
	}
	if err := f.RemoveCamera("temp"); err != nil {
		t.Fatalf("RemoveCamera: %v", err)
	}
	if len(f.List()) != 2 {
		t.Fatalf("List = %d cameras after remove, want 2", len(f.List()))
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test . -run 'TestRemoveCamera|TestAddThenRemoveCamera' -race -v`
Expected: FAIL to compile — `f.RemoveCamera` undefined on `*Fleet`.

- [ ] **Step 3: Implement**

Add `"errors"` to `camfarm.go`'s import block (it is not currently imported there — check the
existing `import (...)` block at the top of the file before adding it, so it is not duplicated).

Add to `camfarm.go`:

```go
// RemoveCamera stops and removes a camera from a running fleet.
//
// It is best effort once id is known to exist: there is nothing a caller
// could sensibly do to retry a partial removal, so both the RTSP and ONVIF
// sides are always attempted regardless of whether the other failed.
func (f *Fleet) RemoveCamera(id string) error {
	f.mu.Lock()
	if _, ok := f.cameras[id]; !ok {
		f.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrUnknownCamera, id)
	}
	delete(f.cameras, id)
	for i, camID := range f.order {
		if camID == id {
			f.order = append(f.order[:i], f.order[i+1:]...)
			break
		}
	}
	f.mu.Unlock()

	var errs []error
	if err := f.srv.RemoveCamera(id); err != nil {
		errs = append(errs, err)
	}
	if f.onvifSrv != nil {
		if err := f.onvifSrv.RemoveCamera(id); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	f.log.Info("camera removed", "camera", id)
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./... -race`
Expected: PASS — every existing test plus the three new ones.

- [ ] **Step 5: Commit**

```bash
git add camfarm.go camfarm_test.go
git commit -m "Add Fleet.RemoveCamera"
```

---

### Task 8: Integration tests — real client through `AddCamera`/`RemoveCamera`, determinism

**Files:**
- Modify: `integration_test.go`

**Interfaces:**
- Consumes: `Fleet.AddCamera`/`RemoveCamera` (Tasks 6-7), `gortsplib.Client` (already a `go.mod`
  dependency, not previously imported by this file).

- [ ] **Step 1: Write the tests**

Add `"github.com/bluenviron/gortsplib/v5"` and `"github.com/bluenviron/gortsplib/v5/pkg/base"` to
`integration_test.go`'s import block, then add:

```go
// TestAddCameraDialableThenRemovedRefuses drives the whole stack: a camera
// added at runtime is immediately dialable with a real RTSP client, and once
// removed, the same URL is refused.
func TestAddCameraDialableThenRemovedRefuses(t *testing.T) {
	f := StartT(t, minimalSpec())

	cam, err := f.AddCamera(CameraSpec{ID: "runtime-cam"})
	if err != nil {
		t.Fatalf("AddCamera: %v", err)
	}

	u, err := base.ParseURL(cam.RTSPURL())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	if _, _, err := c.Describe(u); err != nil {
		c.Close()
		t.Fatalf("DESCRIBE runtime-cam: %v", err)
	}
	c.Close()

	if err := f.RemoveCamera("runtime-cam"); err != nil {
		t.Fatalf("RemoveCamera: %v", err)
	}

	c2 := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
	if err := c2.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer c2.Close()
	if _, _, err := c2.Describe(u); err == nil {
		t.Fatal("DESCRIBE of a removed camera succeeded")
	} else if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want a 404", err)
	}
}

// TestAddCameraSeedDerivationIsDeterministic mirrors
// TestSameSeedReproducesCameraSeeds for the dynamic path: the same root seed,
// driven through the same sequence of AddCamera calls, must derive the same
// seeds for the added cameras.
func TestAddCameraSeedDerivationIsDeterministic(t *testing.T) {
	spec := Spec{Seed: 0xdeadbeefcafe, Cameras: []CameraSpec{{ID: "a"}, {ID: "b"}}}

	collect := func() map[string]uint64 {
		f := StartT(t, spec)
		out := map[string]uint64{}
		for _, id := range []string{"c", "d", "e"} {
			cam, err := f.AddCamera(CameraSpec{ID: id})
			if err != nil {
				t.Fatalf("AddCamera(%s): %v", id, err)
			}
			out[id] = cam.Stats().Seed
		}
		return out
	}

	first := collect()
	second := collect()
	for id, want := range first {
		if got := second[id]; got != want {
			t.Errorf("%s: seed %#x then %#x", id, want, got)
		}
	}
}
```

- [ ] **Step 2: Run the tests**

Run: `go test . -run 'TestAddCameraDialableThenRemovedRefuses|TestAddCameraSeedDerivationIsDeterministic' -race -v`
Expected: PASS (this task verifies the already-implemented Tasks 1-7 end to end; it is not
expected to fail first, unlike the earlier tasks that introduced new production code).

- [ ] **Step 3: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add integration_test.go
git commit -m "Add end-to-end AddCamera/RemoveCamera and seed-determinism tests"
```

---

### Task 9: Documentation — README and CLAUDE.md

**Files:**
- Modify: `README.md`
- Modify: `CLAUDE.md`

- [ ] **Step 1: Update README's Status section**

In `README.md`, find the ONVIF "Works" bullet (it ends with the `> Implements/supports...`
blockquote about ONVIF conformance) and the following "Not built yet" bullet. Insert a new bullet
between them:

```markdown
- **Works:** a Go API for mutating a running fleet — `Fleet.AddCamera` starts a new camera (RTSP,
  and its ONVIF endpoint when the camera's spec sets credentials) without restarting the process,
  and `Fleet.RemoveCamera` stops and removes one. Both apply the same validation `Start` does, so a
  spec that would be refused at startup is refused here too.
```

- [ ] **Step 2: Move CLAUDE.md's "Configuration surface" decision from open to resolved**

In `CLAUDE.md`, under `## Resolved architectural decisions`, after the existing item 2, add:

```markdown
3. **Configuration surface: resolved as both, with a runtime API alongside the declarative spec.**
   `Fleet.AddCamera`/`Fleet.RemoveCamera` (`docs/superpowers/specs/2026-09-13-dynamic-camera-lifecycle-design.md`)
   are the runtime half; the declarative `Spec` remains the source of truth for a fleet's initial
   construction, and both paths share the same per-camera validation so they cannot enforce
   different rules for what a valid camera is. This does not resolve the dashboard or the binary
   that will drive this API interactively — those are later slices built on top of it.
```

Under `## Open architectural decisions`, find:

```markdown
6. **Configuration surface.** A declarative fleet spec (for example YAML) describing the fleet up
   front, versus a runtime API for adding, removing, and mutating cameras while a test is running,
   versus offering both. Note that both `framelag` and `onvif-mcp` will want to drive this farm
   programmatically in the middle of a test run, which bears on how much a static declarative spec
   alone can cover.

7. **Library or binary.** This needs to be usable as a Go library from another project's tests,
   something like `camfarm.Start(spec)` inside a `TestMain`, and not only as a standalone binary,
   because making other repositories testable without hardware is the whole point of building it. Frame
   what that implies for the public API surface: what must be stable, what can change, and what a
   caller needs from a single import.
```

Replace with (item 6 removed, item 7 renumbered to 6, items 1-5 unchanged — matching the exact
renumbering convention `git show 387060a -- CLAUDE.md` used for the previous two resolved items):

```markdown
6. **Library or binary.** This needs to be usable as a Go library from another project's tests,
   something like `camfarm.Start(spec)` inside a `TestMain`, and not only as a standalone binary,
   because making other repositories testable without hardware is the whole point of building it. Frame
   what that implies for the public API surface: what must be stable, what can change, and what a
   caller needs from a single import.
```

- [ ] **Step 3: Commit**

```bash
git add README.md CLAUDE.md
git commit -m "Document Fleet.AddCamera/RemoveCamera; resolve the configuration-surface decision"
```
