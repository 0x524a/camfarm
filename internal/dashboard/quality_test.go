package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0x524A/rtspeek/pkg/rtspeek"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/quality"
)

// qfake is the analyser seam: a call blocks until release is closed or its
// context ends.
type qfake struct {
	release chan struct{}
	res     *rtspeek.Analysis
	live    atomic.Int32
	urls    chan string
	opts    chan rtspeek.AnalyzeOptions
}

func newQFake(res *rtspeek.Analysis) *qfake {
	return &qfake{release: make(chan struct{}), res: res, urls: make(chan string, 16), opts: make(chan rtspeek.AnalyzeOptions, 16)}
}

func (f *qfake) analyze(ctx context.Context, url string, o rtspeek.AnalyzeOptions) (*rtspeek.Analysis, error) {
	f.live.Add(1)
	defer f.live.Add(-1)
	f.urls <- url
	f.opts <- o
	select {
	case <-f.release:
		return f.res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// qualityServer serves a two-camera fleet ("front-door", "back-door") with a
// runner whose analyser is f.
func qualityServer(t *testing.T, f *qfake, max int) (*httptest.Server, *camfarm.Fleet, *quality.Runner) {
	t.Helper()
	fleet := camfarm.StartT(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "front-door"}, {ID: "back-door"}}})
	r := quality.New(quality.Options{MaxConcurrent: max, Analyze: f.analyze})
	t.Cleanup(r.Close)
	ts := httptest.NewServer(Handler(fleet, Options{Quality: r}))
	t.Cleanup(ts.Close)
	return ts, fleet, r
}

func decodeRun(t *testing.T, resp *http.Response) quality.Run {
	t.Helper()
	var run quality.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	return run
}

func qURL(ts *httptest.Server, id string) string {
	return ts.URL + "/api/v1/cameras/" + id + "/quality"
}

func waitRun(t *testing.T, ts *httptest.Server, id string, want quality.State) quality.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp := do(t, "GET", qURL(ts, id), "")
		if resp.StatusCode == 200 {
			if run := decodeRun(t, resp); run.State == want {
				return run
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("camera %q never reached %q", id, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestQualityRoutesAre404WhenDisabled(t *testing.T) {
	ts, _ := newServer(t, Options{})
	for _, m := range []string{"POST", "GET", "DELETE"} {
		if r := do(t, m, qURL(ts, "front-door"), ""); r.StatusCode != 404 {
			t.Errorf("%s = %d, want 404", m, r.StatusCode)
		}
	}
}

func TestStartQualityTestsTheCamerasOwnURL(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{Findings: []rtspeek.Finding{{Severity: "info", Code: "x", Message: "m"}}})
	ts, fleet, _ := qualityServer(t, f, 0)
	cam, _ := fleet.Camera("front-door")

	resp := do(t, "POST", qURL(ts, "front-door"), "") // no body at all
	if resp.StatusCode != 202 {
		t.Fatalf("start = %d", resp.StatusCode)
	}
	run := decodeRun(t, resp)
	if run.State != quality.StateRunning || run.Camera != "front-door" ||
		run.Request != (quality.Request{DurationSeconds: 5, Transport: quality.TransportAuto}) {
		t.Errorf("run = %+v", run)
	}
	if u := <-f.urls; u != cam.RTSPURL() {
		t.Errorf("analysed %q, want the camera's own %q", u, cam.RTSPURL())
	}

	close(f.release)
	done := waitRun(t, ts, "front-door", quality.StateDone)
	if done.Verdict != "ok" || done.Analysis == nil || len(done.Analysis.Findings) != 1 {
		t.Errorf("done = %+v", done)
	}
}

func TestStartQualityPassesTheOptionsThrough(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{})
	ts, _, _ := qualityServer(t, f, 0)
	resp := do(t, "POST", qURL(ts, "front-door"), `{"durationSeconds":2,"transport":"tcp"}`)
	if resp.StatusCode != 202 {
		t.Fatalf("start = %d", resp.StatusCode)
	}
	<-f.urls
	if o := <-f.opts; o.Duration != 2*time.Second || o.Transport != rtspeek.TransportTCP {
		t.Errorf("options = %+v", o)
	}
}

func TestStartQualityAcceptsNullAndChunkedBodies(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{})
	close(f.release)
	ts, _, _ := qualityServer(t, f, 0)

	if r := do(t, "POST", qURL(ts, "front-door"), `null`); r.StatusCode != 202 {
		t.Errorf("null body = %d, want 202", r.StatusCode)
	}
	waitRun(t, ts, "front-door", quality.StateDone)

	for name, body := range map[string]string{"empty chunked": "", "json chunked": `{"durationSeconds":2}`} {
		req, _ := http.NewRequest("POST", qURL(ts, "back-door"), io.NopCloser(strings.NewReader(body))) // unknown length: chunked
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 202 {
			t.Errorf("%s = %d, want 202", name, resp.StatusCode)
		}
		waitRun(t, ts, "back-door", quality.StateDone)
	}
}

