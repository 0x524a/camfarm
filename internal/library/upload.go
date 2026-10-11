package library

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const maxNameLen = 200

// cleanName reduces an uploaded file name to a safe base name.
func cleanName(name string) (string, error) {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	switch {
	case name == "" || name == "." || name == "..":
		return "", fmt.Errorf("%w: the name is empty", ErrBadName)
	case strings.HasPrefix(name, "."):
		return "", fmt.Errorf("%w: names starting with a dot are hidden from the library", ErrBadName)
	case len(name) > maxNameLen:
		return "", fmt.Errorf("%w: longer than %d bytes", ErrBadName, maxNameLen)
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return "", fmt.Errorf("%w: contains a control character", ErrBadName)
	case !videoExt[strings.ToLower(filepath.Ext(name))]:
		return "", fmt.Errorf("%w: the extension must be a video type such as .ts or .mp4", ErrBadName)
	}
	return name, nil
}

// linkUnique gives the part file its final name without overwriting anything.
// os.Link fails if the target exists, so two uploads of one name cannot clobber
// each other: the loser takes the next numeric suffix.
func linkUnique(part, dir, name string) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 10000; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		final := filepath.Join(dir, candidate)
		err := os.Link(part, final)
		if err == nil {
			return final, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("library: no free name for %q", name)
}

// Upload stores r as a new library file and validates it. An invalid file is
// kept, so the report explaining why stays visible; the caller may delete it.
func (l *Library) Upload(ctx context.Context, name string, r io.Reader) (Entry, error) {
	dir, ok := l.uploadRoot()
	if !ok {
		return Entry{}, ErrReadOnly
	}
	base, err := cleanName(name)
	if err != nil {
		return Entry{}, err
	}

	part, err := os.CreateTemp(dir, ".upload-*.part")
	if err != nil {
		return Entry{}, fmt.Errorf("library: creating upload file: %w", err)
	}
	partPath := part.Name()
	defer func() { _ = os.Remove(partPath) }() // after the link, this removes only the temp name

	n, err := io.Copy(part, io.LimitReader(r, l.opt.MaxUpload+1))
	if cerr := part.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Entry{}, fmt.Errorf("library: receiving upload: %w", err)
	}
	if n > l.opt.MaxUpload {
		return Entry{}, fmt.Errorf("%w (%d bytes)", ErrTooLarge, l.opt.MaxUpload)
	}

	final, err := linkUnique(partPath, dir, base)
	if err != nil {
		return Entry{}, fmt.Errorf("library: placing upload: %w", err)
	}
	st, err := os.Stat(final)
	if err != nil {
		return Entry{}, err
	}
	rep, ok := l.analyze(ctx, final)
	if !ok {
		_ = os.Remove(final)
		return Entry{}, ctx.Err()
	}
	name = filepath.Base(final)
	e := Entry{
		ID: idFor(dir, name), Root: dir, Path: final, Name: name,
		Size: st.Size(), Modified: st.ModTime(), Report: rep,
	}
	l.store(e)
	return e, nil
}

// Delete removes a file from disk and from the library. Only files in the
// upload root can be deleted. The caller refuses first if a camera uses it.
func (l *Library) Delete(id string) error {
	e, ok := l.Resolve(id)
	if !ok {
		return ErrNotFound
	}
	up, ok := l.uploadRoot()
	if !ok || e.Root != up {
		return ErrNotDeletable
	}
	if err := os.Remove(e.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("library: deleting %q: %w", e.Name, err)
	}
	l.mu.Lock()
	delete(l.entries, id)
	l.mu.Unlock()
	return nil
}
