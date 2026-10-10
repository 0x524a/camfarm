package camfarm

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/0x524a/camfarm/internal/fault"
)

const (
	faultTableStart = "<!-- fault-table:start -->"
	faultTableEnd   = "<!-- fault-table:end -->"
)

// faultTable renders the catalogue as the README table. The README must
// contain it verbatim between the markers, so the documentation cannot drift
// from what the code reports.
func faultTable() string {
	var b strings.Builder
	b.WriteString("| Kind | Implemented | Models |\n|---|---|---|\n")
	for _, ki := range fault.Catalogue() {
		impl := "no"
		if ki.Implemented {
			impl = "yes"
		}
		_, _ = fmt.Fprintf(&b, "| `%s` | %s | %s |\n", ki.Kind, impl, ki.Models)
	}
	return b.String()
}

func TestREADMEFaultTableMatchesTheCatalogue(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, m := range []string{faultTableStart, faultTableEnd} {
		if n := strings.Count(s, m); n != 1 {
			t.Fatalf("README.md contains %q %d times, want exactly once", m, n)
		}
	}
	i, j := strings.Index(s, faultTableStart), strings.Index(s, faultTableEnd)
	if i < 0 || j < i {
		t.Fatalf("README.md lacks the %s ... %s markers", faultTableStart, faultTableEnd)
	}
	got := strings.TrimSpace(s[i+len(faultTableStart) : j])
	want := strings.TrimSpace(faultTable())
	if got != want {
		t.Fatalf("README fault table is out of date. Replace the text between the markers with:\n\n%s\n", want)
	}
}
