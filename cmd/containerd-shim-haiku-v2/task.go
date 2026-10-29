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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	eventtypes "github.com/containerd/containerd/api/events"
	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	taskpb "github.com/containerd/containerd/api/types/task"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/core/events"
	typeurl "github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// proc is any supervised process: a container init or an exec process.
type proc struct {
	id     string
	execID string
	rootfs string
	cwd    string
	env    []string

	cmd     *exec.Cmd
	pid     int
	started bool

	exitStatus uint32
	exitedAt   time.Time
	exited     bool

	stdin  *os.File
	stdout *os.File
	stderr *os.File

	stdinPath  string
	stdoutPath string
	stderrPath string

	// Terminal state: master is the pty master; the child holds the slave.
	// ptyDone closes once the pty's output has all been copied to stdout.
	master  *os.File
	ptyDone chan struct{}

	waitCh chan struct{}
}

// haikuTaskService implements the containerd TaskService. Containers run
// as chrooted processes spawned by the shim's own init re-exec; there is no
// supervising runtime like runc.
type haikuTaskService struct {
	mu        sync.Mutex
	tasks     map[string]*proc
	execs     map[string]map[string]*proc // container id -> exec id -> proc
	publisher events.Publisher
}

func newHaikuTaskService(publisher events.Publisher) *haikuTaskService {
	return &haikuTaskService{
		tasks:     make(map[string]*proc),
		execs:     make(map[string]map[string]*proc),
		publisher: publisher,
	}
}

// openPty allocates a pty master/slave pair (Haiku /dev/tt/pN). Both ends
// are opened with O_NOCTTY: Haiku's pty driver refuses to open a slave as
// the opener's controlling terminal unless it leads a session, and the
// terminal belongs to the container process (see ttyAttr), not the shim.
func openPty() (master, slave *os.File, _ error) {
	mfd, err := syscall.PosixOpenpt(syscall.O_RDWR | syscall.O_NOCTTY)
	if err != nil {
		return nil, nil, err
	}
	if err := syscall.Grantpt(mfd); err != nil {
		syscall.Close(mfd)
		return nil, nil, err
	}
	if err := syscall.Unlockpt(mfd); err != nil {
		syscall.Close(mfd)
		return nil, nil, err
	}
	name, err := syscall.Ptsname(mfd)
	if err != nil {
		syscall.Close(mfd)
		return nil, nil, err
	}
	slave, err = os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		syscall.Close(mfd)
		return nil, nil, err
	}
	return os.NewFile(uintptr(mfd), "ptymaster"), slave, nil
}

// pumpPty copies between the pty master and the client's fifos. The
// returned channel closes when the output side ends: the shim closes its
// slave once the child has it, so that happens when the container's
// processes have all closed the terminal.
func pumpPty(master, stdin, stdout *os.File) chan struct{} {
	if stdin != nil {
		go func() {
			_, _ = io.Copy(master, stdin)
		}()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var out io.Writer = io.Discard
		if stdout != nil {
			out = stdout
		}
		_, _ = io.Copy(out, master)
	}()
	return done
}

// ttyAttr starts a process on a pty in its own session, with the pty
// (its stdin) as its controlling terminal, so that job control and the
// terminal's signals (^C, ^Z) reach it.
var ttyAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}

// procAttr starts a container process as the leader of its own process
// group (a session, on a pty), which the teams it starts join: Kill signals
// the group, and when the init exits its group goes with it.
func procAttr(tty bool) *syscall.SysProcAttr {
	if tty {
		return ttyAttr
	}
	return &syscall.SysProcAttr{Setpgid: true}
}

// waitPty waits briefly for a terminal's remaining output to reach the
// client before its stdio is closed; a background process that keeps the
// terminal open must not hold up the exit.
func waitPty(t *proc) {
	if t.ptyDone == nil {
		return
	}
	select {
	case <-t.ptyDone:
	case <-time.After(2 * time.Second):
	}
}

// haikuWinsize matches struct winsize in termios.h.
type haikuWinsize struct {
	Row, Col, Xpixel, Ypixel uint16
}

