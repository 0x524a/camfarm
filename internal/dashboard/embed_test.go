package dashboard

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

var (
	htmlRef   = regexp.MustCompile(`(?:src|href)="(\./)?([^"#:]+\.(?:js|css))"`)
	importRef = regexp.MustCompile(`from\s+"\./([^"]+\.js)"`)
)

// Every script, stylesheet and module the page loads must be in the embedded
// filesystem, so a missing file fails the build and not the browser.
func TestEmbeddedPageReferencesExist(t *testing.T) {
	web, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []string{"app.html", "index.html"} {
		b, err := fs.ReadFile(web, page)
		if err != nil {
			continue // app.html is renamed to index.html in the last task
		}
		for _, m := range htmlRef.FindAllStringSubmatch(string(b), -1) {
			if _, err := fs.Stat(web, m[2]); err != nil {
				t.Errorf("%s references %q, which is not embedded", page, m[2])
			}
		}
	}
	err = fs.WalkDir(web, "js", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
			return err
		}
		b, rerr := fs.ReadFile(web, p)
		if rerr != nil {
			return rerr
		}
		for _, m := range importRef.FindAllStringSubmatch(string(b), -1) {
			if _, serr := fs.Stat(web, "js/"+m[1]); serr != nil {
				t.Errorf("%s imports %q, which is not embedded", p, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
