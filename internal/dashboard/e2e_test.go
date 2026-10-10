//go:build e2e

// Real-browser tests of the dashboard. They sit behind the e2e build tag so the
// default `go test ./...` needs no browser. A missing browser fails the tests;
// it never skips them.
//
// Install the browser once with:
//
//	go run github.com/playwright-community/playwright-go/cmd/playwright install --with-deps chromium
//
// CAMFARM_E2E_CHROMIUM may name a Chromium/Chrome executable to launch instead
// of the one Playwright downloaded.
package dashboard

import (
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/playwright-community/playwright-go"

	"github.com/0x524a/camfarm"
)

const assertTimeoutMS = 15000

var (
	envOnce sync.Once
	pw      *playwright.Playwright
	browser playwright.Browser
	envErr  error
)

// launch starts Chromium once per test binary. The driver is run with
// SkipInstallBrowsers so a missing browser surfaces as a failure instead of a
// silent multi-hundred-megabyte download.
func launch(t *testing.T) playwright.Browser {
	t.Helper()
	envOnce.Do(func() {
		pw, envErr = playwright.Run(&playwright.RunOptions{SkipInstallBrowsers: true})
		if envErr != nil {
			return
		}
		opts := playwright.BrowserTypeLaunchOptions{Headless: playwright.Bool(true)}
		if p := os.Getenv("CAMFARM_E2E_CHROMIUM"); p != "" {
			opts.ExecutablePath = playwright.String(p)
		}
		browser, envErr = pw.Chromium.Launch(opts)
	})
	if envErr != nil {
		t.Fatalf("starting Playwright/Chromium (run `go run github.com/playwright-community/playwright-go/cmd/playwright install --with-deps chromium`): %v", envErr)
	}
	return browser
}

func TestMain(m *testing.M) {
	code := m.Run()
	if browser != nil {
		_ = browser.Close()
	}
	if pw != nil {
		_ = pw.Stop()
	}
	os.Exit(code)
}

func e2eServer(t *testing.T) (string, *camfarm.Fleet) {
	t.Helper()
	f := camfarm.StartT(t, camfarm.Spec{Seed: 0xE2E, Cameras: []camfarm.CameraSpec{{ID: "front-door"}}})
	ts := httptest.NewServer(Handler(f, Options{}))
	t.Cleanup(ts.Close)
	return ts.URL, f
}

// session is a page plus everything it logged that a clean run must not have.
type session struct {
	page     playwright.Page
	mu       sync.Mutex
	problems []string
}

func (s *session) add(format string, a ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.problems = append(s.problems, fmt.Sprintf(format, a...))
}

// requireClean fails the test if the page logged a console error, a page
// error, a failed request or an HTTP error response. allowedStatus lists
// statuses the test deliberately provoked; the browser's matching "Failed to
// load resource" console line is ignored for those.
func (s *session) requireClean(t *testing.T, allowedStatus ...int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.problems {
		ok := false
		for _, st := range allowedStatus {
			if strings.Contains(p, fmt.Sprintf("status of %d", st)) || strings.Contains(p, fmt.Sprintf("http %d ", st)) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("browser reported a problem: %s", p)
		}
	}
}

func newSession(t *testing.T, opts ...playwright.BrowserNewContextOptions) *session {
	t.Helper()
	b := launch(t)
	var o playwright.BrowserNewContextOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	bctx, err := b.NewContext(o)
	if err != nil {
		t.Fatalf("NewContext: %v", err)
	}
	t.Cleanup(func() { _ = bctx.Close() })
	page, err := bctx.NewPage()
	if err != nil {
		t.Fatalf("NewPage: %v", err)
	}
	s := &session{page: page}
	page.On("console", func(m playwright.ConsoleMessage) {
		if m.Type() == "error" {
			s.add("console error: %s", m.Text())
		}
	})
	page.On("pageerror", func(err error) { s.add("page error: %v", err) })
	// Chromium reports ERR_ABORTED for a body-less response (a 204 to a DELETE)
	// whose body the page never reads, even though the request succeeded. A
	// request that already got a response is therefore not counted as failed.
	var responded sync.Map
	page.On("requestfailed", func(r playwright.Request) {
		if _, ok := responded.Load(r); ok {
			return
		}
		s.add("request failed: %s %s: %v", r.Method(), r.URL(), r.Failure())
	})
	page.On("response", func(r playwright.Response) {
		responded.Store(r.Request(), true)
		if r.Status() >= 400 {
			s.add("http %d from %s %s", r.Status(), r.Request().Method(), r.URL())
		}
	})
	return s
}

func expect() playwright.PlaywrightAssertions {
	return playwright.NewPlaywrightAssertions(assertTimeoutMS)
}

