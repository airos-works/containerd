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
	"syscall"
	"unsafe"

	v2 "github.com/containerd/cgroups/v3/cgroup2/stats"
)

// Haiku has no cgroups: a container's resource usage is read from the kernel
// kit's team and area info (libroot), for every team in the container's
// process trees. The calls go through assembly trampolines to the
// dynamically imported symbols (kernel_haiku_$GOARCH.s), the way the syscall
// package's own wrappers do.

//go:cgo_import_dynamic libc__get_next_team_info _get_next_team_info "libroot.so"
//go:cgo_import_dynamic libc__get_team_usage_info _get_team_usage_info "libroot.so"
//go:cgo_import_dynamic libc__get_next_area_info _get_next_area_info "libroot.so"
//go:cgo_import_dynamic libc_sysconf sysconf "libroot.so"

var (
	libc__get_next_team_info_trampoline_addr  uintptr
	libc__get_team_usage_info_trampoline_addr uintptr
	libc__get_next_area_info_trampoline_addr  uintptr
	libc_sysconf_trampoline_addr              uintptr
)

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

// teamInfo is team_info from <OS.h>.
type teamInfo struct {
	Team              int32
	ThreadCount       int32
	ImageCount        int32
	AreaCount         int32
	DebuggerNubThread int32
	DebuggerNubPort   int32
	Argc              int32
	Args              [64]byte
	UID               uint32
	GID               uint32
	RealUID           uint32
	RealGID           uint32
	GroupID           int32
	SessionID         int32
	Parent            int32
	Name              [32]byte
	StartTime         int64
}

// teamUsageInfo is team_usage_info from <OS.h>, in microseconds.
type teamUsageInfo struct {
	UserTime   int64
	KernelTime int64
}

// areaInfo is area_info from <OS.h>.
type areaInfo struct {
	Area       int32
	Name       [32]byte
	Size       uintptr
	Lock       uint32
	Protection uint32
	Team       int32
	RAMSize    uint32
	CopyCount  uint32
	InCount    uint32
	OutCount   uint32
	Address    uintptr
}

const (
	teamUsageSelf     = 0  // B_TEAM_USAGE_SELF
	teamUsageChildren = -1 // B_TEAM_USAGE_CHILDREN: its waited-for children
	scPageSize        = 27 // _SC_PAGE_SIZE
	scPhysPages       = 40 // _SC_PHYS_PAGES
)

func nextTeamInfo(cookie *int32, info *teamInfo) bool {
	r, _, _ := syscall_syscall6(libc__get_next_team_info_trampoline_addr,
		uintptr(unsafe.Pointer(cookie)), uintptr(unsafe.Pointer(info)), unsafe.Sizeof(*info), 0, 0, 0)
	return int32(r) == 0
}

func teamUsage(team int32, who int32) (teamUsageInfo, bool) {
	var info teamUsageInfo
	r, _, _ := syscall_syscall6(libc__get_team_usage_info_trampoline_addr,
		uintptr(team), uintptr(who), uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0, 0)
	return info, int32(r) == 0
}

func nextAreaInfo(team int32, cookie *int, info *areaInfo) bool {
	r, _, _ := syscall_syscall6(libc__get_next_area_info_trampoline_addr,
		uintptr(team), uintptr(unsafe.Pointer(cookie)), uintptr(unsafe.Pointer(info)), unsafe.Sizeof(*info), 0, 0)
	return int32(r) == 0
}

func sysconf(name int) int64 {
	r, _, _ := syscall_syscall6(libc_sysconf_trampoline_addr, uintptr(name), 0, 0, 0, 0, 0)
	return int64(r)
}

// teamTree returns the teams in roots that still run and all their
// descendants: the processes of a container, as a cgroup would hold them.
func teamTree(roots []int32) []int32 {
	children := make(map[int32][]int32)
	alive := make(map[int32]bool)
	var cookie int32
	var info teamInfo
	for nextTeamInfo(&cookie, &info) {
		children[info.Parent] = append(children[info.Parent], info.Team)
		alive[info.Team] = true
	}

	var teams []int32
	seen := make(map[int32]bool)
	queue := append([]int32(nil), roots...)
	for len(queue) > 0 {
		team := queue[0]
		queue = queue[1:]
		if seen[team] || !alive[team] {
			continue
		}
		seen[team] = true
		teams = append(teams, team)
		queue = append(queue, children[team]...)
	}
	return teams
}

// teamMetrics adds up the usage of the teams in roots and all their
// descendants, in the cgroup v2 shape the daemon reads (Linux shims report
// a container's cgroup the same way). Memory is the RAM its areas hold;
// the limit is the machine's RAM, as Haiku has no per-container limits.
func teamMetrics(roots []int32) *v2.Metrics {
	var user, kernel, ram, teams uint64
	for _, team := range teamTree(roots) {
		self, ok := teamUsage(team, teamUsageSelf)
		if !ok {
			continue // exited since the team list was read
		}
		teams++
		user += uint64(self.UserTime)
		kernel += uint64(self.KernelTime)
		if dead, ok := teamUsage(team, teamUsageChildren); ok {
			user += uint64(dead.UserTime)
			kernel += uint64(dead.KernelTime)
		}
		var areaCookie int
		var area areaInfo
		for nextAreaInfo(team, &areaCookie, &area) {
			ram += uint64(area.RAMSize)
		}
	}

	var limit uint64
	if pages, size := sysconf(scPhysPages), sysconf(scPageSize); pages > 0 && size > 0 {
		limit = uint64(pages) * uint64(size)
	}
	return &v2.Metrics{
		Pids: &v2.PidsStat{Current: teams},
		CPU: &v2.CPUStat{
			UsageUsec:  user + kernel,
			UserUsec:   user,
			SystemUsec: kernel,
		},
		Memory: &v2.MemoryStat{
			Usage:      ram,
			UsageLimit: limit,
		},
	}
}
