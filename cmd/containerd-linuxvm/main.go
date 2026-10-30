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

// containerd-linuxvm runs air/OS's Linux VM: QEMU (with NVMM where the CPU
// has it) booting a small Linux whose init is containerd-linuxvm-agent. It
// owns the VM's one virtio serial channel and multiplexes it: every client
// connection to its Unix socket sends a header line and becomes one stream
// to the agent. The haiku shim starts it on demand for the first container
// from a Linux image.
//
//	containerd-linuxvm [flags]             the supervisor
//	containerd-linuxvm status | stop       ask the running one
//	containerd-linuxvm run ROOTFS CMD...   a container from a directory
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/pkg/linuxvm"
	"github.com/containerd/containerd/v2/pkg/linuxvm/mux"
)

// Status is what a "status" request returns.
type Status struct {
	Running  bool          `json:"running"`
	Booting  bool          `json:"booting,omitempty"`
	QEMUPid  int           `json:"qemuPid,omitempty"`
	Accel    string        `json:"accel,omitempty"`
	MemoryMB int           `json:"memoryMB,omitempty"`
	CPUs     int           `json:"cpus,omitempty"`
	Booted   time.Time     `json:"booted,omitempty"`
	Info     *linuxvm.Info `json:"info,omitempty"`
	Error    string        `json:"error,omitempty"`
}

type vm struct {
	cfg config

	mu      sync.Mutex
	cmd     *exec.Cmd
	sess    *mux.Session
	accel   string
	booted  time.Time
	lastErr error
	exited  chan struct{}
}

type config struct {
	socket   string
	stateDir string
	dataDir  string
	qemu     string
	memoryMB int
	cpus     int
	// bootTimeout is how long to wait for the agent, in seconds (0: a
	// minute, 20 under TCG)
	bootTimeout int
	share       string
}

func main() {
	var cfg config
	flag.StringVar(&cfg.socket, "socket", defaultSocket, "the Unix socket clients connect to")
	flag.StringVar(&cfg.stateDir, "state", defaultState, "runtime state (the agent channel, the console log)")
	flag.StringVar(&cfg.dataDir, "data", defaultData(), "the VM's kernel and initramfs")
	flag.StringVar(&cfg.qemu, "qemu", defaultQEMU(), "the QEMU system emulator")
	flag.IntVar(&cfg.memoryMB, "memory", 0, "the VM's memory in MiB (0: a quarter of the host's, 1-8 GiB)")
	flag.IntVar(&cfg.cpus, "cpus", 0, "the VM's processors (0: half the host's, at most 8)")
	flag.StringVar(&cfg.share, "share", "/", "the host directory the VM mounts at "+linuxvm.HostMount)
	flag.Parse()
	switch flag.Arg(0) {
	case "run":
		os.Exit(runContainer(cfg.socket, flag.Args()[1:]))
	case "status", "stop", "debug":
		os.Exit(request(cfg.socket, flag.Arg(0)))
	}
	readSettings(&cfg)
	if cfg.cpus <= 0 {
		cfg.cpus = min(max(runtime.NumCPU()/2, 1), 8)
	}
	if cfg.memoryMB <= 0 {
		cfg.memoryMB = min(max(int(hostMemoryMB()/4), 1024), 8192)
	}
	log.SetPrefix("containerd-linuxvm: ")
	log.SetFlags(log.LstdFlags)

	if err := os.MkdirAll(cfg.stateDir, 0o700); err != nil {
		log.Fatal(err)
	}
	l, err := listen(cfg.socket)
	if err != nil {
		log.Fatal(err)
	}
	v := &vm{cfg: cfg}
	v.killStale()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	stopping := make(chan struct{})
	go func() {
		<-sig
		close(stopping)
		l.Close()
		os.Remove(cfg.socket)
		v.stop()
		os.Exit(0)
	}()

	for {
		c, err := l.Accept()
		if err != nil {
			select {
			case <-stopping:
				select {} // the signal handler stops the VM and exits
			default:
			}
			log.Fatal(err)
		}
		go v.handle(c)
	}
}

// killStale stops a VM whose supervisor died without stopping it, so that
// two never share the agent's socket and the host's files.
func (v *vm) killStale() {
	b, err := os.ReadFile(v.pidFile())
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil && p.Signal(syscall.Signal(0)) == nil {
		log.Printf("stopping the VM a previous supervisor left running (pid %d)", pid)
		_ = p.Kill()
	}
	os.Remove(v.pidFile())
}

func (v *vm) pidFile() string { return filepath.Join(v.cfg.stateDir, "qemu.pid") }

// request is "containerd-linuxvm status|stop": asks the running supervisor.
func request(socket, what string) int {
	c, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "the Linux VM's supervisor is not running")
		return 1
	}
	defer c.Close()
	if _, err := io.WriteString(c, what+"\n"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_, _ = io.Copy(os.Stdout, c)
	return 0
}

