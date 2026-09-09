package media

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSniffMPEGTS(t *testing.T) {
	got, err := sniff(fixtureBytesForTest(t))
	if err != nil {
		t.Fatalf("sniff: %v", err)
	}
	if got != containerMPEGTS {
		t.Errorf("sniff = %q, want %q", got, containerMPEGTS)
	}
}

func TestSniffISOBMFF(t *testing.T) {
	path := remuxFixture(t, fixtureBytesForTest(t), "out.mp4")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got, err := sniff(b)
	if err != nil {
		t.Fatalf("sniff: %v", err)
	}
	if got != containerISOBMFF {
		t.Errorf("sniff = %q, want %q", got, containerISOBMFF)
	}
}

// TestSniffRejectsSingleStraySyncByte proves the stride check earns its keep: a
// buffer starting with 0x47 but without sync bytes at the packet stride is not
// MPEG-TS, and claiming it would send a non-TS file to the wrong parser.
func TestSniffRejectsSingleStraySyncByte(t *testing.T) {
	header := make([]byte, sniffLen)
	header[0] = 0x47 // and nothing at 188 or 376
	if got, err := sniff(header); err == nil {
		t.Fatalf("sniff = %q, want an error for a stray sync byte", got)
	}
}

func TestSniffUnrecognisedNamesTheBytesAndSupportedSet(t *testing.T) {
	_, err := sniff([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x00, 0x00, 0x00, 0x00}) // EBML/Matroska
	if err == nil {
		t.Fatal("expected an error for an unsupported container")
	}
	// The message must let a user who brought a Matroska file understand why it
	// was refused, rather than seeing a parse failure from the wrong parser.
	for _, want := range []string{"1a45dfa3", string(containerMPEGTS), string(containerISOBMFF)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err.Error(), want)
		}
	}
}

func TestSniffEmptyInput(t *testing.T) {
	if _, err := sniff(nil); err == nil {
		t.Fatal("expected an error for empty input")
	}
}

// TestParseAnyIgnoresFilenameExtension is the point of content sniffing: a file
// named .mp4 that actually holds MPEG-TS bytes must load, because an extension
// can lie and refusing it would be refusing a valid source.
func TestParseAnyIgnoresFilenameExtension(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lying-name.mp4")
	if err := os.WriteFile(path, fixtureBytesForTest(t), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	m, err := (&FileSource{Path: path}).Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Codec != CodecH264 || len(m.AUs) != fixtureAUs {
		t.Errorf("codec = %q with %d AUs, want %q with %d", m.Codec, len(m.AUs), CodecH264, fixtureAUs)
	}
}

func TestParseAnyRejectsUnrecognisedContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.ts")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xAB}, 1024), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := (&FileSource{Path: path}).Load(); err == nil {
		t.Fatal("expected an error for unrecognised content")
	}
}

func TestFileSourceLoadsMP4(t *testing.T) {
	path := remuxFixture(t, fixtureBytesForTest(t), "out.mp4")
	m, err := (&FileSource{Path: path}).Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(m.AUs) != fixtureAUs {
		t.Errorf("access units = %d, want %d", len(m.AUs), fixtureAUs)
	}
}
