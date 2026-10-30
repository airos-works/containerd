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

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"syscall"
)

const (
	defaultSocket = "/var/run/containerd-linuxvm.sock"
	defaultState  = "/var/run/containerd-linuxvm"
)

// The package installs the VM under /boot/system/data/linuxvm and its
// QEMU under /boot/system/lib/linuxvm (not on PATH: it is the VM's own);
// a build of one's own goes in the non-packaged hierarchy.
func defaultData() string { return firstExisting("data/linuxvm/vmlinuz", "data/linuxvm") }

func defaultQEMU() string {
	name := "qemu-system-x86_64"
	if runtime.GOARCH == "arm64" {
		name = "qemu-system-aarch64"
	}
	return firstExisting("lib/linuxvm/"+name, "lib/linuxvm/"+name)
}

// firstExisting returns /boot/system/non-packaged/<result> if <probe> is
// there, and /boot/system/<result> otherwise.
func firstExisting(probe, result string) string {
	if _, err := os.Stat(filepath.Join("/boot/system/non-packaged", probe)); err == nil {
		return filepath.Join("/boot/system/non-packaged", result)
	}
	return filepath.Join("/boot/system", result)
}

func settingsPath() string {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/boot/home"
	}
	return filepath.Join(home, "config/settings/docker_linuxvm")
}

var memMax = regexp.MustCompile(`/ (\d+)\)`)

func hostMemoryMB() int64 {
	out, err := exec.Command("sysinfo", "-mem").Output()
	if err != nil {
		return 4096
	}
	m := memMax.FindSubmatch(out)
	if m == nil {
		return 4096
	}
	n, _ := strconv.ParseInt(string(m[1]), 10, 64)
	return n >> 20
}

// accelerator is NVMM where the kernel driver found SVM or VMX.
func accelerator() string {
	if runtime.GOARCH == "amd64" {
		if _, err := os.Stat("/dev/misc/nvmm"); err == nil {
			return "nvmm"
		}
	}
	return "tcg"
}

func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