// loadSpec reads the OCI configuration from the bundle directory.
func loadSpec(bundle string) (*specs.Spec, error) {
	f, err := os.Open(filepath.Join(bundle, "config.json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var s specs.Spec
	if err := json.NewDecoder(f).Decode(&s); err != nil {
		return nil, fmt.Errorf("decode config.json: %w", err)
	}
	return &s, nil
}

// openFifo opens a stdio fifo. The daemon creates the fifos and opens
// its ends before calling Create, so blocking opens succeed immediately
// and give correct EOF/EPIPE semantics: stdin reads see EOF when the
// client closes its write end, stdout writes fail with EPIPE on detach.
func openFifo(path string, flag int) (*os.File, error) {
	if path == "" {
		return nil, nil
	}
	return os.OpenFile(path, flag, 0)
}

func (s *haikuTaskService) Create(ctx context.Context, req *taskapi.CreateTaskRequest) (_ *taskapi.CreateTaskResponse, err error) {
	defer func() {
		if r := recover(); r != nil {
			os.Stderr.WriteString(fmt.Sprintf("shim PANIC in Create: %v\n", r))
			err = fmt.Errorf("internal: %v", r)
		}
	}()
	os.Stderr.WriteString(fmt.Sprintf("shim Create: id=%s bundle=%s stdin=%s stdout=%s stderr=%s\n",
		req.ID, req.Bundle, req.Stdin, req.Stdout, req.Stderr))
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tasks[req.ID]; exists {
		return nil, fmt.Errorf("task %s already exists", req.ID)
	}
	bundle := req.Bundle
	if bundle == "" {
		bundle = mustGetwd()
	}
	spec, err := loadSpec(bundle)
	if err != nil {
		return nil, err
	}
	if spec.Root == nil || spec.Root.Path == "" {
		return nil, fmt.Errorf("spec has no root filesystem path")
	}
	if spec.Process == nil || len(spec.Process.Args) == 0 {
		return nil, fmt.Errorf("spec has no process args")
	}
	if err := linkBindMounts(spec.Root.Path, spec.Mounts); err != nil {
		return nil, err
	}

	// Stdio: containerd created fifos for each non-empty stream. Opening
	// them blocks until the client attaches; the daemon does this before
	// Start, so by Create time the other ends exist.
	stdin, err := openFifo(req.Stdin, os.O_RDONLY)
	if err != nil {
		return nil, fmt.Errorf("open stdin fifo: %w", err)
	}
	stdout, err := openFifo(req.Stdout, os.O_WRONLY)
	if err != nil {
		return nil, fmt.Errorf("open stdout fifo: %w", err)
	}
	stderr := stdout
	if req.Stderr != "" && req.Stderr != req.Stdout {
		stderr, err = openFifo(req.Stderr, os.O_WRONLY)
		if err != nil {
			return nil, fmt.Errorf("open stderr fifo: %w", err)
		}
	}

	childStdin, childStdout, childStderr := stdin, stdout, stderr
	var master, slave *os.File
	if req.Terminal {
		master, slave, err = openPty()
		if err != nil {
			return nil, fmt.Errorf("allocate pty: %w", err)
		}
		childStdin = slave
		childStdout = slave
		childStderr = slave
	}

	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	argv := append([]string{filepath.Base(self), initFlag}, spec.Process.Args...)
	cmd := exec.Command(self, argv[1:]...)
	cmd.Dir = "/"
	cmd.Env = append([]string{
		envInitRootfs + "=" + spec.Root.Path,
		envInitCwd + "=" + spec.Process.Cwd,
	}, spec.Process.Env...)
	cmd.Stdin = childStdin
	cmd.Stdout = childStdout
	cmd.Stderr = childStderr
	cmd.SysProcAttr = procAttr(slave != nil)

	t := &proc{
		id:         req.ID,
		rootfs:     spec.Root.Path,
		cwd:        spec.Process.Cwd,
		env:        spec.Process.Env,
		cmd:        cmd,
		stdin:      stdin,
		stdout:     stdout,
		stderr:     stderr,
		stdinPath:  req.Stdin,
		stdoutPath: req.Stdout,
		stderrPath: req.Stderr,
		master:     master,
		waitCh:     make(chan struct{}),
	}
	err = cmd.Start()
	if slave != nil {
		slave.Close()
	}
	if err != nil {
		if master != nil {
			master.Close()
		}
		return nil, fmt.Errorf("start container init: %w", err)
	}
	t.pid = cmd.Process.Pid
	s.tasks[req.ID] = t
	if master != nil {
		t.ptyDone = pumpPty(master, t.stdin, t.stdout)
	}
	go s.reap(ctx, t, req.ID, req.ID)

	os.Stderr.WriteString(fmt.Sprintf("shim Create: spawned init pid=%d\n", t.pid))
	return &taskapi.CreateTaskResponse{Pid: uint32(t.pid)}, nil
}

// spawn builds and starts an init-mode child for argv; tty starts it on a
// pty (stdin) as its controlling terminal.
func spawn(rootfs, cwd string, env, argv []string, stdin, stdout, stderr *os.File, tty bool) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	argv0 := append([]string{filepath.Base(self), initFlag}, argv...)
	_ = argv0
	cmd := exec.Command(self, append([]string{initFlag}, argv...)...)
	cmd.Dir = "/"
	cmd.Env = append([]string{
		envInitRootfs + "=" + rootfs,
		envInitCwd + "=" + cwd,
	}, env...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = procAttr(tty)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// reap waits for a process, records its exit status, publishes the exit
// event, and closes its stdio.
func (s *haikuTaskService) reap(ctx context.Context, t *proc, containerID, eventID string) {
	err := t.cmd.Wait()
	t.exitedAt = time.Now()
	if exitErr, ok := err.(*exec.ExitError); ok {
		t.exitStatus = uint32(exitErr.ExitCode())
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			t.exitStatus = uint32(128 + int(ws.Signal()))
		}
	} else if err != nil {
		t.exitStatus = 255
	}
	if eventID == containerID {
		s.killRemaining(containerID, t)
	}
	waitPty(t)
	t.exited = true
	close(t.waitCh)
	if s.publisher != nil {
		evt := &eventtypes.TaskExit{
			ContainerID: containerID,
			ID:          eventID,
			Pid:         uint32(t.pid),
			ExitStatus:  t.exitStatus,
			ExitedAt:    timestamppb.New(t.exitedAt),
		}
		// ctx is the Create or Exec request's, long cancelled by now; the
		// publisher retries with it, so it must outlive the request.
		_ = s.publisher.Publish(context.WithoutCancel(ctx), "/tasks/exit", evt)
	}
	if t.stdin != nil {
		t.stdin.Close()
	}
	if t.stdout != nil {
		t.stdout.Close()
	}
	if t.stderr != nil && t.stderr != t.stdout {
		t.stderr.Close()
	}
	if t.master != nil {
		t.master.Close()
	}
}

// killRemaining kills what is left of a container once its init exits, as
// Linux does when a pid namespace's init dies: the teams in the init's
// process group, and each exec process with its group.
func (s *haikuTaskService) killRemaining(containerID string, init *proc) {
	_ = syscall.Kill(-init.pid, syscall.SIGKILL)
	s.mu.Lock()
	var execs []*proc
	for _, p := range s.execs[containerID] {
		if !p.exited && p.cmd != nil && p.cmd.Process != nil {
			execs = append(execs, p)
		}
	}
	s.mu.Unlock()
	for _, p := range execs {
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		_ = p.cmd.Process.Kill()
	}
}

// findProc returns the init or exec process for a request.
func (s *haikuTaskService) findProc(containerID, execID string) *proc {
	if execID == "" {
		return s.tasks[containerID]
	}
	if m, ok := s.execs[containerID]; ok {
		return m[execID]
	}
	return nil
}

func (s *haikuTaskService) Start(ctx context.Context, req *taskapi.StartRequest) (*taskapi.StartResponse, error) {
	s.mu.Lock()
	t := s.findProc(req.ID, req.ExecID)
	s.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("task %s not found", req.ID)
	}
	t.started = true
	return &taskapi.StartResponse{Pid: uint32(t.pid)}, nil
}

func (s *haikuTaskService) State(ctx context.Context, req *taskapi.StateRequest) (*taskapi.StateResponse, error) {
	s.mu.Lock()
	t := s.findProc(req.ID, req.ExecID)
	s.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("task %s not found", req.ID)
	}
	status := tasktypes.Status_CREATED
	if t.exited {
		status = tasktypes.Status_STOPPED
	} else if t.started {
		status = tasktypes.Status_RUNNING
	}
	resp := &taskapi.StateResponse{
		ID:         req.ID,
		ExecID:     req.ExecID,
		Pid:        uint32(t.pid),
		Status:     status,
		Stdin:      t.stdinPath,
		Stdout:     t.stdoutPath,
		Stderr:     t.stderrPath,
		ExitStatus: t.exitStatus,
	}
	if t.exited {
		resp.ExitedAt = timestamppb.New(t.exitedAt)
	}
	return resp, nil
}

