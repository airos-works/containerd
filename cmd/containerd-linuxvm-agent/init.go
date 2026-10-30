//go:build linux

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containerd/containerd/v2/pkg/linuxvm"
	"golang.org/x/sys/unix"
)

const switchedEnv = "LINUXVM_SWITCHED_ROOT"

// initVM makes the initramfs a working system: a root runc can pivot away
// from, the kernel filesystems, cgroup v2 with its controllers, the host's
// files over virtio-9p, and the VM's network.
func initVM() error {
	if os.Getenv(switchedEnv) == "" {
		return switchRoot()
	}
	os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")

	mounts := []struct {
		src, dst, typ string
		flags         uintptr
		data          string
	}{
		{"proc", "/proc", "proc", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"sysfs", "/sys", "sysfs", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755"},
		{"devpts", "/dev/pts", "devpts", unix.MS_NOSUID | unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620,gid=5"},
		{"shm", "/dev/shm", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
		{"mqueue", "/dev/mqueue", "mqueue", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, "nsdelegate"},
		{"tmpfs", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=0755"},
		{"tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
	}
	for _, m := range mounts {
		if err := os.MkdirAll(m.dst, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(m.src, m.dst, m.typ, m.flags, m.data); err != nil && err != unix.EBUSY {
			return fmt.Errorf("mount %s: %w", m.dst, err)
		}
	}
	_ = os.Symlink("/proc/self/fd", "/dev/fd")
	_ = os.Symlink("/proc/self/fd/0", "/dev/stdin")
	_ = os.Symlink("/proc/self/fd/1", "/dev/stdout")
	_ = os.Symlink("/proc/self/fd/2", "/dev/stderr")
	_ = os.Remove("/dev/ptmx")
	_ = os.Symlink("pts/ptmx", "/dev/ptmx")

	// Every controller, for the containers' cgroups below.
	if b, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		var enable []string
		for _, c := range strings.Fields(string(b)) {
			enable = append(enable, "+"+c)
		}
		_ = os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte(strings.Join(enable, " ")), 0o644)
	}

	// The host's root directory, twice (a virtio 9p device mounts once).
	// cache=mmap keeps what the host changes (an edited bind-mounted
	// source file) visible at once, and still lets containers mmap their
	// files; cache=loose serves root filesystems and volumes from the page
	// cache, many times faster.
	for _, m := range []struct{ tag, dir, cache string }{
		{"host", linuxvm.HostMount, "mmap"},
		{"hostc", linuxvm.CachedMount, "loose"},
	} {
		if err := os.MkdirAll(m.dir, 0o755); err != nil {
			return err
		}
		opts := "trans=virtio,version=9p2000.L,msize=512000,access=client,cache=" + m.cache
		if err := unix.Mount(m.tag, m.dir, "9p", 0, opts+",posixacl"); err != nil {
			logf("mount the host's files with ACLs: %v", err)
			if err := unix.Mount(m.tag, m.dir, "9p", 0, opts); err != nil {
				return fmt.Errorf("mount the host's files at %s: %w", m.dir, err)
			}
		}
	}

	_ = unix.Sethostname([]byte("airos-linuxvm"))
	for _, s := range []struct{ path, value string }{
		{"/proc/sys/net/ipv4/ip_forward", "1"},
		{"/proc/sys/net/ipv6/conf/all/forwarding", "1"},
		{"/proc/sys/net/bridge/bridge-nf-call-iptables", "0"},
		{"/proc/sys/net/bridge/bridge-nf-call-ip6tables", "0"},
		{"/proc/sys/net/ipv4/ip_unprivileged_port_start", "0"},
		{"/proc/sys/fs/inotify/max_user_watches", "524288"},
		{"/proc/sys/vm/overcommit_memory", "1"},
	} {
		_ = os.WriteFile(s.path, []byte(s.value), 0o644)
	}
	if err := setupVMNetwork(); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	return nil
}

// switchRoot copies the initramfs to a tmpfs and makes that the root:
// runc's pivot_root cannot leave the initial rootfs.
func switchRoot() error {
	const newRoot = "/sysroot"
	if err := os.MkdirAll(newRoot, 0o755); err != nil {
		return err
	}
	if err := unix.Mount("rootfs", newRoot, "tmpfs", 0, "mode=0755"); err != nil {
		return err
	}
	err := filepath.WalkDir("/", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == newRoot || p == "/proc" || p == "/sys" || p == "/dev" {
			if p != "/" {
				_ = os.MkdirAll(newRoot+p, 0o755)
				return filepath.SkipDir
			}
		}
		if p == "/" {
			return nil
		}
		return copyEntry(p, newRoot+p, d)
	})
	if err != nil {
		return fmt.Errorf("copy root: %w", err)
	}
	// Free the old root's memory: nothing on it is needed any more.
	entries, _ := os.ReadDir("/")
	for _, e := range entries {
		switch e.Name() {
		case "sysroot", "proc", "sys", "dev":
			continue
		}
		_ = os.RemoveAll("/" + e.Name())
	}
	if err := unix.Chdir(newRoot); err != nil {
		return err
	}
	if err := unix.Mount(".", "/", "", unix.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move root: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return err
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}
	exe := "/init"
	if _, err := os.Stat(exe); err != nil {
		exe = "/sbin/containerd-linuxvm-agent"
	}
	return syscall.Exec(exe, os.Args, append(os.Environ(), switchedEnv+"=1"))
}

func copyEntry(src, dst string, d fs.DirEntry) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	st := info.Sys().(*syscall.Stat_t)
	switch mode := info.Mode(); {
	case mode.IsDir():
		if err := os.Mkdir(dst, mode.Perm()); err != nil && !os.IsExist(err) {
			return err
		}
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
	case mode.IsRegular():
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode.Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
	default:
		if err := unix.Mknod(dst, st.Mode, int(st.Rdev)); err != nil {
			return err
		}
	}
	_ = os.Lchown(dst, int(st.Uid), int(st.Gid))
	if info.Mode()&os.ModeSymlink == 0 {
		// setuid and friends: Mkdir and OpenFile drop them
		_ = unix.Chmod(dst, st.Mode&07777)
	}
	return nil
}

// run runs a helper program from the VM's userland.
func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
