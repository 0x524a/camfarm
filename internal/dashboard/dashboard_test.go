package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0x524a/camfarm"
)

func newServer(t *testing.T, opt Options) (*httptest.Server, *camfarm.Fleet) {
	t.Helper()
	f := camfarm.StartT(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "front-door"}}})
	ts := httptest.NewServer(Handler(f, opt))
	t.Cleanup(ts.Close)
	return ts, f
}

func do(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestListAndGetCameras(t *testing.T) {
	ts, _ := newServer(t, Options{})

	resp := do(t, "GET", ts.URL+"/api/v1/cameras", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 1 || list[0]["id"] != "front-door" {
		t.Fatalf("list = %v", list)
	}
	for _, k := range []string{"rtspUrl", "onvifEndpoint", "seed", "codec", "width", "height"} {
		if _, ok := list[0][k]; !ok {
			t.Errorf("camera JSON has no %q: %v", k, list[0])
		}
	}

	resp = do(t, "GET", ts.URL+"/api/v1/cameras/front-door", "")
	if resp.StatusCode != 200 {
		t.Fatalf("get status = %d", resp.StatusCode)
	}
	resp = do(t, "GET", ts.URL+"/api/v1/cameras/nope", "")
	if resp.StatusCode != 404 {
		t.Fatalf("unknown camera status = %d, want 404", resp.StatusCode)
	}
}

func TestAddAndRemoveCamera(t *testing.T) {
	ts, f := newServer(t, Options{})

	resp := do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"lobby","auth":{"username":"a","password":"b"}}`)
	if resp.StatusCode != 201 {
		t.Fatalf("add status = %d, want 201", resp.StatusCode)
	}
	cam, err := f.Camera("lobby")
	if err != nil {
		t.Fatalf("camera not added: %v", err)
	}
	if _, _, ok := cam.Auth(); !ok {
		t.Error("credentials not applied")
	}

	if resp = do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"lobby"}`); resp.StatusCode != 409 {
		t.Errorf("duplicate status = %d, want 409", resp.StatusCode)
	}
	if resp = do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"a/b"}`); resp.StatusCode != 422 {
		t.Errorf("invalid id status = %d, want 422", resp.StatusCode)
	}
	if resp = do(t, "POST", ts.URL+"/api/v1/cameras", `{not json`); resp.StatusCode != 400 {
		t.Errorf("malformed body status = %d, want 400", resp.StatusCode)
	}

	if resp = do(t, "DELETE", ts.URL+"/api/v1/cameras/lobby", ""); resp.StatusCode != 204 {
		t.Errorf("delete status = %d, want 204", resp.StatusCode)
	}
	if resp = do(t, "DELETE", ts.URL+"/api/v1/cameras/lobby", ""); resp.StatusCode != 404 {
		t.Errorf("second delete status = %d, want 404", resp.StatusCode)
	}
}

func TestMutationsRequireJSONContentType(t *testing.T) {
	ts, _ := newServer(t, Options{})
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/cameras", strings.NewReader(`{"id":"x"}`))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 415 {
		t.Fatalf("status = %d, want 415", resp.StatusCode)
	}
}

func TestSnapshotIsAJPEGOfTheRealStream(t *testing.T) {
	ts, _ := newServer(t, Options{})
	resp := do(t, "GET", ts.URL+"/api/v1/cameras/front-door/snapshot.jpg", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q", ct)
	}
	img, err := jpeg.Decode(resp.Body)
	if err != nil {
		t.Fatalf("not a JPEG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 320 || b.Dy() != 240 {
		t.Errorf("bounds = %v, want 320x240", b)
	}
	if resp = do(t, "GET", ts.URL+"/api/v1/cameras/nope/snapshot.jpg", ""); resp.StatusCode != 404 {
		t.Errorf("unknown camera snapshot status = %d, want 404", resp.StatusCode)
	}
}

// capturing returns a capture seam that counts calls and blocks until released.
func capturing(release <-chan struct{}) (func(context.Context, *camfarm.Camera) (image.Image, error), *atomic.Int32) {
	var n atomic.Int32
	return func(ctx context.Context, _ *camfarm.Camera) (image.Image, error) {
		n.Add(1)
		select {
		case <-release:
			return image.NewRGBA(image.Rect(0, 0, 4, 4)), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, &n
}

func TestConcurrentSnapshotsOfOneCameraAreCoalesced(t *testing.T) {
	release := make(chan struct{})
	capture, calls := capturing(release)
	ts, _ := newServer(t, Options{capture: capture})

	const clients = 5
	var wg sync.WaitGroup
	statuses := make([]int, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(ts.URL + "/api/v1/cameras/front-door/snapshot.jpg")
			if err != nil {
				t.Errorf("client %d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			statuses[i] = resp.StatusCode
		}()
	}
	// Let every request join the one in-flight capture before releasing it.
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("captures = %d, want 1 shared by %d requests", got, clients)
	}
	for i, s := range statuses {
		if s != 200 {
			t.Errorf("client %d status = %d", i, s)
		}
	}
}

func TestSnapshotTimeoutIs504(t *testing.T) {
	capture, _ := capturing(make(chan struct{})) // never released
	ts, _ := newServer(t, Options{capture: capture, SnapshotTimeout: 200 * time.Millisecond})
	start := time.Now()
	resp := do(t, "GET", ts.URL+"/api/v1/cameras/front-door/snapshot.jpg", "")
	if resp.StatusCode != 504 {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the timeout did not bound the request")
	}
}

func TestSnapshotConcurrencyCap(t *testing.T) {
	release := make(chan struct{})
	capture, calls := capturing(release)
	ts, f := newServer(t, Options{capture: capture, MaxConcurrentSnapshots: 1, SnapshotTimeout: 5 * time.Second})
	if _, err := f.AddCamera(camfarm.CameraSpec{ID: "second"}); err != nil {
		t.Fatalf("AddCamera: %v", err)
	}

	var wg sync.WaitGroup
	for _, id := range []string{"front-door", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(ts.URL + "/api/v1/cameras/" + id + "/snapshot.jpg")
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("captures in flight = %d, want 1 under a cap of 1", got)
	}
	close(release)
	wg.Wait()
	if got := calls.Load(); got != 2 {
		t.Fatalf("total captures = %d, want 2", got)
	}
}

func TestInjectAndClearFaultOverHTTP(t *testing.T) {
	ts, f := newServer(t, Options{})
	cam, _ := f.Camera("front-door")

	resp := do(t, "POST", ts.URL+"/api/v1/cameras/front-door/faults", `{"kind":"frame_drop","rate":0.5,"protectKeyframes":true}`)
	if resp.StatusCode != 200 {
		t.Fatalf("inject status = %d", resp.StatusCode)
	}
	if got := cam.Faults(); len(got) != 1 || !got[0].ProtectKeyframes {
		t.Fatalf("Faults() = %+v", got)
	}
	if resp = do(t, "POST", ts.URL+"/api/v1/cameras/front-door/faults", `{"kind":"accept_then_silence","rate":1}`); resp.StatusCode != 422 {
		t.Errorf("unsupported kind status = %d, want 422", resp.StatusCode)
	}
	if resp = do(t, "POST", ts.URL+"/api/v1/cameras/nope/faults", `{"kind":"frame_drop","rate":1}`); resp.StatusCode != 404 {
		t.Errorf("unknown camera status = %d, want 404", resp.StatusCode)
	}
	if resp = do(t, "DELETE", ts.URL+"/api/v1/cameras/front-door/faults", ""); resp.StatusCode != 204 {
		t.Errorf("clear status = %d, want 204", resp.StatusCode)
	}
	if len(cam.Faults()) != 0 {
		t.Error("faults not cleared")
	}
}

func TestRootServesTheEmbeddedPage(t *testing.T) {
	ts, _ := newServer(t, Options{})
	resp := do(t, "GET", ts.URL+"/", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	if !strings.Contains(strings.ToLower(buf.String()), "<!doctype html>") {
		t.Errorf("root is not an HTML page: %.80q", buf.String())
	}
}

func TestEmbeddedPageCarriesTheTestHooks(t *testing.T) {
	ts, _ := newServer(t, Options{})
	resp := do(t, "GET", ts.URL+"/", "")
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	page := buf.String()
	// This only pins the construction patterns in the page source. The Task 10
	// browser tests are the real contract for these hooks.
	for _, hook := range []string{
		`<code id="replay">`,
		`<div id="error" role="alert"></div>`,
		`<p id="conn" role="status">`,
		`<form id="add-form"`,
		`<input id="add-id" required`,
		`<input id="add-user"`,
		`<input id="add-pass"`,
		`<select id="add-source">`,
		`<option value="fixture">`,
		`<option value="file">`,
		`<input id="add-path"`,
		`<button id="add-submit" type="submit">`,
		`<table id="cameras">`,
		`"data-id": c.id`,
		`class: "btn-snapshot secondary"`,
		`class: "btn-remove danger"`,
		`class: "btn-fault"`,
		`class: "btn-clear-fault secondary"`,
		`class: "snapshot"`,
		`class: "fault-rate", type: "number"`,
		`class: "fault-protect", type: "checkbox"`,
		`class: "fault-active"`,
		`class: "copy secondary", type: "button"`,
		`"data-copy": value`,
		`"cell-rtsp"`,
		`"cell-onvif"`,
	} {
		if !strings.Contains(page, hook) {
			t.Errorf("page is missing hook %q", hook)
		}
	}
}

func TestCancelledWaiterDoesNotCancelSharedCapture(t *testing.T) {
	release := make(chan struct{})
	capture, calls := capturing(release)
	ts, _ := newServer(t, Options{capture: capture})
	url := ts.URL + "/api/v1/cameras/front-door/snapshot.jpg"

	ctx, cancel := context.WithCancel(context.Background())
	cancelledDone := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		cancelledDone <- err
	}()
	okStatus := make(chan int, 1)
	go func() {
		resp, err := http.Get(url)
		if err != nil {
			t.Errorf("second waiter: %v", err)
			okStatus <- 0
			return
		}
		resp.Body.Close()
		okStatus <- resp.StatusCode
	}()
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-cancelledDone:
		if err == nil {
			t.Error("cancelled request unexpectedly succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled waiter did not return promptly")
	}
	close(release)
	if s := <-okStatus; s != 200 {
		t.Errorf("other waiter status = %d, want 200", s)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("captures = %d, want 1", got)
	}
}

func TestInFlightMapIsEmptyAfterSuccessAndTimeout(t *testing.T) {
	f := camfarm.StartT(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "c"}}})
	cam, err := f.Camera("c")
	if err != nil {
		t.Fatal(err)
	}
	var n atomic.Int32
	var hang atomic.Bool
	s := newSnapshotter(Options{SnapshotTimeout: 100 * time.Millisecond, capture: func(ctx context.Context, _ *camfarm.Camera) (image.Image, error) {
		n.Add(1)
		if hang.Load() {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return image.NewRGBA(image.Rect(0, 0, 4, 4)), nil
	}})
	empty := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.flights) == 0
	}
	if _, err := s.get(context.Background(), cam); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !empty() {
		t.Error("flight left behind after success")
	}
	hang.Store(true)
	if _, err := s.get(context.Background(), cam); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if !empty() {
		t.Error("flight left behind after timeout")
	}
	hang.Store(false)
	if _, err := s.get(context.Background(), cam); err != nil {
		t.Fatalf("get after timeout: %v", err)
	}
	if got := n.Load(); got != 3 {
		t.Errorf("captures = %d, want 3 (each request started a fresh one)", got)
	}
}

func TestPanickingCaptureIs500AndRecovers(t *testing.T) {
	var panicNext atomic.Bool
	panicNext.Store(true)
	ts, _ := newServer(t, Options{capture: func(context.Context, *camfarm.Camera) (image.Image, error) {
		if panicNext.Swap(false) {
			panic("boom")
		}
		return image.NewRGBA(image.Rect(0, 0, 4, 4)), nil
	}})
	url := ts.URL + "/api/v1/cameras/front-door/snapshot.jpg"
	if resp := do(t, "GET", url, ""); resp.StatusCode != 500 {
		t.Fatalf("panic status = %d, want 500", resp.StatusCode)
	}
	if resp := do(t, "GET", url, ""); resp.StatusCode != 200 {
		t.Fatalf("next request status = %d, want 200", resp.StatusCode)
	}
}

func TestBodyLimitsAndStrictness(t *testing.T) {
	ts, _ := newServer(t, Options{})
	big := `{"id":"` + strings.Repeat("x", 2<<20) + `"}`
	if resp := do(t, "POST", ts.URL+"/api/v1/cameras", big); resp.StatusCode != 413 {
		t.Errorf("oversize status = %d, want 413", resp.StatusCode)
	}
	if resp := do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"x","bogus":1}`); resp.StatusCode != 400 {
		t.Errorf("unknown field status = %d, want 400", resp.StatusCode)
	}
	if resp := do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"x"} {"id":"y"}`); resp.StatusCode != 400 {
		t.Errorf("trailing data status = %d, want 400", resp.StatusCode)
	}
}

func TestReaddedCameraDoesNotJoinOldFlight(t *testing.T) {
	release := make(chan struct{})
	capture, calls := capturing(release)
	ts, f := newServer(t, Options{capture: capture})
	url := ts.URL + "/api/v1/cameras/front-door/snapshot.jpg"

	var wg sync.WaitGroup
	get := func() {
		defer wg.Done()
		resp, err := http.Get(url)
		if err != nil {
			t.Errorf("GET: %v", err)
			return
		}
		resp.Body.Close()
	}
	wg.Add(1)
	go get()
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := f.RemoveCamera("front-door"); err != nil {
		t.Fatalf("RemoveCamera: %v", err)
	}
	if _, err := f.AddCamera(camfarm.CameraSpec{ID: "front-door"}); err != nil {
		t.Fatalf("AddCamera: %v", err)
	}
	wg.Add(1)
	go get()
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("captures = %d, want 2: the re-added camera joined the old flight", got)
	}
	close(release)
	wg.Wait()
}

// Seeds are uint64 and JavaScript rounds JSON numbers above 2^53, so the API
// must send them as decimal strings.
func TestSeedsAbove2Pow53RoundTripExactly(t *testing.T) {
	const root = uint64(0xfedcba9876543210)
	f := camfarm.StartT(t, camfarm.Spec{Seed: root, Cameras: []camfarm.CameraSpec{{ID: "c"}}})
	ts := httptest.NewServer(Handler(f, Options{}))
	t.Cleanup(ts.Close)

	var fleet struct {
		Seed string `json:"seed"`
	}
	resp := do(t, "GET", ts.URL+"/api/v1/fleet", "")
	if err := json.NewDecoder(resp.Body).Decode(&fleet); err != nil {
		t.Fatal(err)
	}
	if fleet.Seed != strconv.FormatUint(root, 10) {
		t.Errorf("fleet seed = %q, want %d", fleet.Seed, root)
	}

	cam, err := f.Camera("c")
	if err != nil {
		t.Fatal(err)
	}
	want := strconv.FormatUint(cam.Stats().Seed, 10)
	var list []struct {
		Seed  string `json:"seed"`
		Stats struct{ Seed string }
	}
	resp = do(t, "GET", ts.URL+"/api/v1/cameras", "")
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Seed != want || list[0].Stats.Seed != want {
		t.Errorf("camera seeds = %+v, want %s", list, want)
	}
	if cam.Stats().Seed <= 1<<53 {
		t.Logf("derived camera seed %d is below 2^53; only the fleet seed exercises the bound", cam.Stats().Seed)
	}
}

func TestFleetEndpoint(t *testing.T) {
	ts, _ := newServer(t, Options{})
	var body struct {
		Seed       string `json:"seed"`
		SpecHash   string `json:"specHash"`
		ReplayLine string `json:"replayLine"`
		Journal    []any  `json:"journal"`
	}
	resp := do(t, "GET", ts.URL+"/api/v1/fleet", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Seed != "1" || body.SpecHash == "" || !strings.Contains(body.ReplayLine, "seed=0x1") {
		t.Errorf("fleet = %+v", body)
	}
	if body.Journal == nil {
		t.Error("journal should be an empty array, not null")
	}
}

// openStream connects to the event stream and returns a line reader that fails
// the test after a generous timeout, plus a function that cancels the request.
func openStream(t *testing.T, url string) (next func() string, cancel context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", url+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	next = func() string {
		t.Helper()
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("stream closed")
			}
			return l
		case <-time.After(10 * time.Second):
			t.Fatal("timed out reading the event stream")
			return ""
		}
	}
	if l := next(); l != ": connected" {
		t.Fatalf("first line = %q, want the connected comment", l)
	}
	return next, cancel
}

func awaitEvent(t *testing.T, next func() string, typ, camera string) {
	t.Helper()
	var sawEvent, sawData bool
	for !(sawEvent && sawData) {
		l := next()
		switch {
		case l == "event: "+typ:
			sawEvent = true
		case strings.HasPrefix(l, "data: ") && strings.Contains(l, `"camera":"`+camera+`"`):
			sawData = true
		}
	}
}

func TestEventStreamDeliversChanges(t *testing.T) {
	ts, _ := newServer(t, Options{})
	next, _ := openStream(t, ts.URL)

	if r := do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"lobby"}`); r.StatusCode != 201 {
		t.Fatalf("add status = %d", r.StatusCode)
	}
	awaitEvent(t, next, "camera_added", "lobby")
}

