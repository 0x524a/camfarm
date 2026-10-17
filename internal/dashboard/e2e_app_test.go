//go:build e2e

// Real-browser tests of the redesigned dashboard (app.html until the swap in the
// last task). They reuse launch, newSession, expect and requireClean from
// e2e_test.go.
package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/playwright-community/playwright-go"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/hostmetrics"
	"github.com/0x524a/camfarm/internal/library"
	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/media/fixture"
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

func TestAppConnectionPillTracksCameraRefreshFailuresAndRecovers(t *testing.T) {
	url, f := appServer(t, appOpts{})
	s := newSession(t)
	s.openApp(t, url)
	conn := s.page.Locator("#conn")
	if err := expect().Locator(conn).ToHaveText("connected"); err != nil {
		t.Fatal(err)
	}
	const glob = "**/api/v1/cameras"
	if err := s.page.Route(glob, func(r playwright.Route) {
		_ = r.Fulfill(playwright.RouteFulfillOptions{
			Status:      playwright.Int(500),
			ContentType: playwright.String("application/json"),
			Body:        `{"error":"cameras are down"}`,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.AddCamera(camfarm.CameraSpec{ID: "second"}); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(conn).ToHaveText("disconnected, retrying"); err != nil {
		t.Fatalf("a failed camera refresh should flip the pill: %v", err)
	}
	if err := expect().Locator(s.page.Locator("#error")).ToHaveText(""); err != nil {
		t.Fatalf("a background failure must not touch the action error region: %v", err)
	}
	if err := s.page.Unroute(glob); err != nil {
		t.Fatal(err)
	}
	if _, err := f.AddCamera(camfarm.CameraSpec{ID: "third"}); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(conn).ToHaveText("connected"); err != nil {
		t.Fatalf("a successful refresh should restore the pill while the stream stays open: %v", err)
	}
	s.requireCleanApp(t, 500)
}

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
	s.requireCleanApp(t)
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
	if err := expect().Locator(lobby.Locator(".rm-prompt")).ToContainText("Remove lobby?"); err != nil {
		t.Fatalf("inline prompt missing: %v", err)
	}
	mustClick(t, lobby.Locator(".btn-remove-cancel"))
	if err := expect().Locator(lobby.Locator(".rm-prompt")).ToBeHidden(); err != nil {
		t.Fatalf("prompt should hide after Cancel: %v", err)
	}
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
	s.requireCleanApp(t)
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
	s.requireCleanApp(t, 409)
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
	s.requireCleanApp(t)
}

// Review Focus 1: ids that collide with Object.prototype names, or that contain
// markup, render as text and can be removed.
func TestAppCameraIDsAreTextAndPrototypeNamesWork(t *testing.T) {
	url, f := appServer(t, appOpts{})
	for _, id := range []string{"constructor", "__proto__", "toString", "<b>x<b>"} {
		if _, err := f.AddCamera(camfarm.CameraSpec{ID: id}); err != nil {
			t.Fatalf("AddCamera(%q): %v", id, err)
		}
	}
	s := newSession(t)
	s.openApp(t, url)
	for _, id := range []string{"constructor", "__proto__", "toString", "<b>x<b>"} {
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
	s.requireCleanApp(t)
}

// Review Focus 2: a failed snapshot shows its error on the card.
func TestAppFailedSnapshotShowsTheErrorOnTheCard(t *testing.T) {
	// The failing camera is first, so a rejected load that stalled the queue
	// would keep the second card from ever loading.
	url, _ := appServer(t, appOpts{cameras: []camfarm.CameraSpec{{ID: "broken"}, {ID: "healthy"}}})
	s := newSession(t)
	if err := s.page.Route("**/snapshot.jpg", func(r playwright.Route) {
		if strings.Contains(r.Request().URL(), "/cameras/broken/") {
			_ = r.Fulfill(playwright.RouteFulfillOptions{
				Status: playwright.Int(503), ContentType: playwright.String("application/json"),
				Body: `{"error":"camera is restarting"}`,
			})
			return
		}
		_ = r.Continue()
	}); err != nil {
		t.Fatal(err)
	}
	s.openApp(t, url)
	if err := expect().Locator(s.camCard("broken").Locator(".snap-ph")).ToContainText("camera is restarting"); err != nil {
		t.Fatalf("snapshot error not shown on the card: %v", err)
	}
	if _, err := s.page.WaitForFunction(`() => {
		const i = document.querySelector('.cam[data-id="healthy"] img.snap');
		return !!i && !i.hidden && i.complete && i.naturalWidth === 320 && i.naturalHeight === 240;
	}`, nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(assertTimeoutMS)}); err != nil {
		t.Fatalf("the other camera's thumbnail did not load after a rejected one: %v", err)
	}
	s.requireCleanApp(t, 503)
}

func TestAppLibrarySourceIsDisabledWithoutALibrary(t *testing.T) {
	url, _ := appServer(t, appOpts{})
	s := newSession(t)
	s.openApp(t, url)
	mustClick(t, s.page.Locator("#add-open"))
	if _, err := s.page.WaitForFunction(`() => document.querySelector('#add-source option[value=library]').disabled`,
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(assertTimeoutMS)}); err != nil {
		t.Fatalf("library option should be disabled with no usable files: %v", err)
	}
	s.requireCleanApp(t)
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
	s.requireCleanApp(t)
}

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
	s.requireCleanApp(t)
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
	s.requireCleanApp(t)
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
	s.requireCleanApp(t, 422, 400)
}

func TestAppUnimplementedFaultKindsAreDisabled(t *testing.T) {
	url, _ := appServer(t, appOpts{})
	s := newSession(t)
	s.openApp(t, url)
	mustClick(t, s.camCard("front-door").Locator(".btn-faults"))
	// The options arrive with the catalogue fetch; wait for them before counting.
	if err := expect().Locator(s.page.Locator("#fault-kind option").First()).ToBeAttached(); err != nil {
		t.Fatal(err)
	}
	n, err := s.page.Locator("#fault-kind option").Count()
	if err != nil || n == 0 {
		t.Fatalf("no kinds listed (n=%d err=%v)", n, err)
	}
	// Every option the server marks unimplemented must be disabled.
	res, err := s.page.Evaluate(`fetch("/api/v1/faults").then(r => r.json()).then(c => {
		const un = c.filter(k => !k.implemented);
		return un.length > 0 && un.every(k => {
			const o = document.querySelector('#fault-kind option[value="' + k.kind + '"]');
			return o && o.disabled;
		});
	})`)
	if err != nil || res != true {
		t.Fatalf("an unimplemented kind is selectable (res=%v err=%v)", res, err)
	}
	s.requireCleanApp(t)
}

func TestAppFaultDrawerKeepsFocusAfterApplyAndTrapsTab(t *testing.T) {
	url, _ := appServer(t, appOpts{})
	s := newSession(t)
	s.openApp(t, url)
	open := s.camCard("front-door").Locator(".btn-faults")
	for round := 0; round < 2; round++ {
		mustClick(t, open)
		if err := expect().Locator(s.page.Locator("#drawer")).ToBeVisible(); err != nil {
			t.Fatal(err)
		}
		if _, err := s.page.Locator("#fault-kind").SelectOption(playwright.SelectOptionValues{Values: &[]string{"frame_drop"}}); err != nil {
			t.Fatal(err)
		}
		mustFill(t, s.page.Locator(`.fault-param[data-name="rate"]`), "0.5")
		mustClick(t, s.page.Locator("#fault-apply"))
		if err := expect().Locator(s.camCard("front-door").Locator(".chip").First()).ToContainText("frame_drop"); err != nil {
			t.Fatal(err)
		}
		inside, err := s.page.Evaluate(`document.getElementById("drawer").contains(document.activeElement)`)
		if err != nil || inside != true {
			t.Fatalf("round %d: focus left the drawer after Apply (inside=%v err=%v)", round, inside, err)
		}
		mustClick(t, s.page.Locator("#fault-clear"))
		if err := expect().Locator(s.camCard("front-door").Locator(".chip").First()).ToContainText("no faults"); err != nil {
			t.Fatal(err)
		}
		inside, err = s.page.Evaluate(`document.getElementById("drawer").contains(document.activeElement)`)
		if err != nil || inside != true {
			t.Fatalf("round %d: focus left the drawer after Clear (inside=%v err=%v)", round, inside, err)
		}
		if round == 0 {
			// Tab from the last control wraps to the first, Shift+Tab the reverse.
			if _, err := s.page.Evaluate(`(() => { const f = [...document.querySelectorAll("#drawer button, #drawer select, #drawer input")].filter(n => !n.disabled); f[f.length-1].focus(); })()`); err != nil {
				t.Fatal(err)
			}
			if err := s.page.Keyboard().Press("Tab"); err != nil {
				t.Fatal(err)
			}
			v, err := s.page.Evaluate(`(() => { const f = [...document.querySelectorAll("#drawer button, #drawer select, #drawer input")].filter(n => !n.disabled); return document.activeElement === f[0]; })()`)
			if err != nil || v != true {
				t.Fatalf("Tab did not wrap to the first control (v=%v err=%v)", v, err)
			}
			if err := s.page.Keyboard().Press("Shift+Tab"); err != nil {
				t.Fatal(err)
			}
			v, err = s.page.Evaluate(`(() => { const f = [...document.querySelectorAll("#drawer button, #drawer select, #drawer input")].filter(n => !n.disabled); return document.activeElement === f[f.length-1]; })()`)
			if err != nil || v != true {
				t.Fatalf("Shift+Tab did not wrap to the last control (v=%v err=%v)", v, err)
			}
		}
		if err := s.page.Keyboard().Press("Escape"); err != nil {
			t.Fatal(err)
		}
		if err := expect().Locator(s.page.Locator("#drawer")).ToBeHidden(); err != nil {
			t.Fatalf("round %d: Escape did not close after Apply/Clear: %v", round, err)
		}
		back, err := s.page.Evaluate(`document.activeElement && document.activeElement.classList.contains("btn-faults")`)
		if err != nil || back != true {
			t.Fatalf("round %d: focus did not return (back=%v err=%v)", round, back, err)
		}
	}
	s.requireCleanApp(t)
}

func TestAppFaultDrawerClosesWhenItsCameraIsRemoved(t *testing.T) {
	url, f := appServer(t, appOpts{})
	s := newSession(t)
	s.openApp(t, url)
	// Let the thumbnail finish so removal does not race an in-flight snapshot.
	if err := expect().Locator(s.camCard("front-door").Locator("img.snap")).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	mustClick(t, s.camCard("front-door").Locator(".btn-faults"))
	if err := expect().Locator(s.page.Locator("#drawer")).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	if err := f.RemoveCamera("front-door"); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(s.page.Locator("#drawer")).ToBeHidden(); err != nil {
		t.Fatalf("drawer stayed open for a removed camera: %v", err)
	}
	if n, err := s.page.Locator("#drawer *").Count(); err != nil || n != 0 {
		t.Fatalf("drawer left content behind (n=%d err=%v)", n, err)
	}
	inert, err := s.page.Evaluate(`document.querySelector("main").inert`)
	if err != nil || inert != false {
		t.Fatalf("page left inert (inert=%v err=%v)", inert, err)
	}
	s.requireCleanApp(t)
}

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
	s.requireCleanApp(t)
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
	s.requireCleanApp(t)
}