func (s *haikuTaskService) Wait(ctx context.Context, req *taskapi.WaitRequest) (*taskapi.WaitResponse, error) {
	s.mu.Lock()
	t := s.findProc(req.ID, req.ExecID)
	s.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("task %s not found", req.ID)
	}
	select {
	case <-t.waitCh:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &taskapi.WaitResponse{
		ExitStatus: t.exitStatus,
		ExitedAt:   timestamppb.New(t.exitedAt),
	}, nil
}

func (s *haikuTaskService) Kill(ctx context.Context, req *taskapi.KillRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	t := s.findProc(req.ID, req.ExecID)
	s.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("task %s not found", req.ID)
	}
	sig := syscall.Signal(req.Signal)
	if !t.exited && t.cmd.Process != nil {
		_ = syscall.Kill(-t.pid, sig)
		if err := t.cmd.Process.Signal(sig); err != nil {
			return nil, err
		}
	}
	return &emptypb.Empty{}, nil
}

func (s *haikuTaskService) Delete(ctx context.Context, req *taskapi.DeleteRequest) (*taskapi.DeleteResponse, error) {
	s.mu.Lock()
	var t *proc
	if req.ExecID == "" {
		t = s.tasks[req.ID]
		delete(s.tasks, req.ID)
		delete(s.execs, req.ID) // its execs went with it (see killRemaining)
	} else if m, ok := s.execs[req.ID]; ok {
		t = m[req.ExecID]
		delete(m, req.ExecID)
	}
	s.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("task %s not found", req.ID)
	}
	return &taskapi.DeleteResponse{
		Pid:        uint32(t.pid),
		ExitStatus: t.exitStatus,
		ExitedAt:   timestamppb.New(t.exitedAt),
	}, nil
}