func (s *session) open(t *testing.T, url string) {
	t.Helper()
	if _, err := s.page.Goto(url + "/"); err != nil {
		t.Fatalf("Goto: %v", err)
	}
}

func (s *session) row(id string) playwright.Locator {
	return s.page.Locator(fmt.Sprintf(`#cameras tbody tr[data-id=%q]`, id))
}

func TestE2ECameraTableRendersURLsAndReplayLine(t *testing.T) {
	url, _ := e2eServer(t)
	s := newSession(t)
	s.open(t, url)
	row := s.row("front-door")
	if err := expect().Locator(row).ToBeVisible(); err != nil {
		t.Fatalf("row not visible: %v", err)
	}
	if err := expect().Locator(row.Locator(".cell-rtsp")).ToContainText("rtsp://"); err != nil {
		t.Fatalf("RTSP URL missing: %v", err)
	}
	if err := expect().Locator(row.Locator(".cell-onvif")).ToContainText("/onvif/front-door"); err != nil {
		t.Fatalf("ONVIF endpoint missing: %v", err)
	}
	if err := expect().Locator(s.page.Locator("#replay")).ToContainText("seed=0xe2e"); err != nil {
		t.Fatalf("replay line missing: %v", err)
	}
	if err := expect().Locator(s.page.Locator("#conn")).ToHaveText("connected"); err != nil {
		t.Fatalf("connection status: %v", err)
	}
	s.requireClean(t)
}

func TestE2EAddAndRemoveACamera(t *testing.T) {
	url, _ := e2eServer(t)
	s := newSession(t)
	s.open(t, url)
	if err := s.page.Locator("#add-id").Fill("lobby"); err != nil {
		t.Fatal(err)
	}
	if err := s.page.Locator("#add-submit").Click(); err != nil {
		t.Fatal(err)
	}
	lobby := s.row("lobby")
	if err := expect().Locator(lobby).ToBeVisible(); err != nil {
		t.Fatalf("added camera did not appear: %v", err)
	}
	if err := lobby.Locator(".btn-remove").Click(); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(lobby).ToHaveCount(0); err != nil {
		t.Fatalf("removed camera still shown: %v", err)
	}
	if err := expect().Locator(s.row("front-door")).ToBeVisible(); err != nil {
		t.Fatalf("the other camera disappeared: %v", err)
	}
	s.requireClean(t)
}

func TestE2EDuplicateCameraShowsAnError(t *testing.T) {
	url, _ := e2eServer(t)
	s := newSession(t)
	s.open(t, url)
	if err := s.page.Locator("#add-id").Fill("front-door"); err != nil {
		t.Fatal(err)
	}
	if err := s.page.Locator("#add-submit").Click(); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(s.page.Locator("#error")).ToContainText("duplicate"); err != nil {
		t.Fatalf("no duplicate error shown: %v", err)
	}
	// The one provoked 409 is the only browser-reported problem allowed.
	s.requireClean(t, 409)
}

func TestE2ESnapshotShowsA320x240Image(t *testing.T) {
	url, _ := e2eServer(t)
	s := newSession(t)
	s.open(t, url)
	row := s.row("front-door")
	if err := row.Locator(".btn-snapshot").Click(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.page.WaitForFunction(`() => {
		const i = document.querySelector('tr[data-id="front-door"] img.snapshot');
		return !!i && !i.hidden && i.complete && i.naturalWidth === 320 && i.naturalHeight === 240;
	}`, nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(assertTimeoutMS)}); err != nil {
		t.Fatalf("snapshot never showed a 320x240 image: %v", err)
	}
	s.requireClean(t)
}

func TestE2EFrameDropControlInjectsAndClears(t *testing.T) {
	url, f := e2eServer(t)
	cam, err := f.Camera("front-door")
	if err != nil {
		t.Fatal(err)
	}
	s := newSession(t)
	s.open(t, url)
	row := s.row("front-door")
	if err := row.Locator(".fault-rate").Fill("1"); err != nil {
		t.Fatal(err)
	}
	if err := row.Locator(".fault-protect").Check(); err != nil {
		t.Fatal(err)
	}
	if err := row.Locator(".btn-fault").Click(); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(row.Locator(".fault-active")).ToContainText("frame_drop"); err != nil {
		t.Fatalf("active fault not shown: %v", err)
	}
	if got := cam.Faults(); len(got) != 1 || got[0].Kind != "frame_drop" || got[0].Rate != 1 || !got[0].ProtectKeyframes {
		t.Fatalf("library faults = %+v", got)
	}
	if err := row.Locator(".btn-clear-fault").Click(); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(row.Locator(".fault-active")).ToHaveText("none"); err != nil {
		t.Fatalf("fault not cleared in the UI: %v", err)
	}
	if got := cam.Faults(); len(got) != 0 {
		t.Fatalf("library still has faults after Clear: %+v", got)
	}
	s.requireClean(t)
}