func TestAppStreamQualityButtonIsAbsentWhenNotEnabled(t *testing.T) {
	url, _ := appServer(t, appOpts{quality: false})
	s := newSession(t)
	s.openApp(t, url)
	if err := expect().Locator(s.camCard("front-door")).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	// The probe settles asynchronously; give it a moment before counting.
	time.Sleep(500 * time.Millisecond)
	for _, sel := range []string{".btn-quality", ".btn-quality-cancel"} {
		n, err := s.camCard("front-door").Locator(sel).Count()
		if err != nil || n != 0 {
			t.Fatalf("%s present although the server has quality disabled (n=%d err=%v)", sel, n, err)
		}
	}
	s.requireCleanApp(t)
}

func mustSelect(t *testing.T, l playwright.Locator, v string) {
	t.Helper()
	if _, err := l.SelectOption(playwright.SelectOptionValues{Values: &[]string{v}}); err != nil {
		t.Fatal(err)
	}
}

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
	s.requireCleanApp(t)
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
	s.requireCleanApp(t)
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
	if err := expect().Locator(libRow(s, "uploads/uploaded-good.ts").Locator(".status")).ToHaveText("Valid"); err != nil {
		t.Fatalf("good upload: %v", err)
	}
	if err := s.page.Locator("#file-input").SetInputFiles([]string{bad}); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(libRow(s, "uploads/uploaded-bad.mp4").Locator(".status")).ToHaveText("Invalid"); err != nil {
		t.Fatalf("bad upload: %v", err)
	}
	s.requireCleanApp(t)
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
	s.requireCleanApp(t)
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
	s.requireCleanApp(t, 400)
}

