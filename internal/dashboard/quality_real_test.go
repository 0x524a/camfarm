package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/0x524A/rtspeek/pkg/rtspeek"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/library"
	"github.com/0x524a/camfarm/internal/media/fixture"
	"github.com/0x524a/camfarm/internal/quality"
)

func realServer(t *testing.T, spec camfarm.Spec, lib *library.Library) (*httptest.Server, *camfarm.Fleet) {
	t.Helper()
	fleet := camfarm.StartT(t, spec)
	r := quality.New(quality.Options{}) // the real rtspeek
	t.Cleanup(r.Close)
	ts := httptest.NewServer(Handler(fleet, Options{Quality: r, Library: lib}))
	t.Cleanup(ts.Close)
	return ts, fleet
}

// runReal starts a test and waits for it to leave the running state.
func runReal(t *testing.T, ts *httptest.Server, id, body string) quality.Run {
	t.Helper()
	if r := do(t, "POST", qURL(ts, id), body); r.StatusCode != 202 {
		t.Fatalf("start = %d", r.StatusCode)
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		run := decodeRun(t, do(t, "GET", qURL(ts, id), ""))
		if run.State != quality.StateRunning {
			return run
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the test on %q never finished", id)
	return quality.Run{}
}

func hasFinding(a *rtspeek.Analysis, code string) bool {
	for _, f := range a.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

func TestRealQualityOnACleanCamera(t *testing.T) {
	ts, fleet := realServer(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "clean"}}}, nil)
	hash, replay, journal := fleet.SpecHash(), fleet.ReplayLine(), len(fleet.Journal())

	run := runReal(t, ts, "clean", `{"durationSeconds":2,"transport":"tcp"}`)
	if run.State != quality.StateDone || run.Analysis == nil || len(run.Analysis.Tracks) == 0 {
		t.Fatalf("run = %+v", run)
	}
	frames := uint64(0)
	for _, tr := range run.Analysis.Tracks {
		if tr.Frames != nil {
			frames += tr.Frames.Frames
		}
	}
	if frames == 0 {
		t.Error("no frames were counted")
	}
	for _, f := range run.Analysis.Findings {
		if f.Severity == rtspeek.SeverityError {
			t.Errorf("a clean camera produced an error finding: %+v", f)
		}
	}
	if fleet.SpecHash() != hash || fleet.ReplayLine() != replay || len(fleet.Journal()) != journal {
		t.Error("a real quality run changed the fleet's replay identity")
	}
}

func TestRealQualitySeesASeededPacketLoss(t *testing.T) {
	spec := camfarm.Spec{Seed: 7, Cameras: []camfarm.CameraSpec{{
		ID: "lossy", Faults: []camfarm.FaultSpec{{Kind: "packet_loss", Rate: 0.3, MeanBurst: 3}},
	}}}
	ts, _ := realServer(t, spec, nil)
	// TCP, because it is the only transport camfarm serves: a UDP SETUP gets
	// 461 Unsupported Transport (measured: transport "udp" fails with
	// setup_failed, and "auto" falls back to TCP). The loss still shows on TCP
	// because packet_loss withholds packets at the source after the packetizer
	// has numbered them, so the receiver sees real RTP sequence gaps. Assert
	// only that the finding is present, never the measured percentage.
	run := runReal(t, ts, "lossy", `{"durationSeconds":3,"transport":"tcp"}`)
	if run.State != quality.StateDone || run.Analysis == nil {
		t.Fatalf("run = %+v", run)
	}
	if !hasFinding(run.Analysis, rtspeek.FindingPacketLoss) {
		t.Errorf("no packet_loss finding for a camera dropping 30%% of packets: %+v", run.Analysis.Findings)
	}
}

// A fault that kills the session is a finding or a failed run, never a 5xx.
func TestRealQualityOnASessionKillingFault(t *testing.T) {
	// Look the teardown parameters up in GET /api/v1/faults (the catalogue) if
	// afterFrames below is rejected.
	spec := camfarm.Spec{Seed: 3, Cameras: []camfarm.CameraSpec{{
		ID: "dropper", Faults: []camfarm.FaultSpec{{Kind: "teardown", AfterFrames: 5}},
	}}}
	ts, _ := realServer(t, spec, nil)
	run := runReal(t, ts, "dropper", `{"durationSeconds":3,"transport":"tcp"}`)
	if run.State != quality.StateDone && run.State != quality.StateFailed {
		t.Fatalf("state = %q", run.State)
	}
	if run.State == quality.StateFailed && run.Error == "" {
		t.Error("a failed run must say why")
	}
	// Teardown has an implicit rate of 1, so the session is always killed.
	if run.Analysis == nil {
		t.Fatalf("no analysis for a session-killing fault: %+v", run)
	}
	killed := hasFinding(run.Analysis, rtspeek.FindingStreamInterrupted)
	for _, f := range run.Analysis.Findings {
		if f.Severity == rtspeek.SeverityError {
			killed = true
		}
	}
	if !killed {
		t.Errorf("teardown left no stream_interrupted or error finding: %+v", run.Analysis.Findings)
	}
}

func TestRealQualityCancelStopsPromptly(t *testing.T) {
	ts, _ := realServer(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "slow"}}}, nil)
	if r := do(t, "POST", qURL(ts, "slow"), `{"durationSeconds":30,"transport":"tcp"}`); r.StatusCode != 202 {
		t.Fatalf("start = %d", r.StatusCode)
	}
	time.Sleep(300 * time.Millisecond) // let it connect
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "DELETE", qURL(ts, "slow"), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE did not return within 5s (does rtspeek honour context cancellation?): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("DELETE = %d, want 204", resp.StatusCode)
	}
	if run := decodeRun(t, do(t, "GET", qURL(ts, "slow"), "")); run.State != quality.StateFailed || run.Error != "cancelled" {
		t.Errorf("run = %+v", run)
	}
}

// The whole flow for a user's own file: upload, make a camera from it, test it.
func TestRealQualityOnALibraryFileCamera(t *testing.T) {
	lib, err := library.New(library.Options{Roots: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := realServer(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "seed-cam"}}}, lib)

	var f fileDTO
	resp := postFile(t, ts.URL, "clip.ts", fixture.Bytes())
	if resp.StatusCode != 201 {
		t.Fatalf("upload = %d", resp.StatusCode)
	}
	_ = json.NewDecoder(resp.Body).Decode(&f)
	if r := do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"from-file","libraryId":"`+f.ID+`"}`); r.StatusCode != 201 {
		t.Fatalf("add camera from the library = %d", r.StatusCode)
	}

	run := runReal(t, ts, "from-file", `{"durationSeconds":2,"transport":"tcp"}`)
	if run.State != quality.StateDone || run.Analysis == nil || len(run.Analysis.Tracks) == 0 {
		t.Errorf("run = %+v", run)
	}
}
