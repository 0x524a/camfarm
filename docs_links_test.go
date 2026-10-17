package camfarm

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var mdLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// brokenLinks lists the relative links and images in a markdown file (given by
// its path and contents) that point at a file which does not exist. Absolute
// URLs and bare #anchors are not checked; a #fragment on a file link is
// stripped before the file is looked up.
func brokenLinks(path, content string) []string {
	var broken []string
	for _, m := range mdLink.FindAllStringSubmatch(content, -1) {
		target := m[1]
		if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
			continue
		}
		if i := strings.Index(target, "#"); i >= 0 {
			target = target[:i]
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), target)); err != nil {
			broken = append(broken, m[1])
		}
	}
	return broken
}

func TestDocumentationLinksAndImagesExist(t *testing.T) {
	files := []string{"README.md"}
	more, err := filepath.Glob(filepath.Join("docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, more...)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range brokenLinks(f, string(raw)) {
			t.Errorf("%s links to %q, which does not exist", f, b)
		}
	}
}

// The link check must actually fail on a broken link, or the test above is vacuous.
func TestBrokenLinkCheckRejectsAMissingFile(t *testing.T) {
	got := brokenLinks("README.md", "[ok](README.md) ![img](docs/assets/nope.png) [web](https://example.com/x) [a](#top) [f](docs/faults.md#x)")
	if len(got) != 1 || got[0] != "docs/assets/nope.png" {
		t.Fatalf("broken = %v, want only docs/assets/nope.png", got)
	}
}