// Review Focus 3.
func TestAppLibraryHidesDisabledControlsWithAReason(t *testing.T) {
	// The view is faked rather than built from a read-only directory, so the
	// test does not depend on the uid (root ignores directory permissions).
	url, _ := appServer(t, appOpts{})
	s := newSession(t)
	if err := s.page.Route("**/api/v1/library", func(r playwright.Route) {
		_ = r.Fulfill(playwright.RouteFulfillOptions{
			Status: playwright.Int(200), ContentType: playwright.String("application/json"),
			Body: `{"roots":[{"path":"/media","writable":false,"ui":false}],"uploadEnabled":false,"allowUIPaths":false,"maxUpload":1048576,"scanning":false,"files":[],"scanErrors":[]}`,
		})
	}); err != nil {
		t.Fatal(err)
	}
	openLibrary(t, s, url)
	if err := expect().Locator(s.page.Locator("#drop")).ToHaveCount(0); err != nil {
		t.Fatal("upload area shown although uploads are disabled")
	}
	if err := expect().Locator(s.page.Locator("#root-form")).ToHaveCount(0); err != nil {
		t.Fatal("add-path form shown although UI paths are disabled")
	}
	if err := expect().Locator(s.page.Locator("#panel-library")).ToContainText("Uploads are off"); err != nil {
		t.Fatalf("no reason for the missing upload area: %v", err)
	}
	if err := expect().Locator(s.page.Locator("#panel-library")).ToContainText("Adding paths is off"); err != nil {
		t.Fatalf("no reason for the missing path form: %v", err)
	}
	s.requireCleanApp(t)
}