// listen takes the socket unless a live supervisor already has it.
func listen(path string) (net.Listener, error) {
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return nil, fmt.Errorf("already running on %s", path)
	}
	os.Remove(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return net.Listen("unix", path)
}

// readSettings reads "key value" lines from the data directory's
// settings file (Docker Desktop writes it): memory (MiB), cpus, and
// boot_timeout (seconds) for unusually slow machines.
func readSettings(cfg *config) {
	f, err := os.Open(settingsPath())
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			continue
		}
		switch k {
		case "memory":
			if cfg.memoryMB == 0 {
				cfg.memoryMB = n
			}
		case "cpus":
			if cfg.cpus == 0 {
				cfg.cpus = n
			}
		case "boot_timeout":
			cfg.bootTimeout = n
		}
	}
}

func (v *vm) handle(c net.Conn) {
	br := bufio.NewReader(c)
	header, err := br.ReadString('\n')
	if err != nil {
		c.Close()
		return
	}
	header = strings.TrimSpace(header)
	switch header {
	case "status":
		_ = json.NewEncoder(c).Encode(v.status())
		c.Close()
		return
	case "stop":
		v.stop()
		_, _ = io.WriteString(c, "stopped\n")
		c.Close()
		return
	case "debug":
		// every goroutine's stack, to see what a stuck supervisor waits on
		buf := make([]byte, 1<<20)
		_, _ = c.Write(buf[:runtime.Stack(buf, true)])
		c.Close()
		return
	}
	sess, err := v.session()
	if err != nil {
		fmt.Fprintf(c, "error %v\n", err)
		c.Close()
		return
	}
	st, err := sess.Open(header)
	if err != nil {
		fmt.Fprintf(c, "error %v\n", err)
		c.Close()
		return
	}
	// The client reads this line before the stream's own bytes.
	if _, err := io.WriteString(c, "ok\n"); err != nil {
		st.Close()
		c.Close()
		return
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(st, br)
		_ = st.CloseWrite()
		close(done)
	}()
	_, _ = io.Copy(c, st)
	if uc, ok := c.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	st.Close()
	c.Close()
}

func (v *vm) status() Status {
	// A boot holds the lock until the agent answers: say so rather than wait.
	if !v.mu.TryLock() {
		return Status{Booting: true, MemoryMB: v.cfg.memoryMB, CPUs: v.cfg.cpus}
	}
	sess, cmd := v.sess, v.cmd
	s := Status{Accel: v.accel, MemoryMB: v.cfg.memoryMB, CPUs: v.cfg.cpus, Booted: v.booted}
	if v.lastErr != nil {
		s.Error = v.lastErr.Error()
	}
	v.mu.Unlock()
	if sess == nil || cmd == nil {
		return s
	}
	s.Running = true
	s.QEMUPid = cmd.Process.Pid
	if st, err := sess.Open(linuxvm.HeaderInfo); err == nil {
		var info linuxvm.Info
		_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
		if json.NewDecoder(st).Decode(&info) == nil {
			s.Info = &info
		}
		st.Close()
	}
	return s
}

// session returns the agent session, booting the VM first if it is not
// running.
func (v *vm) session() (*mux.Session, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.sess != nil {
		select {
		case <-v.sess.Done():
			v.sess = nil
		default:
			return v.sess, nil
		}
	}
	if err := v.boot(); err != nil {
		v.lastErr = err
		log.Printf("boot: %v", err)
		return nil, err
	}
	v.lastErr = nil
	return v.sess, nil
}

