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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

// On Linux the supervisor exists to develop the agent with KVM.
const (
	defaultSocket = "/tmp/containerd-linuxvm.sock"
	defaultState  = "/tmp/containerd-linuxvm"
)

func defaultData() string {
	exe, _ := os.Executable()
	return filepath.Join(filepath.Dir(exe), "linuxvm")
}

func defaultQEMU() string {
	name := "qemu-system-x86_64"
	if runtime.GOARCH == "arm64" {
		name = "qemu-system-aarch64"
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return name
}

func settingsPath() string { return filepath.Join(os.Getenv("HOME"), ".config/docker_linuxvm") }

func hostMemoryMB() int64 {
	var si unix.Sysinfo_t
	if unix.Sysinfo(&si) != nil {
		return 4096
	}
	return int64(si.Totalram) * int64(si.Unit) >> 20
}

func accelerator() string {
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
		f.Close()
		return "kvm"
	}
	return "tcg"
}

func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