func TestE2EUnimplementedFaultKindsAreDisabled(t *testing.T) {
	url, _ := e2eServer(t)
	s := newSession(t)
	s.open(t, url)
	for _, kind := range []string{"keyframe_starvation", "packet_loss"} {
		opt := s.page.Locator(fmt.Sprintf(`tr[data-id="front-door"] .fault-kind option[value=%q]`, kind))
		if err := expect().Locator(opt).ToBeDisabled(); err != nil {
			t.Fatalf("unimplemented fault kind %s is selectable: %v", kind, err)
		}
	}
}

func TestE2ECopyButtonCopiesTheRTSPURL(t *testing.T) {
	url, f := e2eServer(t)
	cam, err := f.Camera("front-door")
	if err != nil {
		t.Fatal(err)
	}
	s := newSession(t, playwright.BrowserNewContextOptions{
		Permissions: []string{"clipboard-read", "clipboard-write"},
	})
	s.open(t, url)
	btn := s.row("front-door").Locator(`button.copy[data-copy^="rtsp://"]`)
	if err := expect().Locator(btn).ToHaveAttribute("data-copy", cam.RTSPURL()); err != nil {
		t.Fatalf("copy button does not carry the RTSP URL: %v", err)
	}
	if err := btn.Click(); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(btn).ToHaveText("Copied"); err != nil {
		t.Fatalf("no Copied feedback: %v", err)
	}
	got, err := s.page.Evaluate(`navigator.clipboard.readText()`)
	if err != nil {
		t.Fatalf("reading the clipboard: %v", err)
	}
	if str, _ := got.(string); str != cam.RTSPURL() {
		t.Fatalf("clipboard = %v, want %q", got, cam.RTSPURL())
	}
	s.requireClean(t)
}

func TestE2ENarrowViewportDoesNotScrollTheWholePage(t *testing.T) {
	url, _ := e2eServer(t)
	s := newSession(t)
	if err := s.page.SetViewportSize(400, 800); err != nil {
		t.Fatal(err)
	}
	s.open(t, url)
	if err := expect().Locator(s.row("front-door")).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	fits, err := s.page.Evaluate(`document.documentElement.scrollWidth <= document.documentElement.clientWidth`)
	if err != nil {
		t.Fatal(err)
	}
	if fits != true {
		t.Fatal("the page scrolls horizontally at 400px; only the table may")
	}
	s.requireClean(t)
}

func TestE2EBothColourSchemesRender(t *testing.T) {
	url, _ := e2eServer(t)
	bg := func(scheme *playwright.ColorScheme) string {
		s := newSession(t, playwright.BrowserNewContextOptions{ColorScheme: scheme})
		s.open(t, url)
		if err := expect().Locator(s.row("front-door")).ToBeVisible(); err != nil {
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
	if strings.TrimSpace(light) == "" || strings.TrimSpace(dark) == "" || light == dark {
		t.Fatalf("light (%q) and dark (%q) backgrounds should be set and differ", light, dark)
	}
}

// The page polls every 2s, so a change that shows up faster than that came
// over the SSE stream. The test waits for the stream to open before changing
// the fleet, then allows well under one poll interval.
func TestE2EFleetChangeAppearsWithoutReloadOrPoll(t *testing.T) {
	url, f := e2eServer(t)
	s := newSession(t)
	if err := s.page.AddInitScript(playwright.Script{Content: playwright.String(`
		window.__sseOpen = false;
		const ES = window.EventSource;
		window.EventSource = function (u, o) {
			const e = new ES(u, o);
			e.addEventListener('open', () => { window.__sseOpen = true; });
			return e;
		};
		window.EventSource.prototype = ES.prototype;
	`)}); err != nil {
		t.Fatal(err)
	}
	s.open(t, url)
	if err := expect().Locator(s.row("front-door")).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.page.WaitForFunction(`() => window.__sseOpen === true`, nil,
		playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(assertTimeoutMS)}); err != nil {
		t.Fatalf("SSE stream never opened: %v", err)
	}
	if err := expect().Locator(s.page.Locator("#conn")).ToHaveText("connected"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.AddCamera(camfarm.CameraSpec{ID: "from-library"}); err != nil {
		t.Fatal(err)
	}
	fast := playwright.NewPlaywrightAssertions(1500)
	if err := fast.Locator(s.row("from-library")).ToBeVisible(); err != nil {
		t.Fatalf("library-side AddCamera was not pushed to the page: %v", err)
	}
	s.requireClean(t)
}
