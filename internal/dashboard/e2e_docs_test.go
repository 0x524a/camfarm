//go:build e2e

package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/playwright-community/playwright-go"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/hostmetrics"
	"github.com/0x524a/camfarm/internal/library"
	"github.com/0x524a/camfarm/internal/media/fixture"
)

// TestDocsScreenshots renders the dashboard against a seeded fleet and writes
// the README's screenshots. It does nothing unless CAMFARM_DOCS_SHOTS names an
// output directory:
//
//	CAMFARM_DOCS_SHOTS=$PWD/docs/assets go test -tags e2e -count=1 -run TestDocsScreenshots ./internal/dashboard/
//
// Use an absolute path: go test runs in the package directory.
//
// CI runs it too, so a page that no longer renders these views fails the build.
// Nothing here is stubbed: the host figures come from the machine running the
// test (the GPU tile shows why it is unavailable when there is no nvidia-smi),
// the library verdicts come from the real validator, and the stream quality
// result comes from a real run against a camera.
func TestDocsScreenshots(t *testing.T) {
	out := os.Getenv("CAMFARM_DOCS_SHOTS")
	if out == "" {
		t.Skip("set CAMFARM_DOCS_SHOTS to a directory to write the README screenshots")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "clips")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{
		"front-door-loop.ts": fixture.Bytes(),
		"lobby-loop.ts":      fixture.Bytes(),
		"truncated.mp4":      []byte("this is not a video"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib, err := library.New(library.Options{Roots: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if err := lib.Scan(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	m := hostmetrics.New(hostmetrics.Options{})
	t.Cleanup(m.Close)

	url, f := appServer(t, appOpts{
		cameras: []camfarm.CameraSpec{{ID: "front-door"}, {ID: "lobby"}, {ID: "loading-dock"}},
		lib:     lib, quality: true, metrics: m,
	})
	dock, _ := f.Camera("loading-dock")
	if err := dock.Inject(camfarm.FaultSpec{Kind: "frame_drop", Rate: 0.2}); err != nil {
		t.Fatal(err)
	}

	shoot := func(name string, s *session, full bool) {
		t.Helper()
		path := filepath.Join(out, name)
		if _, err := s.page.Screenshot(playwright.PageScreenshotOptions{Path: playwright.String(path), FullPage: playwright.Bool(full)}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if st, err := os.Stat(path); err != nil || st.Size() < 20_000 {
			t.Fatalf("%s is missing or blank (%v)", name, err)
		}
	}

	light := newSession(t, playwright.BrowserNewContextOptions{
		ColorScheme: playwright.ColorSchemeLight,
		Viewport:    &playwright.Size{Width: 1280, Height: 860},
	})
	light.openApp(t, url)
	for _, id := range []string{"front-door", "lobby", "loading-dock"} {
		// A thumbnail is a captured frame: wait for each camera's image to load.
		if err := expect().Locator(light.camCard(id).Locator("img")).ToBeVisible(); err != nil {
			t.Fatalf("no thumbnail for %s: %v", id, err)
		}
	}
	card := light.camCard("front-door")
	mustClick(t, card.Locator(".btn-quality"))
	res := card.Locator(".qres.done")
	if err := expect().Locator(res).ToBeVisible(playwright.LocatorAssertionsToBeVisibleOptions{Timeout: playwright.Float(30000)}); err != nil {
		t.Fatalf("quality result never appeared: %v", err)
	}
	cpu := light.page.Locator(`#metrics .tile[data-tile="cpu"] .tile-value`)
	if err := expect().Locator(cpu).Not().ToHaveText("-", playwright.LocatorAssertionsToHaveTextOptions{Timeout: playwright.Float(20000)}); err != nil {
		t.Fatalf("CPU tile never filled: %v", err)
	}
	shoot("dashboard-cameras.png", light, false)

	mustClick(t, light.camCard("loading-dock").Locator(".btn-faults"))
	if err := expect().Locator(light.page.Locator("#drawer")).ToBeVisible(); err != nil {
		t.Fatal(err)
	}
	mustSelect(t, light.page.Locator("#fault-kind"), "packet_loss")
	shoot("dashboard-fault-drawer.png", light, false)
	light.requireCleanApp(t)

	dark := newSession(t, playwright.BrowserNewContextOptions{
		ColorScheme: playwright.ColorSchemeDark,
		Viewport:    &playwright.Size{Width: 1280, Height: 700},
	})
	openLibrary(t, dark, url)
	for name, status := range map[string]string{"front-door-loop.ts": "Valid", "truncated.mp4": "Invalid"} {
		if err := expect().Locator(libRow(dark, name).Locator(".status")).ToHaveText(regexp.MustCompile("^" + status)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	shoot("dashboard-library.png", dark, false)
	dark.requireCleanApp(t)
}
