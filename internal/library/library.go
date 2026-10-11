// Package library is the set of video files the farm can serve: directories
// ("roots") that are scanned, an optional upload target, and a validity report
// per file. Validation runs once per file, on scan or upload, never continuously.
package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0x524a/camfarm/internal/media"
)

// Errors the dashboard maps to HTTP statuses.
var (
	ErrReadOnly        = errors.New("library: no writable media directory is configured")
	ErrTooLarge        = errors.New("library: upload exceeds the size limit")
	ErrNotDeletable    = errors.New("library: only files in the upload directory can be deleted")
	ErrNotFound        = errors.New("library: unknown file")
	ErrBadName         = errors.New("library: invalid file name")
	ErrUIPathsDisabled = errors.New("library: adding directories from the UI is disabled (start with --allow-ui-paths)")
	ErrNotDirectory    = errors.New("library: not a directory")
)

const (
	defaultMaxUpload = 2 << 30
	defaultWorkers   = 2
	defaultTimeout   = 30 * time.Second
	// maxDepth is how many path components below a root a file may sit.
	maxDepth = 3
)

// videoExt lists the extensions that are scanned. The container is still
// decided from content, so a listed extension on an unsupported container is
// reported as invalid, not skipped.
var videoExt = map[string]bool{
	".ts": true, ".mp4": true, ".m4v": true, ".mov": true, ".mkv": true,
	".avi": true, ".h264": true, ".h265": true, ".mpg": true,
}

// Options configures a Library.
type Options struct {
	Roots        []string
	AllowUIPaths bool
	MaxUpload    int64
	Workers      int
	Timeout      time.Duration
	// Analyze validates one file; nil means media.AnalyzeFile. Tests only.
	Analyze func(path string) media.Report
}

// RootInfo describes one configured directory.
type RootInfo struct {
	Path     string `json:"path"`
	Writable bool   `json:"writable"`
	UI       bool   `json:"ui"`
}

// Entry is one file and its report.
type Entry struct {
	ID       string       `json:"id"`
	Root     string       `json:"root"`
	Path     string       `json:"path"`
	Name     string       `json:"name"`
	Size     int64        `json:"size"`
	Modified time.Time    `json:"modified"`
	Report   media.Report `json:"report"`
}

// Library holds the roots and the cached reports.
type Library struct {
	opt Options
	sem chan struct{}

	scanning atomic.Int32

	mu      sync.Mutex
	roots   []RootInfo
	entries map[string]Entry
}

// New builds a Library over the startup roots. A root that does not exist or
// is not a directory is an error. Leftover upload part files are removed.
func New(opt Options) (*Library, error) {
	if opt.MaxUpload <= 0 {
		opt.MaxUpload = defaultMaxUpload
	}
	if opt.Workers <= 0 {
		opt.Workers = defaultWorkers
	}
	if opt.Timeout <= 0 {
		opt.Timeout = defaultTimeout
	}
	l := &Library{opt: opt, sem: make(chan struct{}, opt.Workers), entries: map[string]Entry{}}
	for _, p := range opt.Roots {
		if err := l.addRoot(p, false); err != nil {
			return nil, fmt.Errorf("media directory %q: %w", p, err)
		}
	}
	if up, ok := l.uploadRoot(); ok {
		cleanPartFiles(up)
	}
	return l, nil
}

func (l *Library) addRoot(p string, ui bool) error {
	abs, err := filepath.Abs(p)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return err
	}
	st, err := os.Stat(real)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return ErrNotDirectory
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.roots {
		if r.Path == real {
			return nil
		}
	}
	l.roots = append(l.roots, RootInfo{Path: real, Writable: !ui && probeWritable(real), UI: ui})
	return nil
}

func probeWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".camfarm-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// AddRoot adds a directory chosen in the UI. It never becomes an upload target.
func (l *Library) AddRoot(path string) error {
	if !l.opt.AllowUIPaths {
		return ErrUIPathsDisabled
	}
	return l.addRoot(path, true)
}

// Roots returns the configured directories in the order they were added.
func (l *Library) Roots() []RootInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.roots)
}

// uploadRoot is the first writable startup root.
func (l *Library) uploadRoot() (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.roots {
		if r.Writable {
			return r.Path, true
		}
	}
	return "", false
}

// UploadEnabled reports whether Upload has somewhere to write.
func (l *Library) UploadEnabled() bool { _, ok := l.uploadRoot(); return ok }

// MaxUpload is the largest accepted upload in bytes.
func (l *Library) MaxUpload() int64 { return l.opt.MaxUpload }

// Scanning reports whether a scan is running.
func (l *Library) Scanning() bool { return l.scanning.Load() > 0 }

// List returns every entry, sorted by name then path.
func (l *Library) List() []Entry {
	l.mu.Lock()
	out := make([]Entry, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, e)
	}
	l.mu.Unlock()
	slices.SortFunc(out, func(a, b Entry) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Path, b.Path)
	})
	return out
}

// Resolve finds an entry by id.
func (l *Library) Resolve(id string) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[id]
	return e, ok
}

// ByPath finds an entry by absolute path.
func (l *Library) ByPath(path string) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if e.Path == path {
			return e, true
		}
	}
	return Entry{}, false
}

// idFor derives a stable id from the root and the path relative to it, so the
// same file seen through two overlapping roots gets two distinct entries.
func idFor(root, rel string) string {
	h := sha256.Sum256([]byte(root + "\x00" + rel))
	return hex.EncodeToString(h[:8])
}

