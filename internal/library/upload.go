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
	"syscall"
	"unicode"
	"unicode/utf8"
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
	case !utf8.ValidString(name):
		return "", fmt.Errorf("%w: not valid UTF-8", ErrBadName)
	case strings.IndexFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) >= 0:
		return "", fmt.Errorf("%w: contains a control or formatting character", ErrBadName)
	case !videoExt[strings.ToLower(filepath.Ext(name))]:
		return "", fmt.Errorf("%w: the extension must be a video type such as .ts or .mp4", ErrBadName)
	}
	return name, nil
}

// linkFile is os.Link; tests swap it to exercise the no-hard-link fallback.
var linkFile = os.Link

// noLinks reports whether err means the filesystem cannot hard-link at all
// (FAT, exFAT, many network and FUSE mounts), as opposed to the name being taken.
func noLinks(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EXDEV)
}

// place gives the part file its final name without overwriting anything. A hard
// link fails if the target exists (a symlink included, even a dangling one), so
// two uploads of one name cannot clobber each other: the loser takes the next
// numeric suffix. Where hard links are unsupported, the name is reserved with an
// exclusive create and the part file is renamed over that placeholder.
func place(part, dir, name string) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 10000; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		final := filepath.Join(dir, candidate)
		err := linkFile(part, final)
		if err == nil {
			return final, nil
		}
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if !noLinks(err) {
			return "", err
		}
		f, oerr := os.OpenFile(final, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if errors.Is(oerr, fs.ErrExist) {
			continue
		}
		if oerr != nil {
			return "", oerr
		}
		_ = f.Close()
		if rerr := os.Rename(part, final); rerr != nil {
			_ = os.Remove(final)
			return "", rerr
		}
		return final, nil
	}
	return "", fmt.Errorf("library: no free name for %q", name)
}

// Upload stores r as a new library file and validates it. An invalid file is
// kept, so the report explaining why stays visible; the caller may delete it.
// The part file is validated before it is placed, so the final name never
// exists while it is being checked.
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
	defer func() { _ = os.Remove(partPath) }() // after placement, this removes only the temp name

	n, err := io.Copy(part, io.LimitReader(r, l.opt.MaxUpload+1))
	if err == nil {
		err = part.Sync()
	}
	if cerr := part.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Entry{}, fmt.Errorf("library: receiving upload: %w", err)
	}
	if n > l.opt.MaxUpload {
		return Entry{}, fmt.Errorf("%w (%d bytes)", ErrTooLarge, l.opt.MaxUpload)
	}

	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	rep, ok := l.analyze(ctx, partPath)
	if !ok {
		return Entry{}, ctx.Err()
	}
	final, err := place(partPath, dir, base)
	if err != nil {
		return Entry{}, fmt.Errorf("library: placing upload: %w", err)
	}
	st, err := os.Lstat(final)
	if err == nil && !st.Mode().IsRegular() {
		err = errors.New("not a regular file")
	}
	if err != nil {
		_ = os.Remove(final)
		return Entry{}, fmt.Errorf("library: placed upload %q is unusable: %w", base, err)
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
// The removal goes through an os.Root, so a directory swapped for a symlink
// since the scan cannot lead it outside the upload root.
func (l *Library) Delete(id string) error {
	e, ok := l.Resolve(id)
	if !ok {
		return ErrNotFound
	}
	up, ok := l.uploadRoot()
	if !ok || e.Root != up {
		return ErrNotDeletable
	}
	rel, err := filepath.Rel(up, e.Path)
	if err != nil || !filepath.IsLocal(rel) {
		return ErrNotDeletable
	}
	root, err := os.OpenRoot(up)
	if err != nil {
		return fmt.Errorf("library: deleting %q: %w", e.Name, err)
	}
	defer root.Close()
	if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("library: deleting %q: %w", e.Name, err)
	}
	l.mu.Lock()
	delete(l.entries, id)
	l.mu.Unlock()
	return nil
}