func (s *haikuTaskService) Shutdown(ctx context.Context, req *taskapi.ShutdownRequest) (*emptypb.Empty, error) {
	// Exec processes belong to their container: once its init task is
	// deleted they are gone too (as in the runc shim).
	s.mu.Lock()
	remaining := len(s.tasks)
	s.mu.Unlock()
	if remaining == 0 || req.Now {
		go func() {
			time.Sleep(100 * time.Millisecond)
			os.Exit(0)
		}()
	}
	return &emptypb.Empty{}, nil
}

func (s *haikuTaskService) Connect(ctx context.Context, req *taskapi.ConnectRequest) (*taskapi.ConnectResponse, error) {
	return &taskapi.ConnectResponse{}, nil
}

// Exec runs an additional process in the container's rootfs, as an
// init-mode child with the exec process spec.
func (s *haikuTaskService) Exec(ctx context.Context, req *taskapi.ExecProcessRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	t := s.tasks[req.ID]
	s.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("task %s not found", req.ID)
	}
	if req.Spec == nil {
		return nil, fmt.Errorf("exec %s: no process spec", req.ExecID)
	}
	v, err := typeurl.UnmarshalAny(req.Spec)
	if err != nil {
		return nil, fmt.Errorf("exec %s: decode spec: %w", req.ExecID, err)
	}
	pspec, ok := v.(*specs.Process)
	if !ok {
		return nil, fmt.Errorf("exec %s: spec is %T, not an OCI process", req.ExecID, v)
	}
	if len(pspec.Args) == 0 {
		return nil, fmt.Errorf("exec %s: empty args", req.ExecID)
	}

	stdin, err := openFifo(req.Stdin, os.O_RDONLY)
	if err != nil {
		return nil, fmt.Errorf("open stdin fifo: %w", err)
	}
	stdout, err := openFifo(req.Stdout, os.O_WRONLY)
	if err != nil {
		return nil, fmt.Errorf("open stdout fifo: %w", err)
	}
	stderr := stdout
	if req.Stderr != "" && req.Stderr != req.Stdout {
		stderr, err = openFifo(req.Stderr, os.O_WRONLY)
		if err != nil {
			return nil, fmt.Errorf("open stderr fifo: %w", err)
		}
	}

	p := &proc{
		id:         req.ID,
		execID:     req.ExecID,
		rootfs:     t.rootfs,
		cwd:        pspec.Cwd,
		env:        pspec.Env,
		stdin:      stdin,
		stdout:     stdout,
		stderr:     stderr,
		stdinPath:  req.Stdin,
		stdoutPath: req.Stdout,
		stderrPath: req.Stderr,
		waitCh:     make(chan struct{}),
	}
	childStdin, childStdout, childStderr := stdin, stdout, stderr
	var slave *os.File
	if req.Terminal || pspec.Terminal {
		p.master, slave, err = openPty()
		if err != nil {
			return nil, fmt.Errorf("exec %s: allocate pty: %w", req.ExecID, err)
		}
		childStdin = slave
		childStdout = slave
		childStderr = slave
	}
	cmd, err := spawn(t.rootfs, pspec.Cwd, pspec.Env, pspec.Args, childStdin, childStdout, childStderr, slave != nil)
	if slave != nil {
		slave.Close()
	}
	if err != nil {
		if p.master != nil {
			p.master.Close()
		}
		return nil, fmt.Errorf("exec %s: %w", req.ExecID, err)
	}
	if p.master != nil {
		p.ptyDone = pumpPty(p.master, stdin, stdout)
	}
	p.cmd = cmd
	p.pid = cmd.Process.Pid
	p.started = true
	s.mu.Lock()
	if s.execs[req.ID] == nil {
		s.execs[req.ID] = make(map[string]*proc)
	}
	s.execs[req.ID][req.ExecID] = p
	s.mu.Unlock()
	go s.reap(ctx, p, req.ID, req.ExecID)
	return &emptypb.Empty{}, nil
}

