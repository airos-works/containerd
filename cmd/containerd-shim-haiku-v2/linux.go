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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	eventtypes "github.com/containerd/containerd/api/events"
	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	_ "github.com/containerd/containerd/v2/core/runtime" // the spec's type URL
	"github.com/containerd/containerd/v2/pkg/linuxvm"
	"github.com/containerd/ttrpc"
	typeurl "github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Containers from Linux images run in air/OS's Linux VM. The shim forwards
// their TaskService requests to the VM's agent, carries their stdio
// between the Engine's fifos and agent streams, publishes their exits, and
// listens on the host for their published ports.

const linuxVMSocket = "/var/run/containerd-linuxvm.sock"

func isLinuxSpec(spec *specs.Spec) bool {
	return spec.Annotations[linuxvm.AnnotationPlatform] == "linux"
}

type linuxProc struct {
	execID string
	stdio  [3]string // the Engine's fifo paths
	pid    uint32
	output sync.WaitGroup // stdout and stderr copied to the fifos
	files  []*os.File

	mu         sync.Mutex
	exited     bool
	exitStatus uint32
	exitedAt   time.Time
	done       chan struct{}
}

type linuxContainer struct {
	id      string
	client  *ttrpc.Client
	tasks   taskapi.TTRPCTaskService
	init    *linuxProc
	execs   map[string]*linuxProc
	closers []io.Closer // port listeners
}

