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

// checkFaultTable reports why readme does not carry the current fault table
// between the markers, or nil if it does.
func checkFaultTable(readme string) error {
	for _, m := range []string{faultTableStart, faultTableEnd} {
		if n := strings.Count(readme, m); n != 1 {
			return fmt.Errorf("README.md contains %q %d times, want exactly once", m, n)
		}
	}
	i, j := strings.Index(readme, faultTableStart), strings.Index(readme, faultTableEnd)
	if j < i {
		return fmt.Errorf("README.md lacks the %s ... %s markers in order", faultTableStart, faultTableEnd)
	}
	got := strings.TrimSpace(readme[i+len(faultTableStart) : j])
	want := strings.TrimSpace(faultTable())
	if got != want {
		return fmt.Errorf("README fault table is out of date. Replace the text between the markers with:\n\n%s\n", want)
	}
	return nil
}

func TestREADMEFaultTableMatchesTheCatalogue(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkFaultTable(string(raw)); err != nil {
		t.Fatal(err)
	}
}

// The drift check must actually fail on drift: a check that passes everything
// would make the test above vacuous.
func TestFaultTableCheckRejectsDrift(t *testing.T) {
	good := faultTableStart + "\n" + faultTable() + faultTableEnd
	if err := checkFaultTable(good); err != nil {
		t.Fatalf("a correct table was rejected: %v", err)
	}
	first := fault.Catalogue()[0]
	for name, mutated := range map[string]string{
		"changed models text": strings.Replace(good, first.Models, first.Models+" (edited)", 1),
		"row removed":         strings.Replace(good, "| `"+string(first.Kind)+"` |", "| `x` |", 1),
		"no markers":          faultTable(),
		"duplicated start":    faultTableStart + good,
		"markers swapped":     faultTableEnd + "\n" + faultTable() + faultTableStart,
	} {
		if err := checkFaultTable(mutated); err == nil {
			t.Errorf("%s: drift was not detected", name)
		}
	}
}
