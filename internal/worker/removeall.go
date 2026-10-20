package worker

import (
	"io/fs"
	"os"
	"path/filepath"
)

// removeAllForce is os.RemoveAll with one extra step for a tree an agent's
// toolchain has written into.
//
// A Go toolchain run inside a workspace sandbox (.poc, .verify, .tools) fills
// GOPATH/pkg/mod, and `go mod download` creates every module directory
// read-only — dr-xr-xr-x. Unlinking an entry needs write permission on its
// *parent*, so os.RemoveAll fails there with EACCES and leaves the subtree
// behind; the caller logs "workspace cleanup failed" and the residue is
// permanent. One scan of a Go repository was measured at 209MB and 2392
// unremovable directories, and a few hundred of those are what filled a 301GB
// volume ("no space left on device", 538 scans, 2026-10-16).
//
// `go clean -modcache` exists for exactly this reason. Since the host is
// cleaning up after a process that is already gone, the equivalent here is to
// restore the write bit on the directories and try again, rather than to ask
// every skill to tidy up after itself.
func removeAllForce(path string) error {
	err := os.RemoveAll(path)
	if err == nil || !os.IsPermission(err) {
		return err
	}
	// Only directories need the write bit: a read-only *file* unlinks fine
	// once its parent is writable. Errors are ignored here — a path that
	// cannot be chmodded reports itself through the retry below, with the
	// original failure if that fails too.
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // unreadable subtree: nothing to widen, let the retry report it
		}
		if d.IsDir() {
			if info, statErr := d.Info(); statErr == nil {
				_ = os.Chmod(p, info.Mode().Perm()|0o200)
			}
		}
		return nil
	})
	if retryErr := os.RemoveAll(path); retryErr != nil {
		return retryErr
	}
	return nil
}
