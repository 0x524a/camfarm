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
	"sync/atomic"
	"testing"
	"time"

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
