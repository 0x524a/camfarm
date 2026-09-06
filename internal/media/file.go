package media

import (
	"bytes"
	"fmt"
	"os"

	"github.com/0x524a/camfarm/internal/media/fixture"
)

// FileSource reads media from an MPEG-TS file on disk.
type FileSource struct {
	Path string
}

// Load parses the file.
func (s *FileSource) Load() (*Media, error) {
	f, err := os.Open(s.Path)
	if err != nil {
		return nil, fmt.Errorf("media: opening source: %w", err)
	}
	defer f.Close()
	return parseMPEGTS(f)
}

// Deterministic reports true: a file replays identically.
func (s *FileSource) Deterministic() bool { return true }

// Describe identifies the source.
func (s *FileSource) Describe() string { return "file:" + s.Path }

// FixtureSource reads the media bundled with camfarm.
type FixtureSource struct{}

// Load parses the bundled fixture.
func (FixtureSource) Load() (*Media, error) { return parseMPEGTS(bytes.NewReader(fixture.Bytes())) }

// Deterministic reports true.
func (FixtureSource) Deterministic() bool { return true }

// Describe identifies the source.
func (FixtureSource) Describe() string { return "fixture:bundled" }

// fixtureData exposes the embedded bytes to this package's tests.
func fixtureData() []byte { return fixture.Bytes() }

// FixtureH265Source reads the bundled H.265 media.
type FixtureH265Source struct{}

// Load parses the bundled H.265 fixture.
func (FixtureH265Source) Load() (*Media, error) {
	return parseMPEGTS(bytes.NewReader(fixture.BytesH265()))
}

// Deterministic reports true.
func (FixtureH265Source) Deterministic() bool { return true }

// Describe identifies the source.
func (FixtureH265Source) Describe() string { return "fixture:bundled-h265" }

// fixtureH265Data exposes the embedded H.265 bytes to this package's tests.
func fixtureH265Data() []byte { return fixture.BytesH265() }
