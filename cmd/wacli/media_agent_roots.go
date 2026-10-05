package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/openclaw/wacli/internal/app"
)

type agentMediaLocation struct {
	app.MediaLocation
	opened []*os.Root
}

func (l *agentMediaLocation) close() {
	for _, root := range l.opened {
		_ = root.Close()
	}
}

// Roots for agent IO are separate from legacy upload policy. Symlinked policy
// roots are resolved once; every component beneath that boundary rejects links.
// Archive cache paths must also be beneath this store's media directory.
func openAgentMediaLocation(path, storeDir string, roots []string, cache, create bool) (*agentMediaLocation, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	storeReal, err := filepath.EvalSymlinks(storeDir)
	if err != nil {
		return nil, err
	}
	boundary := filepath.VolumeName(abs) + string(filepath.Separator)
	rel, err := filepath.Rel(boundary, abs)
	if err != nil {
		return nil, err
	}
	if cache {
		cacheDir := filepath.Join(storeDir, "media")
		base := storeDir
		if !pathWithin(cacheDir, abs) && pathWithin(filepath.Join(storeReal, "media"), abs) {
			base = storeReal
		}
		if !pathWithin(filepath.Join(base, "media"), abs) {
			return nil, mediaAgentPathError()
		}
		boundary = storeReal
		rel, err = filepath.Rel(base, abs)
		if err != nil {
			return nil, err
		}
	}
	canonical := filepath.Join(boundary, rel)
	if len(roots) > 0 {
		matched := false
		for _, policyRoot := range roots {
			real, err := filepath.EvalSymlinks(policyRoot)
			if err != nil {
				continue
			}
			candidate := canonical
			// Explicit output may use the policy root's configured symlink alias.
			if !cache && pathWithin(policyRoot, abs) {
				r, err := filepath.Rel(policyRoot, abs)
				if err != nil {
					return nil, err
				}
				candidate = filepath.Join(real, r)
			}
			if pathWithin(real, candidate) {
				matched = true
				if !cache {
					boundary = real
					rel, err = filepath.Rel(real, candidate)
					canonical = candidate
				}
				break
			}
		}
		if !matched || err != nil {
			return nil, mediaAgentPathError()
		}
	}
	// Output can only enter the selected store under media, never DB/session,
	// LOCK/socket/heartbeat or other control paths, even if policy roots allow it.
	if !cache && (canonical == storeReal || pathWithin(storeReal, canonical)) && !pathWithin(filepath.Join(storeReal, "media"), canonical) {
		return nil, mediaAgentPathError()
	}
	anchor, err := os.OpenRoot(boundary)
	if err != nil {
		return nil, err
	}
	loc := &agentMediaLocation{opened: []*os.Root{anchor}}
	fail := func(err error) (*agentMediaLocation, error) { loc.close(); return nil, err }
	anchorInfo, err := anchor.Stat(".")
	if err != nil {
		return fail(err)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 0 || parts[len(parts)-1] == "." {
		return fail(mediaAgentPathError())
	}
	parent := anchor
	type binding struct {
		name string
		info os.FileInfo
	}
	var bindings []binding
	for i, part := range parts[:len(parts)-1] {
		if part == "" || part == "." || part == ".." {
			return fail(mediaAgentPathError())
		}
		info, err := parent.Lstat(part)
		if errors.Is(err, os.ErrNotExist) && create {
			if err = parent.Mkdir(part, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return fail(err)
			}
			info, err = parent.Lstat(part)
		}
		if err != nil {
			return fail(err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fail(mediaAgentPathError())
		}
		next, err := parent.OpenRoot(part)
		if err != nil {
			return fail(err)
		}
		loc.opened = append(loc.opened, next)
		fd, err := next.Stat(".")
		if err != nil || !os.SameFile(info, fd) {
			return fail(&app.MediaArtifactError{Code: "media_changed", Publication: "not_written", Cause: err})
		}
		bindings = append(bindings, binding{filepath.Join(parts[:i+1]...), fd})
		parent = next
	}
	loc.MediaLocation = app.MediaLocation{Root: parent, Name: parts[len(parts)-1], Path: canonical}
	loc.Check = func() error {
		info, err := os.Lstat(boundary)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, anchorInfo) {
			return &app.MediaArtifactError{Code: "media_changed", Publication: "not_written", Cause: err}
		}
		for _, b := range bindings {
			info, err := anchor.Lstat(b.name)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, b.info) {
				return &app.MediaArtifactError{Code: "media_changed", Publication: "not_written", Cause: err}
			}
		}
		// A hardlink under an allowed path must not make an archive control
		// file readable as media. Compare inode identities without reading it.
		file, err := parent.Lstat(loc.Name)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if file != nil && file.Mode().IsRegular() {
			entries, err := os.ReadDir(storeReal)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				control, err := os.Stat(filepath.Join(storeReal, entry.Name()))
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				if control != nil && os.SameFile(file, control) {
					return mediaAgentPathError()
				}
			}
		}
		return nil
	}
	if err := loc.Check(); err != nil {
		return fail(err)
	}
	return loc, nil
}

func mediaAgentPathError() error {
	return &app.MediaArtifactError{Code: "path_not_allowed", Publication: "not_written"}
}