func TestEventStreamSurvivesFaultThenRemoval(t *testing.T) {
	ts, _ := newServer(t, Options{})
	next, _ := openStream(t, ts.URL)

	if r := do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"gone"}`); r.StatusCode != 201 {
		t.Fatalf("add status = %d", r.StatusCode)
	}
	if r := do(t, "POST", ts.URL+"/api/v1/cameras/gone/faults", `{"kind":"frame_drop","rate":1}`); r.StatusCode != 200 {
		t.Fatalf("inject status = %d", r.StatusCode)
	}
	if r := do(t, "DELETE", ts.URL+"/api/v1/cameras/gone", ""); r.StatusCode != 204 {
		t.Fatalf("remove status = %d", r.StatusCode)
	}
	awaitEvent(t, next, "camera_removed", "gone")
	// The stream must still be alive for later changes.
	if r := do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"after"}`); r.StatusCode != 201 {
		t.Fatalf("add status = %d", r.StatusCode)
	}
	awaitEvent(t, next, "camera_added", "after")
}

func TestEventStreamHandlerEndsOnDisconnect(t *testing.T) {
	f := camfarm.StartT(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "front-door"}}})
	h := Handler(f, Options{})
	returned := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		if r.URL.Path == "/api/v1/events" {
			returned <- struct{}{}
		}
	}))
	t.Cleanup(ts.Close)

	_, cancel := openStream(t, ts.URL)
	cancel()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("events handler did not return after the client disconnected")
	}
}