// boot starts QEMU and waits for the agent; v.mu is held.
func (v *vm) boot() error {
	if v.cmd != nil {
		v.stopLocked()
	}
	kernel := filepath.Join(v.cfg.dataDir, "vmlinuz")
	initrd := filepath.Join(v.cfg.dataDir, "initramfs.img")
	for _, f := range []string{kernel, initrd} {
		if _, err := os.Stat(f); err != nil {
			return fmt.Errorf("the Linux VM is not installed: %w", err)
		}
	}
	agentSock := filepath.Join(v.cfg.stateDir, "agent.sock")
	os.Remove(agentSock)
	accel := accelerator()
	args := []string{
		"-nodefaults", "-no-user-config", "-nographic",
		"-machine", machineType(accel), "-accel", accel,
		"-cpu", cpuModel(accel),
		"-smp", strconv.Itoa(v.cfg.cpus),
		"-m", strconv.Itoa(v.cfg.memoryMB),
		"-kernel", kernel, "-initrd", initrd,
		// not quiet: the console log is what tells why a VM did not come up
		"-append", "console=" + consoleDevice + " panic=-1 rdinit=/init" + kernelArgs,
		"-no-reboot",
		"-chardev", "file,id=console,path=" + filepath.Join(v.cfg.stateDir, "console.log"),
		"-serial", "chardev:console",
		"-device", virtioDevice("virtio-serial"),
		"-chardev", "socket,id=agent,path=" + agentSock + ",server=on,wait=off",
		"-device", "virtserialport,chardev=agent,name=" + linuxvm.PortName,
		"-fsdev", "local,id=host,path=" + v.cfg.share + ",security_model=passthrough,multidevs=remap",
		"-device", virtioDevice("virtio-9p") + ",fsdev=host,mount_tag=host",
		"-fsdev", "local,id=hostc,path=" + v.cfg.share + ",security_model=passthrough,multidevs=remap",
		"-device", virtioDevice("virtio-9p") + ",fsdev=hostc,mount_tag=hostc",
		"-netdev", "user,id=net0,ipv6=off",
		"-device", virtioDevice("virtio-net") + ",netdev=net0,romfile=",
		"-device", virtioDevice("virtio-rng"),
	}
	if fw := firmwareDir(v.cfg.dataDir); fw != "" {
		args = append([]string{"-L", fw}, args...)
	}
	cmd := exec.Command(v.cfg.qemu, args...)
	logf, err := os.OpenFile(filepath.Join(v.cfg.stateDir, "qemu.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err == nil {
		cmd.Stdout = logf
		cmd.Stderr = logf
	}
	cmd.SysProcAttr = detachAttr()
	log.Printf("booting: %s %s", v.cfg.qemu, strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start QEMU: %w", err)
	}
	_ = os.WriteFile(v.pidFile(), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644)
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		if logf != nil {
			logf.Close()
		}
		close(exited)
	}()

	conn, err := dialRetry(agentSock, exited, 10*time.Second)
	if err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("%w (see %s)", err, filepath.Join(v.cfg.stateDir, "qemu.log"))
	}
	sess := mux.Client(conn)
	// Without a hypervisor (TCG) the kernel boots several times slower.
	bootTimeout := 60 * time.Second
	if accel == "tcg" {
		bootTimeout = 20 * time.Minute
	}
	if v.cfg.bootTimeout > 0 {
		bootTimeout = time.Duration(v.cfg.bootTimeout) * time.Second
	}
	if err := ping(sess, exited, bootTimeout); err != nil {
		sess.Close()
		_ = cmd.Process.Kill()
		return fmt.Errorf("the agent did not answer: %w (see %s)", err, filepath.Join(v.cfg.stateDir, "console.log"))
	}
	v.cmd, v.sess, v.accel, v.exited, v.booted = cmd, sess, accel, exited, time.Now()
	log.Printf("the VM is up (%s, pid %d)", accel, cmd.Process.Pid)
	go syncClock(sess)
	go func() {
		<-exited
		log.Printf("QEMU exited")
		sess.Close()
	}()
	return nil
}

// syncClock sets the VM's clock from the host's now and every minute.
func syncClock(sess *mux.Session) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if st, err := sess.Open(fmt.Sprintf("%s %d", linuxvm.HeaderTime, time.Now().UnixNano())); err == nil {
			st.Close()
		}
		select {
		case <-sess.Done():
			return
		case <-t.C:
		}
	}
}

func dialRetry(path string, exited <-chan struct{}, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	for {
		c, err := net.Dial("unix", path)
		if err == nil {
			return c, nil
		}
		select {
		case <-exited:
			return nil, errors.New("QEMU exited")
		default:
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ping waits until the agent answers. Each attempt waits longer for the
// pong (from a second to 30): an emulated VM answers slowly, and attempts
// given up too soon only queue more work for it.
func ping(sess *mux.Session, exited <-chan struct{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	wait := time.Second
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return errors.New("QEMU exited")
		default:
		}
		st, err := sess.Open(linuxvm.HeaderPing)
		if err != nil {
			return err
		}
		_ = st.SetReadDeadline(time.Now().Add(wait))
		line, err := bufio.NewReader(st).ReadString('\n')
		st.Close()
		if err == nil && strings.TrimSpace(line) == "pong" {
			return nil
		}
		// Until the guest opens its port, what we sent waits: ask again,
		// and wait longer.
		wait = min(wait*2, 30*time.Second)
	}
	return errors.New("timed out")
}

func (v *vm) stop() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stopLocked()
}

func (v *vm) stopLocked() {
	if v.sess != nil {
		v.sess.Close()
		v.sess = nil
	}
	if v.cmd != nil {
		_ = v.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-v.exited:
		case <-time.After(5 * time.Second):
			_ = v.cmd.Process.Kill()
		}
		v.cmd = nil
		os.Remove(v.pidFile())
	}
}

func firmwareDir(data string) string {
	fw := filepath.Join(data, "firmware")
	if _, err := os.Stat(fw); err == nil {
		return fw
	}
	return ""
}