func TestAppLibraryFilterChipKeepsKeyboardFocus(t *testing.T) {
	lib, _ := libraryFixture(t, library.Options{})
	if err := lib.Scan(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	url, _ := appServer(t, appOpts{lib: lib})
	s := newSession(t)
	openLibrary(t, s, url)
	chip := s.page.Locator(`.stat[data-f="invalid"]`)
	if err := expect().Locator(chip).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	if err := chip.Focus(); err != nil {
		t.Fatal(err)
	}
	if err := s.page.Keyboard().Press("Enter"); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(s.page.Locator("#files tbody tr")).ToHaveCount(1); err != nil {
		t.Fatalf("keyboard activation did not filter: %v", err)
	}
	cls, err := s.page.Evaluate(`document.activeElement.className + "|" + document.activeElement.dataset.f`)
	if err != nil || cls != "stat|invalid" {
		t.Fatalf("focus left the chip after filtering: %v err=%v", cls, err)
	}
	s.requireCleanApp(t)
}

func TestAppLibraryUploadsSeveralFilesOneAfterAnother(t *testing.T) {
	lib, err := library.New(library.Options{Roots: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	url, _ := appServer(t, appOpts{lib: lib})
	s := newSession(t)
	openLibrary(t, s, url)
	dir := t.TempDir()
	var paths []string
	for _, n := range []string{"one.ts", "two.ts", "three.ts"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, fixture.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	var gets atomic.Int32
	s.page.On("request", func(r playwright.Request) {
		if r.Method() == "GET" && strings.HasSuffix(r.URL(), "/api/v1/library") {
			gets.Add(1)
		}
	})
	if err := s.page.Locator("#file-input").SetInputFiles(paths); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"one.ts", "two.ts", "three.ts"} {
		if err := expect().Locator(libRow(s, "uploads/"+n).Locator(".status")).ToHaveText("Valid"); err != nil {
			t.Fatalf("%s not listed: %v", n, err)
		}
	}
	if n := gets.Load(); n != 0 {
		t.Fatalf("upload re-fetched the library %d times; the response already carries the report", n)
	}
	s.requireCleanApp(t)
}

func TestAppLibraryMidBatchUploadFailureIsStillReportedAtTheEnd(t *testing.T) {
	lib, err := library.New(library.Options{Roots: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	url, _ := appServer(t, appOpts{lib: lib})
	s := newSession(t)
	var posts atomic.Int32
	if err := s.page.Route("**/api/v1/library/upload", func(r playwright.Route) {
		if posts.Add(1) == 2 {
			_ = r.Fulfill(playwright.RouteFulfillOptions{
				Status: playwright.Int(400), ContentType: playwright.String("application/json"),
				Body: `{"error":"not a supported video type"}`,
			})
			return
		}
		_ = r.Continue()
	}); err != nil {
		t.Fatal(err)
	}
	openLibrary(t, s, url)
	dir := t.TempDir()
	var paths []string
	for _, n := range []string{"first.ts", "bad.ts", "last.ts"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, fixture.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	if err := s.page.Locator("#file-input").SetInputFiles(paths); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"first.ts", "last.ts"} {
		if err := expect().Locator(libRow(s, "uploads/"+n)).ToBeVisible(); err != nil {
			t.Fatalf("%s not listed: %v", n, err)
		}
	}
	if err := expect().Locator(s.page.Locator("#error")).ToContainText("bad.ts: not a supported video type"); err != nil {
		t.Fatalf("failure cleared by the later success: %v", err)
	}
	if n, _ := libRow(s, "uploads/bad.ts").Count(); n != 0 {
		t.Fatal("rejected file is listed")
	}
	s.requireCleanApp(t, 400)
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
	s.requireCleanApp(t)
}

func TestAppLibraryDisabledServerShowsNoLibraryTab(t *testing.T) {
	url, _ := appServer(t, appOpts{})
	s := newSession(t)
	openLibrary(t, s, url)
	if err := expect().Locator(s.page.Locator("#panel-library")).ToContainText("not enabled"); err != nil {
		t.Fatalf("a server without a library should say so: %v", err)
	}
	s.requireCleanApp(t)
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
	row := libRow(s, "uploads/gone.ts")
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
	s.requireCleanApp(t)
}

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
	s.requireCleanApp(t)
}

// An unavailable GPU shows its reason; a null value shows "-"; the other tiles
// keep their numbers.
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
	if err != nil || strings.TrimSpace(val) != "-" {
		t.Fatalf("a null CPU reading should render as exactly \"-\": %q err=%v", val, err)
	}
	s.requireCleanApp(t)
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
	s.requireCleanApp(t)
}

// When the stream drops the strip dims and says stale.
func TestAppMetricsStripGoesStaleWhenTheStreamDrops(t *testing.T) {
	s0, _ := sampler(t, gpuOKApp)
	f := camfarm.StartT(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "front-door"}}})
	h := Handler(f, Options{Metrics: s0})
	ts := httptest.NewServer(h)
	addr := ts.Listener.Addr().String()
	// The page still holds an open event stream, so drop it before Close waits.
	defer func() { ts.CloseClientConnections(); ts.Close() }()
	s := newSession(t)
	s.openApp(t, ts.URL)
	// Wait for a live connection first, or the "not stale" check proves nothing.
	if err := expect().Locator(s.page.Locator("#conn")).ToHaveText("connected"); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(s.page.Locator("#metrics.stale")).ToHaveCount(0); err != nil {
		t.Fatal(err)
	}
	if err := expect().Locator(s.page.Locator("#metrics .stale-note")).ToBeHidden(); err != nil {
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
	if err := expect().Locator(s.page.Locator("#metrics .stale-note")).ToBeVisible(); err != nil {
		t.Fatalf("strip does not visibly say stale: %v", err)
	}
	// Bring the server back on the same address; EventSource reconnects.
	var l net.Listener
	var err error
	for i := 0; i < 20; i++ {
		if l, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("cannot rebind %s to test recovery: %v", addr, err)
	}
	ts = httptest.NewUnstartedServer(h)
	_ = ts.Listener.Close() // the unstarted server's own throwaway listener
	ts.Listener = l
	ts.Start()
	if err := expect().Locator(s.page.Locator("#conn")).ToHaveText("connected"); err != nil {
		t.Fatalf("did not reconnect: %v", err)
	}
	if err := expect().Locator(s.page.Locator("#metrics.stale")).ToHaveCount(0); err != nil {
		t.Fatalf("strip still stale after recovery: %v", err)
	}
	if err := expect().Locator(s.page.Locator("#metrics .stale-note")).ToBeHidden(); err != nil {
		t.Fatalf("stale text still visible after recovery: %v", err)
	}
	// No requireCleanApp: the dropped stream legitimately logs console errors.
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
	s.requireCleanApp(t)
}
