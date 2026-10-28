//go:build haiku

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

// containerd-shim-haiku-v2 is the air/OS container runtime. It implements
// the containerd shim v3 bootstrap and TaskService over ttrpc, and runs
// containers as chrooted process trees (Haiku has no namespaces). The
// binary is installed as containerd-shim-runc-v2 so containerd's default
// runtime (io.containerd.runc.v2) resolves to it.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// init-mode re-exec entry: the task service spawns the shim binary with
// "--haiku-init" to run the container process. Everything the init child
// needs travels in the environment because the argument vector is the
// container's own.
const initFlag = "--haiku-init"

const (
	envInitRootfs = "HAIKU_SHIM_INIT_ROOTFS"
	envInitCwd    = "HAIKU_SHIM_INIT_CWD"
	// envInitChroot enables filesystem isolation via chroot. Haiku only
	// supports chroot into mount points, so this stays off until the
	// snapshotter mounts a bfs image per container.
	envInitChroot = "HAIKU_SHIM_INIT_CHROOT"
)

// runInit performs the actual container start: chroot into the rootfs,
// set the working directory, and exec the entrypoint. Never returns on
// success.
func runInit() error {
	rootfs := os.Getenv(envInitRootfs)
	cwd := os.Getenv(envInitCwd)
	if rootfs == "" {
		return fmt.Errorf("init: %s not set", envInitRootfs)
	}
	// Chroot is opt-in: it is broken on current Haiku (executables exit
	// silently inside a chroot even from a mount point), so containers
	// run cwd-isolated by default. HAIKU_SHIM_INIT_CHROOT=1 forces it for
	// kernels where chroot works.
	chrootMode := os.Getenv(envInitChroot) == "1"
	if chrootMode {
		if err := syscall.Chroot(rootfs); err != nil {
			return fmt.Errorf("init: chroot %s: %w", rootfs, err)
		}
		if cwd == "" {
			cwd = "/"
		}
		if err := syscall.Chdir(cwd); err != nil {
			_ = syscall.Chdir("/")
		}
	} else {
		// Filesystem isolation is not available yet (Haiku chroot needs
		// a mount point): run with the rootfs as the working directory.
		if cwd == "" || cwd == "/" {
			cwd = rootfs
		} else {
			cwd = rootfs + cwd
		}
		if err := syscall.Chdir(cwd); err != nil {
			return fmt.Errorf("init: chdir %s: %w", cwd, err)
		}
	}
	argv := os.Args[2:]
	if len(argv) == 0 {
		return fmt.Errorf("init: empty argv")
	}
	if !chrootMode && len(argv[0]) > 0 && argv[0][0] == '/' {
		// Without chroot, absolute paths would resolve against the host
		// root; make the entrypoint relative to the rootfs working dir.
		argv[0] = "." + argv[0]
	}
	env := os.Environ()
	clean := env[:0]
	for _, e := range env {
		if strings.HasPrefix(e, envInitRootfs+"=") || strings.HasPrefix(e, envInitCwd+"=") || strings.HasPrefix(e, envInitChroot+"=") {
			continue
		}
		clean = append(clean, e)
	}
	if err := syscall.Exec(argv[0], argv, clean); err != nil {
		return fmt.Errorf("init: exec %s: %w", argv[0], err)
	}
	return nil
}

func main() {
	// Unconditional trace into the bundle directory (the shim's cwd for
	// both helper and server modes), best effort.
	if f, err := os.OpenFile("shim-debug.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
		fmt.Fprintf(f, "shim invoked: args=%v\n", os.Args)
		os.Stderr = f
	} else {
		if f2, err2 := os.OpenFile("/boot/home/shim-debug.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err2 == nil {
			fmt.Fprintf(f2, "shim invoked (fallback): args=%v cwd-err=%v\n", os.Args, err)
			os.Stderr = f2
		}
	}
	if len(os.Args) > 1 && os.Args[1] == initFlag {
		if err := runInit(); err != nil {
			fmt.Fprintf(os.Stderr, "haiku-shim: %s\n", err)
		}
		os.Exit(127)
	}
	// Server (long-running) and helper invocations share plugin
	// registration so the task service is available in server mode.
	registerTaskPlugin()
	RunShimHaiku(context.Background())
}