func (l *Library) store(e Entry) {
	l.mu.Lock()
	l.entries[e.ID] = e
	l.mu.Unlock()
}

// analyze validates one file with a time limit. ok is false only when ctx ended
// first; a timeout is an ordinary invalid report. The abandoned goroutine
// finishes on its own: AnalyzeFile cannot be interrupted.
//
// Trade-off: the caller's semaphore token is released once analyze gives up on
// a hung file, so hung files can briefly exceed Workers, and Refresh and Upload
// call analyze without taking the semaphore at all.
func (l *Library) analyze(ctx context.Context, path string) (media.Report, bool) {
	fn := l.opt.Analyze
	if fn == nil {
		fn = media.AnalyzeFile
	}
	done := make(chan media.Report, 1)
	go func() { done <- fn(path) }()
	timer := time.NewTimer(l.opt.Timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r, true
	case <-timer.C:
		r := media.Report{Status: media.StatusInvalid, Issues: []media.Issue{{
			Code: "loader_error", Severity: media.SeverityInvalid,
			Message: "Checking the file took too long and was stopped.",
			Detail:  "limit " + l.opt.Timeout.String(),
		}}}
		return r, true
	case <-ctx.Done():
		return media.Report{}, false
	}
}

// Scan walks one root (or all, when root is empty) and validates new or changed
// files. Unchanged files, judged by size and mtime, keep their cached report
// unless force is set. Files that disappeared are dropped.
func (l *Library) Scan(ctx context.Context, root string, force bool) error {
	roots := l.Roots()
	if root != "" {
		i := slices.IndexFunc(roots, func(r RootInfo) bool { return r.Path == root })
		if i < 0 {
			return fmt.Errorf("%w: %q is not a configured directory", ErrNotFound, root)
		}
		roots = roots[i : i+1]
	}
	l.scanning.Add(1)
	defer l.scanning.Add(-1)
	var errs []error
	for _, r := range roots {
		if err := l.scanRoot(ctx, r.Path, force); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type candidate struct {
	path, name string
	size       int64
	mod        time.Time
}

func (l *Library) scanRoot(ctx context.Context, root string, force bool) error {
	var cands []candidate
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil // an unreadable subdirectory is skipped, not fatal
		}
		if p == root {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // never follow links: they could point outside the root
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			if strings.Count(rel, string(filepath.Separator))+1 >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !videoExt[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		cands = append(cands, candidate{p, rel, info.Size(), info.ModTime()})
		return nil
	})
	if err != nil {
		return fmt.Errorf("library: scanning %q: %w", root, err)
	}

	seen := make(map[string]bool, len(cands))
	var wg sync.WaitGroup
	for _, c := range cands {
		id := idFor(root, c.name)
		seen[id] = true
		if old, ok := l.Resolve(id); ok && !force && old.Size == c.size && old.Modified.Equal(c.mod) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case l.sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-l.sem }()
			rep, ok := l.analyze(ctx, c.path)
			if !ok {
				return
			}
			l.store(Entry{ID: id, Root: root, Path: c.path, Name: c.name, Size: c.size, Modified: c.mod, Report: rep})
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}

	// Prune entries the walk did not see, but only those whose file is really
	// gone: an entry stored after the walk (concurrent scan, upload, refresh)
	// must survive.
	l.mu.Lock()
	for id, e := range l.entries {
		if e.Root != root || seen[id] {
			continue
		}
		if st, err := os.Lstat(e.Path); err != nil || !st.Mode().IsRegular() {
			delete(l.entries, id)
		}
	}
	l.mu.Unlock()
	return nil
}

// Refresh re-checks one file just before it is used. If the file changed since
// the scan it is re-validated; if it is gone it is dropped and ErrNotFound is
// returned. This keeps a stale "valid" from becoming a camera.
func (l *Library) Refresh(ctx context.Context, id string) (Entry, error) {
	e, ok := l.Resolve(id)
	if !ok {
		return Entry{}, ErrNotFound
	}
	// Lstat plus an exact real-path check: a file swapped for a symlink (or a
	// parent directory swapped for one) must not be served from outside the root.
	st, err := os.Lstat(e.Path)
	if err == nil && st.Mode().IsRegular() {
		if real, rerr := filepath.EvalSymlinks(e.Path); rerr != nil || real != e.Path {
			err = errors.New("path no longer resolves to itself")
		}
	}
	if err != nil || !st.Mode().IsRegular() {
		l.mu.Lock()
		delete(l.entries, id)
		l.mu.Unlock()
		return Entry{}, fmt.Errorf("%w: %q is gone", ErrNotFound, e.Name)
	}
	if st.Size() == e.Size && st.ModTime().Equal(e.Modified) {
		return e, nil
	}
	rep, ok := l.analyze(ctx, e.Path)
	if !ok {
		return Entry{}, ctx.Err()
	}
	e.Size, e.Modified, e.Report = st.Size(), st.ModTime(), rep
	l.store(e)
	return e, nil
}

// cleanPartFiles removes upload temp files left by a crash.
func cleanPartFiles(dir string) {
	parts, _ := filepath.Glob(filepath.Join(dir, ".upload-*.part"))
	for _, p := range parts {
		_ = os.Remove(p)
	}
}
