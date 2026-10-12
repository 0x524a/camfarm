package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/0x524A/rtspeek/pkg/rtspeek"

	"github.com/0x524a/camfarm/internal/quality"
)

// sseFrames connects to /api/v1/events and delivers (event, data) pairs.
func sseFrames(t *testing.T, base string) <-chan [2]string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	sc := bufio.NewScanner(resp.Body)
	// The handler subscribes before it sends ": connected", so once that comment
	// has arrived any later fleet or quality event is guaranteed to be delivered.
	for connected := false; !connected; {
		if !sc.Scan() {
			t.Fatalf("event stream ended before the connected comment: %v", sc.Err())
		}
		connected = sc.Text() == ": connected"
	}
	out := make(chan [2]string, 16)
	go func() {
		defer close(out)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				out <- [2]string{ev, strings.TrimPrefix(line, "data: ")}
			}
		}
	}()
	return out
}

func nextFrame(t *testing.T, ch <-chan [2]string, event string) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case fr, ok := <-ch:
			if !ok {
				t.Fatalf("stream ended before a %q event", event)
			}
			if fr[0] == event {
				return fr[1]
			}
		case <-deadline:
			t.Fatalf("no %q event", event)
		}
	}
}

func TestQualityEventsArriveOnTheEventStream(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{Findings: []rtspeek.Finding{{Severity: "error", Code: "e", Message: "m"}}})
	ts, _, _ := qualityServer(t, f, 0)
	frames := sseFrames(t, ts.URL)

	do(t, "POST", qURL(ts, "front-door"), "")
	var ev quality.Event
	_ = json.Unmarshal([]byte(nextFrame(t, frames, "quality")), &ev)
	if ev != (quality.Event{Camera: "front-door", State: quality.StateRunning}) {
		t.Errorf("first event = %+v", ev)
	}
	close(f.release)
	_ = json.Unmarshal([]byte(nextFrame(t, frames, "quality")), &ev)
	if ev != (quality.Event{Camera: "front-door", State: quality.StateDone, Verdict: "error"}) {
		t.Errorf("second event = %+v", ev)
	}
}

// The quality handlers and event stream must not touch the fleet: the journal,
// spec hash, replay line and the fleet's own feed stay as they were. (A real run
// is still an RTSP client and counts toward the camera's request index.)
func TestAQualityRunDoesNotTouchTheFleetsReplayIdentity(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{})
	close(f.release)
	ts, fleet, _ := qualityServer(t, f, 0)

	hash, replay, journal := fleet.SpecHash(), fleet.ReplayLine(), len(fleet.Journal())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	feed := fleet.Subscribe(ctx)

	do(t, "POST", qURL(ts, "front-door"), "")
	waitRun(t, ts, "front-door", quality.StateDone)
	// (No DELETE here: the fake is pre-released, so a second run would already
	// be done; cancellation is covered by the lifecycle tests.)

	if fleet.SpecHash() != hash || fleet.ReplayLine() != replay || len(fleet.Journal()) != journal {
		t.Errorf("a quality run changed the fleet's identity: %q %q %d -> %q %q %d",
			hash, replay, journal, fleet.SpecHash(), fleet.ReplayLine(), len(fleet.Journal()))
	}
	select {
	case ev := <-feed:
		t.Errorf("a quality run published to the fleet's feed: %+v", ev)
	default:
	}
}

func TestEventStreamWorksWithoutAQualityRunner(t *testing.T) {
	ts, _ := newServer(t, Options{})
	frames := sseFrames(t, ts.URL)
	if r := do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"late"}`); r.StatusCode != 201 {
		t.Fatalf("add camera = %d", r.StatusCode)
	}
	nextFrame(t, frames, "camera_added") // a fleet event still flows when quality is disabled
}
