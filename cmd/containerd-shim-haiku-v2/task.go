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
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	eventtypes "github.com/containerd/containerd/api/events"
	taskpb "github.com/containerd/containerd/api/types/task"
	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/core/events"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// haikuTask is a running (or finished) container init process.
type haikuTask struct {
	id     string
	bundle string
	spec   *specs.Spec

	cmd     *exec.Cmd
	pid     int
	started bool

	exitStatus uint32
	exitedAt   time.Time
	exited     bool

	stdin  *os.File
	stdout *os.File
	stderr *os.File

	waitOnce sync.Once
	waitCh   chan struct{}
}

// haikuTaskService implements the containerd TaskService. Containers run
// as chrooted processes spawned by the shim's own init re-exec; there is no
// supervising runtime like runc.
type haikuTaskService struct {
	mu     sync.Mutex
	tasks  map[string]*haikuTask
	publisher events.Publisher
}

func newHaikuTaskService(publisher events.Publisher) *haikuTaskService {
	return &haikuTaskService{
		tasks:     make(map[string]*haikuTask),
		publisher: publisher,
	}
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

// openFifo opens a stdio fifo without blocking: like go-fifo, use
// O_NONBLOCK and a read/write handle, which never waits for a peer and
// works regardless of the order the daemon attaches its ends.
func openFifo(path string, _ int) (*os.File, error) {
	if path == "" {
		return nil, nil
	}
	return os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
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
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	t := &haikuTask{
		id:     req.ID,
		bundle: bundle,
		spec:   spec,
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		waitCh: make(chan struct{}),
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start container init: %w", err)
	}
	t.pid = cmd.Process.Pid
	s.tasks[req.ID] = t

	// Reap the child and record the exit status.
	go func() {
		err := cmd.Wait()
		t.exitedAt = time.Now()
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.exitStatus = uint32(exitErr.ExitCode())
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				t.exitStatus = uint32(128 + int(ws.Signal()))
			}
		} else if err != nil {
			t.exitStatus = 255
		}
		t.exited = true
		close(t.waitCh)
		if s.publisher != nil {
			evt := &eventtypes.TaskExit{
				ContainerID: req.ID,
				ID:          req.ID,
				Pid:         uint32(t.pid),
				ExitStatus:  t.exitStatus,
				ExitedAt:    timestamppb.New(t.exitedAt),
			}
			// Best effort: the Wait response also carries the status.
			_ = s.publisher.Publish(ctx, "/tasks/exit", evt)
		}
		if stdin != nil {
			stdin.Close()
		}
		if stdout != nil {
			stdout.Close()
		}
		if stderr != nil && stderr != stdout {
			stderr.Close()
		}
	}()

	os.Stderr.WriteString(fmt.Sprintf("shim Create: spawned init pid=%d\n", t.pid))
	return &taskapi.CreateTaskResponse{Pid: uint32(t.pid)}, nil
}

func (s *haikuTaskService) Start(ctx context.Context, req *taskapi.StartRequest) (*taskapi.StartResponse, error) {
	s.mu.Lock()
	t, ok := s.tasks[req.ID]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("task %s not found", req.ID)
	}
	if req.ExecID != "" {
		return nil, fmt.Errorf("exec not supported on haiku yet")
	}
	t.started = true
	return &taskapi.StartResponse{Pid: uint32(t.pid)}, nil
}

func (s *haikuTaskService) State(ctx context.Context, req *taskapi.StateRequest) (*taskapi.StateResponse, error) {
	s.mu.Lock()
	t, ok := s.tasks[req.ID]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("task %s not found", req.ID)
	}
	status := tasktypes.Status_CREATED
	if t.exited {
		status = tasktypes.Status_STOPPED
	} else if t.started {
		status = tasktypes.Status_RUNNING
	}
	resp := &taskapi.StateResponse{
		ID:         t.id,
		Pid:        uint32(t.pid),
		Status:     status,
		Terminal:   t.spec.Process.Terminal,
		ExitStatus: t.exitStatus,
	}
	if t.exited {
		resp.ExitedAt = timestamppb.New(t.exitedAt)
	}
	return resp, nil
}

func (s *haikuTaskService) Wait(ctx context.Context, req *taskapi.WaitRequest) (*taskapi.WaitResponse, error) {
	s.mu.Lock()
	t, ok := s.tasks[req.ID]
	s.mu.Unlock()
	if !ok {
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
	t, ok := s.tasks[req.ID]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("task %s not found", req.ID)
	}
	sig := syscall.Signal(req.Signal)
	if !t.exited && t.cmd.Process != nil {
		// Signal the process group so children of the entrypoint die too.
		_ = syscall.Kill(-t.pid, sig)
		if err := t.cmd.Process.Signal(sig); err != nil {
			return nil, err
		}
	}
	return &emptypb.Empty{}, nil
}

func (s *haikuTaskService) Delete(ctx context.Context, req *taskapi.DeleteRequest) (*taskapi.DeleteResponse, error) {
	s.mu.Lock()
	t, ok := s.tasks[req.ID]
	if ok {
		delete(s.tasks, req.ID)
	}
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("task %s not found", req.ID)
	}
	return &taskapi.DeleteResponse{
		Pid:        uint32(t.pid),
		ExitStatus: t.exitStatus,
		ExitedAt:   timestamppb.New(t.exitedAt),
	}, nil
}

func (s *haikuTaskService) Shutdown(ctx context.Context, req *taskapi.ShutdownRequest) (*emptypb.Empty, error) {
	// Exit the shim once the last task is gone; containerd sends
	// Shutdown after Delete when stopping the shim.
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

func (s *haikuTaskService) Exec(ctx context.Context, req *taskapi.ExecProcessRequest) (*emptypb.Empty, error) {
	return nil, fmt.Errorf("exec not supported on haiku yet")
}

func (s *haikuTaskService) ResizePty(ctx context.Context, req *taskapi.ResizePtyRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (s *haikuTaskService) CloseIO(ctx context.Context, req *taskapi.CloseIORequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	t, ok := s.tasks[req.ID]
	s.mu.Unlock()
	if ok && req.Stdin && t.stdin != nil {
		_ = t.stdin.Close()
	}
	return &emptypb.Empty{}, nil
}

func (s *haikuTaskService) Pids(ctx context.Context, req *taskapi.PidsRequest) (*taskapi.PidsResponse, error) {
	s.mu.Lock()
	t, ok := s.tasks[req.ID]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("task %s not found", req.ID)
	}
	return &taskapi.PidsResponse{
		Processes: []*taskpb.ProcessInfo{{Pid: uint32(t.pid)}},
	}, nil
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
	return &taskapi.StatsResponse{Stats: &anypb.Any{}}, nil
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