func (s *haikuTaskService) ResizePty(ctx context.Context, req *taskapi.ResizePtyRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	t := s.findProc(req.ID, req.ExecID)
	s.mu.Unlock()
	if t == nil || t.master == nil {
		return &emptypb.Empty{}, nil
	}
	ws := haikuWinsize{Row: uint16(req.Height), Col: uint16(req.Width)}
	const tiocswinsz = 0x8000 + 13
	if err := syscall.Ioctl(int(t.master.Fd()), tiocswinsz, uintptr(unsafe.Pointer(&ws))); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *haikuTaskService) CloseIO(ctx context.Context, req *taskapi.CloseIORequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	t := s.findProc(req.ID, req.ExecID)
	s.mu.Unlock()
	if t != nil && req.Stdin && t.stdin != nil {
		_ = t.stdin.Close()
	}
	return &emptypb.Empty{}, nil
}

// Pids lists the container's processes: its init and exec processes and
// every team they started (docker top shows them all, as on Linux).
func (s *haikuTaskService) Pids(ctx context.Context, req *taskapi.PidsRequest) (*taskapi.PidsResponse, error) {
	s.mu.Lock()
	roots := s.rootTeams(req.ID)
	s.mu.Unlock()
	resp := &taskapi.PidsResponse{}
	for _, team := range teamTree(roots) {
		resp.Processes = append(resp.Processes, &taskpb.ProcessInfo{Pid: uint32(team)})
	}
	return resp, nil
}

// rootTeams returns the running init and exec processes of a container;
// s.mu must be held.
func (s *haikuTaskService) rootTeams(id string) []int32 {
	var roots []int32
	if t := s.tasks[id]; t != nil && !t.exited {
		roots = append(roots, int32(t.pid))
	}
	for _, p := range s.execs[id] {
		if !p.exited {
			roots = append(roots, int32(p.pid))
		}
	}
	return roots
}

func (s *haikuTaskService) Pause(ctx context.Context, req *taskapi.PauseRequest) (*emptypb.Empty, error) {
	return nil, fmt.Errorf("pause not supported on haiku")
}

func (s *haikuTaskService) Resume(ctx context.Context, req *taskapi.ResumeRequest) (*emptypb.Empty, error) {
	return nil, fmt.Errorf("resume not supported on haiku")
}

func (s *haikuTaskService) Checkpoint(ctx context.Context, req *taskapi.CheckpointTaskRequest) (*emptypb.Empty, error) {
	return nil, fmt.Errorf("checkpoint not supported on haiku")
}

func (s *haikuTaskService) Stats(ctx context.Context, req *taskapi.StatsRequest) (*taskapi.StatsResponse, error) {
	s.mu.Lock()
	t := s.tasks[req.ID]
	roots := s.rootTeams(req.ID)
	s.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("task %s not found", req.ID)
	}
	stats, err := typeurl.MarshalAnyToProto(teamMetrics(roots))
	if err != nil {
		return nil, err
	}
	return &taskapi.StatsResponse{Stats: stats}, nil
}

func (s *haikuTaskService) Update(ctx context.Context, req *taskapi.UpdateTaskRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

var _ taskapi.TTRPCTaskService = (*haikuTaskService)(nil)

func mustGetwd() string {
	d, err := os.Getwd()
	if err != nil {
		return "."
	}
	return d
}