// startLinuxVM runs the supervisor if its socket does not answer.
func startLinuxVM() error {
	if c, err := net.DialTimeout("unix", linuxVMSocket, time.Second); err == nil {
		c.Close()
		return nil
	}
	exe := "containerd-linuxvm"
	if self, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(self), exe); fileExists(p) {
			exe = p
		}
	}
	if p, err := exec.LookPath(exe); err == nil {
		exe = p
	}
	logf, _ := os.OpenFile("/var/log/containerd-linuxvm.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	cmd := exec.Command(exe, "-socket", linuxVMSocket)
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if logf != nil {
		cmd.Stdout, cmd.Stderr = logf, logf
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the Linux VM (%s): %w", exe, err)
	}
	go func() { _ = cmd.Wait() }() // another shim may own it later; reap if we can
	for i := 0; i < 100; i++ {
		if c, err := net.DialTimeout("unix", linuxVMSocket, time.Second); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the Linux VM supervisor did not start (see /var/log/containerd-linuxvm.log)")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func ioToken() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// hostToVM is a host path as the VM sees it.
func hostToVM(p string) (string, error) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(real) {
		if real, err = filepath.Abs(real); err != nil {
			return "", err
		}
	}
	return linuxvm.VMPath(real), nil
}

// vmSpec rewrites the Engine's spec for the VM: the root filesystem and the
// bind mounts' sources become paths on the VM's share of the host.
func vmSpec(spec *specs.Spec) (*specs.Spec, error) {
	out := *spec
	root := *spec.Root
	p, err := hostToVM(root.Path)
	if err != nil {
		return nil, fmt.Errorf("root filesystem: %w", err)
	}
	root.Path = p
	out.Root = &root
	out.Mounts = nil
	for _, m := range spec.Mounts {
		if isBind(m) {
			src, err := hostToVM(m.Source)
			if err != nil {
				return nil, fmt.Errorf("mount %s: %w", m.Destination, err)
			}
			m.Source = src
			m.Type = "bind"
		}
		out.Mounts = append(out.Mounts, m)
	}
	return &out, nil
}

func isBind(m specs.Mount) bool {
	if m.Type == "bind" {
		return true
	}
	for _, o := range m.Options {
		if o == "bind" || o == "rbind" {
			return true
		}
	}
	return false
}

// openStdio connects the Engine's fifos to agent streams and returns the
// names the agent knows them by.
func openStdio(p *linuxProc, stdin, stdout, stderr string) (names [3]string, err error) {
	defer func() {
		if err != nil {
			for _, f := range p.files {
				f.Close()
			}
		}
	}()
	open := func(fifo string, flag int) (*os.File, net.Conn, string, error) {
		f, err := openFifo(fifo, flag)
		if err != nil {
			return nil, nil, "", err
		}
		tok := ioToken()
		st, err := linuxvm.Dial(linuxVMSocket, linuxvm.HeaderIO+" "+tok)
		if err != nil {
			f.Close()
			return nil, nil, "", err
		}
		p.files = append(p.files, f)
		return f, st, linuxvm.IOPrefix + tok, nil
	}
	if stdin != "" {
		f, st, name, err := open(stdin, os.O_RDONLY)
		if err != nil {
			return names, fmt.Errorf("stdin: %w", err)
		}
		names[0] = name
		go func() {
			_, _ = io.Copy(st, f)
			if cw, ok := st.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
		}()
	}
	copyOut := func(f *os.File, st net.Conn) {
		defer p.output.Done()
		_, _ = io.Copy(f, st)
		st.Close()
	}
	if stdout != "" {
		f, st, name, err := open(stdout, os.O_WRONLY)
		if err != nil {
			return names, fmt.Errorf("stdout: %w", err)
		}
		names[1] = name
		p.output.Add(1)
		go copyOut(f, st)
	}
	if stderr != "" && stderr == stdout {
		names[2] = names[1]
	} else if stderr != "" {
		f, st, name, err := open(stderr, os.O_WRONLY)
		if err != nil {
			return names, fmt.Errorf("stderr: %w", err)
		}
		names[2] = name
		p.output.Add(1)
		go copyOut(f, st)
	}
	return names, nil
}

func (s *haikuTaskService) createLinux(ctx context.Context, req *taskapi.CreateTaskRequest, spec *specs.Spec) (*taskapi.CreateTaskResponse, error) {
	if err := startLinuxVM(); err != nil {
		return nil, err
	}
	vspec, err := vmSpec(spec)
	if err != nil {
		return nil, err
	}
	opts, err := typeurl.MarshalAnyToProto(vspec)
	if err != nil {
		return nil, err
	}
	conn, err := linuxvm.Dial(linuxVMSocket, linuxvm.HeaderTTRPC)
	if err != nil {
		return nil, err
	}
	client := ttrpc.NewClient(conn)
	c := &linuxContainer{
		id:     req.ID,
		client: client,
		tasks:  taskapi.NewTTRPCTaskClient(client),
		execs:  make(map[string]*linuxProc),
	}
	fail := func(err error) (*taskapi.CreateTaskResponse, error) {
		c.closeAll()
		return nil, err
	}

	p := &linuxProc{stdio: [3]string{req.Stdin, req.Stdout, req.Stderr}, done: make(chan struct{})}
	names, err := openStdio(p, req.Stdin, req.Stdout, req.Stderr)
	if err != nil {
		return fail(err)
	}
	c.init = p
	if err := c.publishPorts(spec); err != nil {
		return fail(err)
	}
	resp, err := c.tasks.Create(ctx, &taskapi.CreateTaskRequest{
		ID: req.ID, Terminal: req.Terminal,
		Stdin: names[0], Stdout: names[1], Stderr: names[2],
		Options: opts,
	})
	if err != nil {
		return fail(err)
	}
	p.pid = resp.Pid
	s.mu.Lock()
	s.linux[req.ID] = c
	s.mu.Unlock()
	go s.waitLinux(ctx, c, p)
	return resp, nil
}

func (c *linuxContainer) closeAll() {
	for _, cl := range c.closers {
		cl.Close()
	}
	c.closers = nil
	if c.init != nil {
		for _, f := range c.init.files {
			f.Close()
		}
	}
	if c.client != nil {
		c.client.Close()
	}
}

// waitLinux publishes a process's exit once the agent reports it and its
// output has reached the Engine.
func (s *haikuTaskService) waitLinux(ctx context.Context, c *linuxContainer, p *linuxProc) {
	resp, err := c.tasks.Wait(context.Background(), &taskapi.WaitRequest{ID: c.id, ExecID: p.execID})
	status, at := uint32(255), time.Now()
	if err == nil {
		status = resp.ExitStatus
		if resp.ExitedAt != nil {
			at = resp.ExitedAt.AsTime()
		}
	}
	waited := make(chan struct{})
	go func() {
		p.output.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
	}
	for _, f := range p.files {
		f.Close()
	}
	p.mu.Lock()
	p.exited, p.exitStatus, p.exitedAt = true, status, at
	p.mu.Unlock()
	close(p.done)
	if p.execID == "" {
		for _, cl := range c.closers {
			cl.Close()
		}
	}
	if s.publisher != nil {
		_ = s.publisher.Publish(context.WithoutCancel(ctx), "/tasks/exit", &eventtypes.TaskExit{
			ContainerID: c.id,
			ID:          firstNonEmpty(p.execID, c.id),
			Pid:         p.pid,
			ExitStatus:  status,
			ExitedAt:    timestamppb.New(at),
		})
	}
}

// linuxSignal is the Linux number of a Haiku signal: the Engine and
// containerd speak the host's numbering, the VM Linux's.
func linuxSignal(sig uint32) uint32 {
	m := map[uint32]uint32{
		5: 17, 7: 13, 10: 19, 12: 18, 13: 20, 16: 21, 17: 22, 18: 10, 19: 12,
		20: 28, 22: 5, 23: 29, 24: 27, 25: 31, 26: 23, 27: 26, 28: 24, 29: 25, 30: 7,
	}
	if l, ok := m[sig]; ok {
		return l
	}
	return sig
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// linuxContainerFor returns the Linux container a request is for, if any.
func (s *haikuTaskService) linuxContainerFor(id string) *linuxContainer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.linux[id]
}

func (c *linuxContainer) proc(execID string) *linuxProc {
	if execID == "" {
		return c.init
	}
	return c.execs[execID]
}

func (s *haikuTaskService) execLinux(ctx context.Context, c *linuxContainer, req *taskapi.ExecProcessRequest) (*emptypb.Empty, error) {
	p := &linuxProc{execID: req.ExecID, stdio: [3]string{req.Stdin, req.Stdout, req.Stderr}, done: make(chan struct{})}
	names, err := openStdio(p, req.Stdin, req.Stdout, req.Stderr)
	if err != nil {
		return nil, err
	}
	fwd := &taskapi.ExecProcessRequest{
		ID: req.ID, ExecID: req.ExecID, Terminal: req.Terminal, Spec: req.Spec,
		Stdin: names[0], Stdout: names[1], Stderr: names[2],
	}
	if _, err := c.tasks.Exec(ctx, fwd); err != nil {
		for _, f := range p.files {
			f.Close()
		}
		return nil, err
	}
	s.mu.Lock()
	c.execs[req.ExecID] = p
	s.mu.Unlock()
	return &emptypb.Empty{}, nil
}

func (s *haikuTaskService) startLinux(ctx context.Context, c *linuxContainer, req *taskapi.StartRequest) (*taskapi.StartResponse, error) {
	resp, err := c.tasks.Start(ctx, req)
	if err != nil {
		return nil, err
	}
	if req.ExecID != "" {
		s.mu.Lock()
		p := c.execs[req.ExecID]
		s.mu.Unlock()
		if p != nil {
			p.pid = resp.Pid
			go s.waitLinux(ctx, c, p)
		}
	}
	return resp, nil
}

func (s *haikuTaskService) stateLinux(ctx context.Context, c *linuxContainer, req *taskapi.StateRequest) (*taskapi.StateResponse, error) {
	resp, err := c.tasks.State(ctx, req)
	s.mu.Lock()
	p := c.proc(req.ExecID)
	s.mu.Unlock()
	if p == nil {
		if err != nil {
			return nil, err
		}
		return resp, nil
	}
	if err != nil {
		// the VM is gone: what we know
		p.mu.Lock()
		defer p.mu.Unlock()
		resp = &taskapi.StateResponse{ID: req.ID, ExecID: req.ExecID, Pid: p.pid, ExitStatus: p.exitStatus}
		if p.exited {
			resp.ExitedAt = timestamppb.New(p.exitedAt)
		}
	}
	resp.Stdin, resp.Stdout, resp.Stderr = p.stdio[0], p.stdio[1], p.stdio[2]
	return resp, nil
}

func (s *haikuTaskService) waitLinuxReq(ctx context.Context, c *linuxContainer, req *taskapi.WaitRequest) (*taskapi.WaitResponse, error) {
	s.mu.Lock()
	p := c.proc(req.ExecID)
	s.mu.Unlock()
	if p == nil {
		return nil, fmt.Errorf("process %s/%s not found", req.ID, req.ExecID)
	}
	select {
	case <-p.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return &taskapi.WaitResponse{ExitStatus: p.exitStatus, ExitedAt: timestamppb.New(p.exitedAt)}, nil
}

func (s *haikuTaskService) deleteLinux(ctx context.Context, c *linuxContainer, req *taskapi.DeleteRequest) (*taskapi.DeleteResponse, error) {
	resp, err := c.tasks.Delete(ctx, req)
	s.mu.Lock()
	p := c.proc(req.ExecID)
	if req.ExecID == "" {
		delete(s.linux, req.ID)
	} else {
		delete(c.execs, req.ExecID)
	}
	s.mu.Unlock()
	if req.ExecID == "" {
		c.closeAll()
	}
	if err != nil && p != nil {
		// the VM restarted and lost it: answer from what we recorded
		p.mu.Lock()
		defer p.mu.Unlock()
		return &taskapi.DeleteResponse{Pid: p.pid, ExitStatus: p.exitStatus, ExitedAt: timestamppb.New(p.exitedAt)}, nil
	}
	return resp, err
}

// publishPorts listens on the host for the container's published ports and
// forwards each connection (or UDP peer) to the agent, which dials the
// container.
func (c *linuxContainer) publishPorts(spec *specs.Spec) error {
	b := spec.Annotations[linuxvm.AnnotationPorts]
	if b == "" {
		return nil
	}
	var ports []linuxvm.PortBinding
	if err := json.Unmarshal([]byte(b), &ports); err != nil {
		return fmt.Errorf("ports annotation: %w", err)
	}
	for _, pb := range ports {
		addr := net.JoinHostPort(pb.HostIP, strconv.Itoa(pb.HostPort))
		dial := fmt.Sprintf("%s %s %s %d", linuxvm.HeaderDial, pb.Proto, c.id, pb.ContainerPort)
		switch pb.Proto {
		case "udp":
			pc, err := net.ListenPacket("udp", addr)
			if err != nil {
				return fmt.Errorf("publish %s/udp: %w", addr, err)
			}
			c.closers = append(c.closers, pc)
			go forwardUDP(pc, dial)
		default:
			l, err := net.Listen("tcp", addr)
			if err != nil {
				return fmt.Errorf("publish %s/tcp: %w", addr, err)
			}
			c.closers = append(c.closers, l)
			go forwardTCP(l, dial)
		}
	}
	return nil
}

func forwardTCP(l net.Listener, dial string) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			st, err := linuxvm.Dial(linuxVMSocket, dial)
			if err != nil {
				return
			}
			defer st.Close()
			done := make(chan struct{})
			go func() {
				_, _ = io.Copy(st, conn)
				if cw, ok := st.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
				close(done)
			}()
			_, _ = io.Copy(conn, st)
			if tc, ok := conn.(*net.TCPConn); ok {
				_ = tc.CloseWrite()
			}
			select {
			case <-done:
			case <-time.After(time.Minute):
			}
		}()
	}
}

