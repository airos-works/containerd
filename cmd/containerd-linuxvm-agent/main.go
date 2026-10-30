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

// containerd-linuxvm-agent is the init (PID 1) of air/OS's Linux VM. It
// serves containerd's TaskService to the haiku shim over a multiplexed
// virtio serial port and runs the containers with runc, so Linux images get
// real namespaces, cgroups and their own networks.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/linuxvm"
	"github.com/containerd/containerd/v2/pkg/linuxvm/mux"
	"github.com/containerd/containerd/v2/pkg/sys/reaper"
	runc "github.com/containerd/go-runc"
	"golang.org/x/sys/unix"
)

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "linuxvm-agent: "+format+"\n", args...)
}

func main() {
	if os.Getpid() == 1 {
		if err := initVM(); err != nil {
			logf("init: %v", err)
			// a broken VM is useless; let the host see it stop
			_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
		}
	}
	runc.Monitor = reaper.Default
	go reapChildren()

	svc := newTaskService()
	port, err := openAgentPort()
	if err == nil {
		logf("ready: serving the host on %s", port.Name())
	}
	if err != nil {
		logf("%v", err)
		_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
		return
	}
	for {
		serve(svc, port)
		// The host closed its end (the supervisor restarted): wait for it
		// to come back. Containers keep running meanwhile.
		time.Sleep(200 * time.Millisecond)
	}
}

// reapChildren reaps every process that ends: as PID 1 the agent inherits
// the containers' orphans, and go-runc learns its children's exit statuses
// from the reaper.
func reapChildren() {
	ch := make(chan os.Signal, 32)
	signal.Notify(ch, unix.SIGCHLD)
	for range ch {
		_ = reaper.Reap()
	}
}

// openAgentPort finds the virtio serial port the host named PortName.
func openAgentPort() (*os.File, error) {
	for i := 0; i < 100; i++ {
		ports, _ := filepath.Glob("/sys/class/virtio-ports/*/name")
		for _, p := range ports {
			b, err := os.ReadFile(p)
			if err != nil || strings.TrimSpace(string(b)) != linuxvm.PortName {
				continue
			}
			dev := "/dev/" + filepath.Base(filepath.Dir(p))
			return os.OpenFile(dev, os.O_RDWR, 0)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("no virtio port %q", linuxvm.PortName)
}

// serve runs one session over the port until the host goes away.
func serve(svc *taskService, port *os.File) {
	sess := mux.Server(port)
	ttrpcConns := make(chan net.Conn)
	go serveTTRPC(svc, ttrpcConns, sess.Done())
	for {
		st, err := sess.Accept()
		if err != nil {
			return
		}
		go handleStream(svc, st, ttrpcConns)
	}
}

func handleStream(svc *taskService, st *mux.Stream, ttrpcConns chan<- net.Conn) {
	fields := strings.Fields(st.Header())
	if len(fields) == 0 {
		st.Close()
		return
	}
	switch fields[0] {
	case linuxvm.HeaderPing:
		_, _ = io.WriteString(st, "pong\n")
		_ = st.CloseWrite()
	case linuxvm.HeaderInfo:
		_ = json.NewEncoder(st).Encode(vmInfo())
		_ = st.CloseWrite()
	case linuxvm.HeaderTime:
		// The VM's clock follows the host's (there is no kvmclock).
		if len(fields) == 2 {
			if ns, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				ts := unix.NsecToTimespec(ns)
				_ = unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
			}
		}
		st.Close()
	case linuxvm.HeaderPs:
		// every process in the VM: the Engine keeps a container's (docker top)
		out, _ := exec.Command("ps", "-o", "pid,user,ppid,stat,time,args").Output()
		_, _ = st.Write(out)
		_ = st.CloseWrite()
	case linuxvm.HeaderTTRPC:
		ttrpcConns <- st
	case linuxvm.HeaderIO:
		if len(fields) != 2 {
			st.Close()
			return
		}
		svc.io.register(fields[1], st)
	case linuxvm.HeaderDial:
		if len(fields) != 4 {
			st.Close()
			return
		}
		svc.dial(st, fields[1], fields[2], fields[3])
	default:
		logf("unknown stream %q", st.Header())
		st.Close()
	}
}

func vmInfo() linuxvm.Info {
	var uts unix.Utsname
	_ = unix.Uname(&uts)
	info := linuxvm.Info{
		Kernel: unix.ByteSliceToString(uts.Release[:]),
		CPUs:   runtime.NumCPU(),
	}
	var si unix.Sysinfo_t
	if unix.Sysinfo(&si) == nil {
		info.MemoryKB = uint64(si.Totalram) * uint64(si.Unit) / 1024
	}
	if v, err := newRunc().Version(context.Background()); err == nil {
		info.Runc = v.Runc
	}
	return info
}

// readLines returns a file's lines, or nothing.
func readLines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}