func TestQualityStatusCodes(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{})
	ts, _, _ := qualityServer(t, f, 0)

	for _, m := range []string{"POST", "GET", "DELETE"} {
		if r := do(t, m, qURL(ts, "no-such-camera"), ""); r.StatusCode != 404 {
			t.Errorf("%s on an unknown camera = %d, want 404", m, r.StatusCode)
		}
	}
	if r := do(t, "GET", qURL(ts, "front-door"), ""); r.StatusCode != 404 {
		t.Errorf("GET before any run = %d, want 404", r.StatusCode)
	}
	if r := do(t, "DELETE", qURL(ts, "front-door"), ""); r.StatusCode != 404 {
		t.Errorf("DELETE with nothing running = %d, want 404", r.StatusCode)
	}

	for name, body := range map[string]string{
		"too short": `{"durationSeconds":0.5}`, "too long": `{"durationSeconds":99}`,
		"negative": `{"durationSeconds":-3}`, "bad transport": `{"transport":"sctp"}`,
	} {
		if r := do(t, "POST", qURL(ts, "front-door"), body); r.StatusCode != 422 {
			t.Errorf("%s = %d, want 422", name, r.StatusCode)
		}
	}
	if r := do(t, "POST", qURL(ts, "front-door"), `{"url":"rtsp://elsewhere/x"}`); r.StatusCode != 400 {
		t.Errorf("a url field = %d, want 400 (the URL is never taken from the request)", r.StatusCode)
	}
	req, _ := http.NewRequest("POST", qURL(ts, "front-door"), strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 415 {
		t.Errorf("wrong content type = %d, want 415", resp.StatusCode)
	}
	if len(f.urls) != 0 {
		t.Error("a rejected request reached the analyser")
	}

	if r := do(t, "POST", qURL(ts, "front-door"), ""); r.StatusCode != 202 {
		t.Fatalf("start = %d", r.StatusCode)
	}
	if r := do(t, "POST", qURL(ts, "front-door"), ""); r.StatusCode != 409 {
		t.Errorf("second start = %d, want 409", r.StatusCode)
	}
}

func TestQualityCapIs429WithRetryAfter(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{})
	ts, _, _ := qualityServer(t, f, 1)
	if r := do(t, "POST", qURL(ts, "front-door"), ""); r.StatusCode != 202 {
		t.Fatalf("first start = %d", r.StatusCode)
	}
	r := do(t, "POST", qURL(ts, "back-door"), "")
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Errorf("start beyond the cap = %d, Retry-After %q", r.StatusCode, r.Header.Get("Retry-After"))
	}
}

func TestDeleteQualityCancelsARunningTest(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{})
	ts, _, _ := qualityServer(t, f, 0)
	do(t, "POST", qURL(ts, "front-door"), "")
	<-f.urls
	if r := do(t, "DELETE", qURL(ts, "front-door"), ""); r.StatusCode != 204 {
		t.Fatalf("DELETE = %d, want 204", r.StatusCode)
	}
	run := decodeRun(t, do(t, "GET", qURL(ts, "front-door"), ""))
	if run.State != quality.StateFailed || run.Error != "cancelled" {
		t.Errorf("run = %+v", run)
	}
	if f.live.Load() != 0 {
		t.Error("the analysis was still running after DELETE returned")
	}
}

func TestQualityRefusesCrossSiteRequests(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{})
	ts, _, _ := qualityServer(t, f, 0)
	for _, m := range []string{"POST", "DELETE"} {
		req, _ := http.NewRequest(m, qURL(ts, "front-door"), nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("cross-site %s = %d, want 403", m, resp.StatusCode)
		}
	}
	if len(f.urls) != 0 {
		t.Error("a cross-site request started a test")
	}
}

func TestCameraDTOCarriesTheQualitySummary(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{Findings: []rtspeek.Finding{{Severity: "warn", Code: "w", Message: "m"}}})
	ts, _, _ := qualityServer(t, f, 0)
	get := func() map[string]any {
		var m map[string]any
		_ = json.NewDecoder(do(t, "GET", ts.URL+"/api/v1/cameras/front-door", "").Body).Decode(&m)
		return m
	}
	if _, has := get()["quality"]; has {
		t.Error("a camera with no result must not carry a quality field")
	}
	do(t, "POST", qURL(ts, "front-door"), "")
	if q, _ := get()["quality"].(map[string]any); q["state"] != "running" {
		t.Errorf("running summary = %v", q)
	}
	close(f.release)
	waitRun(t, ts, "front-door", quality.StateDone)
	q, _ := get()["quality"].(map[string]any)
	if q["state"] != "done" || q["verdict"] != "warn" || q["finishedAt"] == nil {
		t.Errorf("done summary = %v", q)
	}
	var list []map[string]any
	_ = json.NewDecoder(do(t, "GET", ts.URL+"/api/v1/cameras", "").Body).Decode(&list)
	for _, c := range list {
		if _, has := c["quality"]; has != (c["id"] == "front-door") {
			t.Errorf("camera %v: quality present = %v", c["id"], has)
		}
	}
}

func TestRemovingACameraCancelsAndForgetsItsTest(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{})
	ts, _, r := qualityServer(t, f, 0)
	do(t, "POST", qURL(ts, "front-door"), "")
	<-f.urls
	if resp := do(t, "DELETE", ts.URL+"/api/v1/cameras/front-door", ""); resp.StatusCode != 204 {
		t.Fatalf("remove camera = %d", resp.StatusCode)
	}
	if f.live.Load() != 0 {
		t.Error("the test kept running after its camera was removed")
	}
	if _, ok := r.Get("front-door"); ok {
		t.Error("the removed camera's result was kept")
	}
	// A failed removal must not forget anything.
	do(t, "POST", qURL(ts, "back-door"), "")
	<-f.urls
	if resp := do(t, "DELETE", ts.URL+"/api/v1/cameras/no-such-camera", ""); resp.StatusCode != 404 {
		t.Fatalf("remove unknown = %d", resp.StatusCode)
	}
	if run, ok := r.Get("back-door"); !ok || run.State != quality.StateRunning {
		t.Errorf("an unrelated removal disturbed back-door: %+v %v", run, ok)
	}
}
