//go:build e2e

// Real-browser tests of the dashboard. They sit behind the e2e build tag so the
// default `go test ./...` needs no browser. A missing browser fails the tests;
// it never skips them.
//
// Install the browser once with:
//
//	go run github.com/playwright-community/playwright-go/cmd/playwright install --with-deps chromium
//
// (That command downloads the driver from a retired CDN and currently fails;
// the e2e CI job assembles the driver from npm instead, see ci.yml.)
//
// CAMFARM_E2E_CHROMIUM may name a Chromium/Chrome executable to launch instead
// of the one Playwright downloaded.
package dashboard

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/playwright-community/playwright-go"
)

const assertTimeoutMS = 15000

var (
	envOnce sync.Once
	pw      *playwright.Playwright
	browser playwright.Browser
	envErr  error
)

// launch starts Chromium once per test binary. The driver is run with
// SkipInstallBrowsers: that does not skip the driver download, it only stops
// Run from also downloading browsers, so a missing browser fails at Launch
// (never skips) instead of triggering a silent multi-hundred-megabyte download.
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
