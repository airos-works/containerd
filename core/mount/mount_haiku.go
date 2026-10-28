//go:build haiku

package mount

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/containerd/errdefs"
)

// Mount is not implemented on Haiku; the Engine realizes snapshot mounts
// itself (symlink alias or bfs volume) in daemon/snapshotter.
func (m *Mount) mount(target string) error {
	return errdefs.ErrNotImplemented
}

// unmount removes the realization of a mount point: a symlink alias is
// deleted, a directory is released with the Haiku unmount command.
func unmount(target string, flags int) error {
	fi, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return os.Remove(target)
	}
	if fi.IsDir() {
		// Only real mounts (bfs volumes) need releasing; plain
		// directories (e.g. unused bundle rootfs) are fine as-is.
		var st, parent syscall.Stat_t
		if err := syscall.Stat(target, &st); err == nil {
			if err := syscall.Stat(filepath.Dir(target), &parent); err == nil && st.Dev != parent.Dev {
				return exec.Command("unmount", target).Run()
			}
		}
	}
	return nil
}

// Unmount releases the mount at target, ignoring stale paths.
func Unmount(target string, flags int) error {
	return unmount(target, flags)
}

// UnmountAll releases the mount point repeatedly; a single pass suffices
// on Haiku.
func UnmountAll(mount string, flags int) error {
	if mount == "" {
		return nil
	}
	if _, err := os.Stat(mount); os.IsNotExist(err) {
		return nil
	}
	return unmount(mount, flags)
}

// UnmountRecursive walks the mount tree rooted at target and releases it.
// On Haiku only the root itself is realized, so this equals UnmountAll.
func UnmountRecursive(target string, flags int) error {
	if target == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Clean(target)); os.IsNotExist(err) {
		return nil
	}
	return unmount(target, flags)
}
