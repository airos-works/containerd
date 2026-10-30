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
	"os"
	"os/exec"
	"strings"

	"github.com/containerd/containerd/v2/pkg/linuxvm"
	"golang.org/x/sys/unix"
)

const switchedEnv = "LINUXVM_SWITCHED_ROOT"

// initVM makes the initramfs a working system: a root runc can pivot away
// from, the kernel filesystems, cgroup v2 with its controllers, the host's
// files over virtio-9p, and the VM's network.
func initVM() error {
	if os.Getenv(switchedEnv) == "" {
		logf("making the root a mount of its own")
		if err := switchRoot(); err != nil {
			return err
		}
	}
	logf("mounting the kernel file systems")
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

	logf("mounting the host's files")
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
	logf("configuring the network")
	if err := setupVMNetwork(); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	return nil
}

// switchRoot makes the root a mount runc can pivot_root away from: the
// initial rootfs cannot be, but a bind mount of it moved over it can, and
// costs nothing (copying the initramfs to a tmpfs took most of an emulated
// VM's start-up).
func switchRoot() error {
	const newRoot = "/sysroot"
	if err := os.MkdirAll(newRoot, 0o755); err != nil {
		return err
	}
	if err := unix.Mount("/", newRoot, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind the root: %w", err)
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
	return os.Setenv(switchedEnv, "1")
}

// run runs a helper program from the VM's userland.
func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