// forwardUDP keeps one agent stream per remote peer; datagrams travel
// length-prefixed.
func forwardUDP(pc net.PacketConn, dial string) {
	var mu sync.Mutex
	peers := map[string]net.Conn{}
	buf := make([]byte, 65535)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			mu.Lock()
			for _, st := range peers {
				st.Close()
			}
			mu.Unlock()
			return
		}
		key := from.String()
		mu.Lock()
		st := peers[key]
		mu.Unlock()
		if st == nil {
			if st, err = linuxvm.Dial(linuxVMSocket, dial); err != nil {
				continue
			}
			mu.Lock()
			peers[key] = st
			mu.Unlock()
			go func(st net.Conn, to net.Addr) {
				defer func() {
					mu.Lock()
					delete(peers, to.String())
					mu.Unlock()
					st.Close()
				}()
				var lb [2]byte
				for {
					if _, err := io.ReadFull(st, lb[:]); err != nil {
						return
					}
					d := make([]byte, int(lb[0])<<8|int(lb[1]))
					if _, err := io.ReadFull(st, d); err != nil {
						return
					}
					_, _ = pc.WriteTo(d, to)
				}
			}(st, from)
		}
		_, _ = st.Write(append([]byte{byte(n >> 8), byte(n)}, buf[:n]...))
	}
}
