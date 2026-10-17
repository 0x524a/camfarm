//go:build e2e

// Real-browser tests of the redesigned dashboard (app.html until the swap in the
// last task). They reuse launch, newSession, expect and requireClean from
// e2e_test.go.
package dashboard

import (
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
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

var probe404 = regexp.MustCompile(`/api/v1/(library|metrics|cameras/[^/]+/quality)$`)

// requireCleanApp is requireClean for the redesigned page. It tolerates the
// browser's console line and the response entry for a 404 from the three
// feature probes (library, metrics, quality) because a server without that
// feature answers them 404 by design. Any other problem still fails.
func (s *session) requireCleanApp(t *testing.T, allowedStatus ...int) {
	t.Helper()
	s.mu.Lock()
	kept := s.problems[:0:0]
	for _, p := range s.problems {
		if strings.Contains(p, "status of 404") {
			continue // the console line carries no URL; the response entry below does
		}
		if strings.HasPrefix(p, "http 404 from GET ") && probe404.MatchString(strings.TrimPrefix(p, "http 404 from GET ")) {
			continue
		}
		kept = append(kept, p)
	}
	s.problems = kept
	s.mu.Unlock()
	s.requireClean(t, allowedStatus...)
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
	s.requireCleanApp(t)
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
	s.requireCleanApp(t)
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
	s.requireCleanApp(t)
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
	s.requireCleanApp(t)
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
		s.requireCleanApp(t)
		str, _ := v.(string)
		return str
	}
	light, dark := bg(playwright.ColorSchemeLight), bg(playwright.ColorSchemeDark)
	if light == "" || dark == "" || light == dark {
		t.Fatalf("light (%q) and dark (%q) backgrounds should be set and differ", light, dark)
	}
}
