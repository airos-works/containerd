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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/cgroups/v3/cgroup2"
	"github.com/containerd/console"
	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	taskpb "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/contrib/seccomp"
	"github.com/containerd/containerd/v2/core/containers"
	_ "github.com/containerd/containerd/v2/core/runtime" // the spec's type URL
	"github.com/containerd/containerd/v2/pkg/linuxvm"
	"github.com/containerd/containerd/v2/pkg/linuxvm/mux"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/containerd/v2/pkg/sys/reaper"
	runc "github.com/containerd/go-runc"
	"github.com/containerd/ttrpc"
	typeurl "github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const bundlesDir = "/run/linuxvm/bundles"

type process struct {
	id, execID string
	pid        int
	terminal   bool
	spec       *specs.Process // an exec's, until it starts

	stdin              io.WriteCloser // the process's stdin: a pipe or the console
	stdinSt            *mux.Stream
	stdoutSt, stderrSt *mux.Stream
	stdinPath          string
	stdoutPath         string
	stderrPath         string
	rio                runc.IO
	console            console.Console
	output             sync.WaitGroup

	started    bool
	exited     bool
	exitStatus uint32
	exitedAt   time.Time
	done       chan struct{}
}

type container struct {
	id      string
	bundle  string
	spec    *specs.Spec
	network linuxvm.Network
	init    *process
	execs   map[string]*process
}

type taskService struct {
	mu         sync.Mutex
	containers map[string]*container
	byPid      map[int]*process
	early      map[int]int // exits seen before their pid was known
	io         *ioStreams
	nets       *networks
}

func newRunc() *runc.Runc {
	return &runc.Runc{
		Command:      "runc",
		Root:         "/run/runc",
		PdeathSignal: unix.SIGKILL,
	}
}

func newTaskService() *taskService {
	s := &taskService{
		containers: make(map[string]*container),
		byPid:      make(map[int]*process),
		early:      make(map[int]int),
		io:         newIOStreams(),
		nets:       newNetworks(),
	}
	_ = os.MkdirAll(bundlesDir, 0o711)
	go s.handleExits(reaper.Default.Subscribe())
	return s
}

func (s *taskService) register(server *ttrpc.Server) {
	taskapi.RegisterTTRPCTaskService(server, s)
}

func (s *taskService) handleExits(exits chan runc.Exit) {
	for e := range exits {
		s.mu.Lock()
		p := s.byPid[e.Pid]
		if p == nil {
			s.early[e.Pid] = e.Status
			if len(s.early) > 4096 {
				s.early = make(map[int]int)
			}
			s.mu.Unlock()
			continue
		}
		delete(s.byPid, e.Pid)
		s.mu.Unlock()
		go s.finish(p, e.Status)
	}
}

// track records a process's pid; s.mu is held. An exit that came first is
// handled at once.
func (s *taskService) track(p *process) {
	if status, ok := s.early[p.pid]; ok {
		delete(s.early, p.pid)
		go s.finish(p, status)
		return
	}
	s.byPid[p.pid] = p
}

// finish records an exit once the process's output reached the host.
func (s *taskService) finish(p *process, status int) {
	waited := make(chan struct{})
	go func() {
		p.output.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		// a background process still holds the terminal or the pipes
	}
	s.mu.Lock()
	p.exited = true
	p.exitStatus = uint32(status)
	p.exitedAt = time.Now()
	s.mu.Unlock()
	if p.console != nil {
		_ = p.console.Close()
	}
	if p.rio != nil {
		_ = p.rio.Close()
	}
	if p.execID == "" {
		syncHost()
	}
	for _, st := range []*mux.Stream{p.stdinSt, p.stdoutSt, p.stderrSt} {
		if st != nil {
			_ = st.CloseWrite()
		}
	}
	close(p.done)
}

func (s *taskService) find(id, execID string) (*container, *process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.containers[id]
	if c == nil {
		return nil, nil, fmt.Errorf("container %s: not found", id)
	}
	if execID == "" {
		return c, c.init, nil
	}
	p := c.execs[execID]
	if p == nil {
		return nil, nil, fmt.Errorf("exec %s in %s: not found", execID, id)
	}
	return c, p, nil
}

func (s *taskService) Create(ctx context.Context, req *taskapi.CreateTaskRequest) (_ *taskapi.CreateTaskResponse, retErr error) {
	if req.Options == nil {
		return nil, fmt.Errorf("create %s: no spec", req.ID)
	}
	v, err := typeurl.UnmarshalAny(req.Options)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", req.ID, err)
	}
	spec, ok := v.(*specs.Spec)
	if !ok {
		return nil, fmt.Errorf("create %s: options are %T, not a spec", req.ID, v)
	}
	s.mu.Lock()
	_, exists := s.containers[req.ID]
	s.mu.Unlock()
	if exists {
		return nil, fmt.Errorf("container %s already exists", req.ID)
	}

	bundle := filepath.Join(bundlesDir, req.ID)
	_ = os.RemoveAll(bundle)
	if err := os.MkdirAll(bundle, 0o711); err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = os.RemoveAll(bundle)
		}
	}()

	// The stdio streams first: they are already here, and wait to be claimed.
	p := &process{id: req.ID, terminal: req.Terminal || spec.Process.Terminal, done: make(chan struct{}),
		stdinPath: req.Stdin, stdoutPath: req.Stdout, stderrPath: req.Stderr}
	if err := s.claimIO(ctx, p, req.Stdin, req.Stdout, req.Stderr); err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			p.closeIO()
		}
	}()

	c := &container{id: req.ID, bundle: bundle, spec: spec, execs: make(map[string]*process)}
	if err := s.prepareSpec(ctx, c); err != nil {
		return nil, fmt.Errorf("create %s: %w", req.ID, err)
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), b, 0o600); err != nil {
		return nil, err
	}

	r := newRunc()
	r.Log, r.LogFormat = filepath.Join(bundle, "log.json"), runc.JSON
	opts := &runc.CreateOpts{PidFile: filepath.Join(bundle, "init.pid")}
	var sock *runc.Socket
	if p.terminal {
		if sock, err = runc.NewTempConsoleSocket(); err != nil {
			return nil, err
		}
		defer sock.Close()
		opts.ConsoleSocket = sock
	} else if p.rio, err = runc.NewPipeIO(int(spec.Process.User.UID), int(spec.Process.User.GID)); err != nil {
		return nil, err
	} else {
		opts.IO = p.rio
	}
	if err := r.Create(ctx, req.ID, bundle, opts); err != nil {
		return nil, fmt.Errorf("runc create: %w%s", err, runcLog(bundle))
	}
	if err := s.startIO(p, sock); err != nil {
		_ = r.Delete(context.Background(), req.ID, &runc.DeleteOpts{Force: true})
		return nil, err
	}
	pid, err := runc.ReadPidFile(opts.PidFile)
	if err != nil {
		_ = r.Delete(context.Background(), req.ID, &runc.DeleteOpts{Force: true})
		return nil, err
	}
	p.pid = pid

	if c.network.Mode == "bridge" && len(c.network.Networks) > 0 {
		names := []string{}
		if c.network.Hostname != "" {
			names = append(names, c.network.Hostname)
		}
		eps, err := s.nets.attach(req.ID, pid, c.network, names)
		if err != nil {
			_ = r.Delete(context.Background(), req.ID, &runc.DeleteOpts{Force: true})
			return nil, err
		}
		s.writeHosts(c, eps[0].ip)
	} else {
		s.writeHosts(c, nil)
	}

	c.init = p
	s.mu.Lock()
	s.containers[req.ID] = c
	s.track(p)
	s.mu.Unlock()
	return &taskapi.CreateTaskResponse{Pid: uint32(pid)}, nil
}

// cachedPath is a path under HostMount on CachedMount.
func cachedPath(p string) string {
	if rest, ok := strings.CutPrefix(p, linuxvm.HostMount); ok && (rest == "" || rest[0] == '/') {
		return linuxvm.CachedMount + rest
	}
	return p
}

// syncHost writes what containers changed through the cached mount back
// to the host, so the Engine reads it (docker cp, commit, export).
func syncHost() {
	f, err := os.Open(linuxvm.CachedMount)
	if err != nil {
		return
	}
	_ = unix.Syncfs(int(f.Fd()))
	f.Close()
}

// runcLog is the last error runc logged for a bundle.
func runcLog(bundle string) string {
	var last string
	for _, line := range readLines(filepath.Join(bundle, "log.json")) {
		var entry struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Level == "error" {
			last = entry.Msg
		}
	}
	if last == "" {
		return ""
	}
	return ": " + last
}

// prepareSpec finishes the Engine's spec for this VM: the user from the
// container's own passwd file, cgroups, seccomp, privileges, the network
// namespace, and the DNS and hosts files.
func (s *taskService) prepareSpec(ctx context.Context, c *container) error {
	spec := c.spec
	if spec.Root == nil || !linuxvm.IsVMPath(spec.Root.Path) {
		return fmt.Errorf("root filesystem is not on the host share")
	}
	if spec.Linux == nil {
		spec.Linux = &specs.Linux{}
	}
	if spec.Process == nil {
		return fmt.Errorf("no process")
	}
	ctx = namespaces.WithNamespace(ctx, "airos")
	ctr := &containers.Container{ID: c.id}
	var opts []oci.SpecOpts
	if u := spec.Annotations[linuxvm.AnnotationUser]; u != "" {
		opts = append(opts, oci.WithUser(u), oci.WithAdditionalGIDs(u))
	} else {
		opts = append(opts, oci.WithAdditionalGIDs("root"))
	}
	privileged := spec.Annotations[linuxvm.AnnotationPrivileged] == "true"
	if privileged {
		opts = append(opts, oci.WithPrivileged, oci.WithAllDevicesAllowed, oci.WithHostDevices,
			func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
				s.Linux.MaskedPaths = nil
				s.Linux.ReadonlyPaths = nil
				return nil
			})
	} else if spec.Linux.Seccomp == nil {
		opts = append(opts, seccomp.WithDefaultProfile())
	}
	if err := oci.ApplyOpts(ctx, nil, ctr, spec, opts...); err != nil {
		return err
	}
	spec.Linux.CgroupsPath = "/airos/" + c.id
	// A private cgroup namespace, as Docker gives containers on cgroup v2:
	// the container sees its own cgroup as the root of /sys/fs/cgroup.
	hasCgroupNS := false
	for _, ns := range spec.Linux.Namespaces {
		hasCgroupNS = hasCgroupNS || ns.Type == specs.CgroupNamespace
	}
	if !hasCgroupNS {
		spec.Linux.Namespaces = append(spec.Linux.Namespaces, specs.LinuxNamespace{Type: specs.CgroupNamespace})
	}

	// The root filesystem and what is under the Engine's data root
	// (volumes) come from the cached mount of the host's files.
	spec.Root.Path = cachedPath(spec.Root.Path)
	if dr := spec.Annotations[linuxvm.AnnotationDataRoot]; dr != "" {
		under := linuxvm.VMPath(dr) + "/"
		for i, m := range spec.Mounts {
			if strings.HasPrefix(m.Source, under) {
				spec.Mounts[i].Source = cachedPath(m.Source)
			}
		}
	}

	if b := spec.Annotations[linuxvm.AnnotationNetwork]; b != "" {
		if err := json.Unmarshal([]byte(b), &c.network); err != nil {
			return fmt.Errorf("network annotation: %w", err)
		}
	}
	if c.network.Mode == "" {
		c.network.Mode = "bridge"
		c.network.Networks = []linuxvm.Endpoint{{Name: "bridge"}}
	}
	setNetNS := func(path string, keep bool) {
		var nss []specs.LinuxNamespace
		for _, ns := range spec.Linux.Namespaces {
			if ns.Type != specs.NetworkNamespace {
				nss = append(nss, ns)
			}
		}
		if keep {
			nss = append(nss, specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: path})
		}
		spec.Linux.Namespaces = nss
	}
	switch {
	case c.network.Mode == "host":
		setNetNS("", false)
	case strings.HasPrefix(c.network.Mode, "container:"):
		other := strings.TrimPrefix(c.network.Mode, "container:")
		s.mu.Lock()
		oc := s.containers[other]
		s.mu.Unlock()
		if oc == nil || oc.init == nil || oc.init.exited {
			return fmt.Errorf("network container %s is not running", other)
		}
		setNetNS(fmt.Sprintf("/proc/%d/ns/net", oc.init.pid), true)
	default:
		setNetNS("", true)
	}

	// /etc/resolv.conf: the Engine's (the host's servers) becomes the
	// upstream of the network's DNS server, which the container uses.
	for i, m := range spec.Mounts {
		switch m.Destination {
		case "/etc/resolv.conf":
			local := filepath.Join(c.bundle, "resolv.conf")
			if err := os.WriteFile(local, s.resolvConf(c, m.Source), 0o644); err == nil {
				spec.Mounts[i].Source = local
			}
		case "/etc/hosts":
			local := filepath.Join(c.bundle, "hosts")
			b, _ := os.ReadFile(m.Source)
			if err := os.WriteFile(local, b, 0o644); err == nil {
				spec.Mounts[i].Source = local
			}
		}
	}
	return nil
}

func (s *taskService) resolvConf(c *container, hostFile string) []byte {
	var servers, other []string
	for _, line := range readLines(hostFile) {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			servers = append(servers, f[1])
			continue
		}
		if len(f) > 0 && (f[0] == "search" || f[0] == "options" || f[0] == "domain") {
			other = append(other, line)
		}
	}
	s.nets.setUpstreams(servers)
	var b strings.Builder
	if c.network.Mode == "bridge" && len(c.network.Networks) > 0 {
		s.nets.mu.Lock()
		bn, err := s.nets.network(c.network.Networks[0].Name)
		s.nets.mu.Unlock()
		if err == nil {
			fmt.Fprintf(&b, "nameserver %s\n", bn.gw)
		}
	} else {
		for _, sv := range servers {
			fmt.Fprintf(&b, "nameserver %s\n", sv)
		}
		fmt.Fprintf(&b, "nameserver %s\n", vmDNS)
	}
	for _, l := range other {
		b.WriteString(l + "\n")
	}
	return []byte(b.String())
}

// writeHosts adds the container's address to its hosts file, in place
// (the file is already bind mounted).
func (s *taskService) writeHosts(c *container, ip net.IP) {
	if ip == nil || c.network.Hostname == "" {
		return
	}
	path := filepath.Join(c.bundle, "hosts")
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	line := fmt.Sprintf("%s\t%s\n", ip, c.network.Hostname)
	_ = os.WriteFile(path, append(b, line...), 0o644)
}

// claimIO takes the request's stdio streams.
func (s *taskService) claimIO(ctx context.Context, p *process, stdin, stdout, stderr string) error {
	var err error
	if p.stdinSt, err = s.io.claim(ctx, stdin); err != nil {
		return err
	}
	if p.stdoutSt, err = s.io.claim(ctx, stdout); err != nil {
		p.closeIO()
		return err
	}
	if stderr != "" && stderr == stdout {
		p.stderrSt = p.stdoutSt
	} else if p.stderrSt, err = s.io.claim(ctx, stderr); err != nil {
		p.closeIO()
		return err
	}
	return nil
}

func (p *process) closeIO() {
	for _, st := range []*mux.Stream{p.stdinSt, p.stdoutSt, p.stderrSt} {
		if st != nil {
			st.Close()
		}
	}
	if p.rio != nil {
		_ = p.rio.Close()
	}
}

// startIO copies between the streams and the process once it exists.
func (s *taskService) startIO(p *process, sock *runc.Socket) error {
	if sock != nil {
		con, err := sock.ReceiveMaster()
		if err != nil {
			p.closeIO()
			return fmt.Errorf("receive the console: %w", err)
		}
		p.console = con
		p.stdin = con
		if p.stdinSt != nil {
			go func() { _, _ = io.Copy(con, p.stdinSt) }()
		}
		if p.stdoutSt != nil {
			p.output.Add(1)
			go func() {
				defer p.output.Done()
				_, _ = io.Copy(p.stdoutSt, con)
			}()
		}
		return nil
	}
	if sc, ok := p.rio.(runc.StartCloser); ok {
		_ = sc.CloseAfterStart()
	}
	p.stdin = p.rio.Stdin()
	if p.stdinSt != nil {
		go func() {
			_, _ = io.Copy(p.rio.Stdin(), p.stdinSt)
			_ = p.rio.Stdin().Close()
		}()
	} else {
		_ = p.rio.Stdin().Close()
	}
	copyOut := func(dst *mux.Stream, src io.Reader) {
		defer p.output.Done()
		if dst == nil {
			_, _ = io.Copy(io.Discard, src)
			return
		}
		_, _ = io.Copy(dst, src)
	}
	p.output.Add(2)
	go copyOut(p.stdoutSt, p.rio.Stdout())
	go copyOut(p.stderrSt, p.rio.Stderr())
	return nil
}

func (s *taskService) Start(ctx context.Context, req *taskapi.StartRequest) (*taskapi.StartResponse, error) {
	c, p, err := s.find(req.ID, req.ExecID)
	if err != nil {
		return nil, err
	}
	r := newRunc()
	if req.ExecID == "" {
		if err := r.Start(ctx, req.ID); err != nil {
			return nil, fmt.Errorf("runc start: %w", err)
		}
		s.mu.Lock()
		p.started = true
		s.mu.Unlock()
		return &taskapi.StartResponse{Pid: uint32(p.pid)}, nil
	}

	dir := filepath.Join(c.bundle, "exec-"+req.ExecID)
	if err := os.MkdirAll(dir, 0o711); err != nil {
		return nil, err
	}
	opts := &runc.ExecOpts{PidFile: filepath.Join(dir, "pid"), Detach: true}
	var sock *runc.Socket
	if p.terminal {
		if sock, err = runc.NewTempConsoleSocket(); err != nil {
			return nil, err
		}
		defer sock.Close()
		opts.ConsoleSocket = sock
	} else {
		if p.rio, err = runc.NewPipeIO(int(p.spec.User.UID), int(p.spec.User.GID)); err != nil {
			return nil, err
		}
		opts.IO = p.rio
	}
	if err := r.Exec(ctx, req.ID, *p.spec, opts); err != nil {
		p.closeIO()
		return nil, fmt.Errorf("runc exec: %w", err)
	}
	if err := s.startIO(p, sock); err != nil {
		return nil, err
	}
	pid, err := runc.ReadPidFile(opts.PidFile)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	p.pid = pid
	p.started = true
	s.track(p)
	s.mu.Unlock()
	return &taskapi.StartResponse{Pid: uint32(pid)}, nil
}

func (s *taskService) Exec(ctx context.Context, req *taskapi.ExecProcessRequest) (*emptypb.Empty, error) {
	c, _, err := s.find(req.ID, "")
	if err != nil {
		return nil, err
	}
	v, err := typeurl.UnmarshalAny(req.Spec)
	if err != nil {
		return nil, err
	}
	pspec, ok := v.(*specs.Process)
	if !ok {
		return nil, fmt.Errorf("exec %s: spec is %T", req.ExecID, v)
	}
	// The Engine's exec spec has the container's capabilities only if it
	// built one; start from the container's process.
	if pspec.Capabilities == nil {
		pspec.Capabilities = c.spec.Process.Capabilities
	}
	if pspec.Rlimits == nil {
		pspec.Rlimits = c.spec.Process.Rlimits
	}
	pspec.ApparmorProfile = c.spec.Process.ApparmorProfile
	pspec.NoNewPrivileges = c.spec.Process.NoNewPrivileges
	if u := pspec.User.Username; u != "" {
		tmp := &specs.Spec{Root: c.spec.Root, Process: &specs.Process{}, Linux: &specs.Linux{}}
		ctx := namespaces.WithNamespace(ctx, "airos")
		if err := oci.ApplyOpts(ctx, nil, &containers.Container{ID: c.id}, tmp,
			oci.WithUser(u), oci.WithAdditionalGIDs(u)); err != nil {
			return nil, fmt.Errorf("exec user %s: %w", u, err)
		}
		pspec.User = tmp.Process.User
	} else if pspec.User.UID == 0 && pspec.User.GID == 0 {
		pspec.User = c.spec.Process.User
	}
	if pspec.Cwd == "" {
		pspec.Cwd = c.spec.Process.Cwd
	}

	p := &process{id: req.ID, execID: req.ExecID, spec: pspec, terminal: req.Terminal || pspec.Terminal,
		done: make(chan struct{}), stdinPath: req.Stdin, stdoutPath: req.Stdout, stderrPath: req.Stderr}
	pspec.Terminal = p.terminal
	if err := s.claimIO(ctx, p, req.Stdin, req.Stdout, req.Stderr); err != nil {
		return nil, err
	}
	s.mu.Lock()
	c.execs[req.ExecID] = p
	s.mu.Unlock()
	return &emptypb.Empty{}, nil
}

func (s *taskService) State(ctx context.Context, req *taskapi.StateRequest) (*taskapi.StateResponse, error) {
	_, p, err := s.find(req.ID, req.ExecID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	status := taskpb.Status_CREATED
	switch {
	case p.exited:
		status = taskpb.Status_STOPPED
	case p.started:
		status = taskpb.Status_RUNNING
	}
	resp := &taskapi.StateResponse{
		ID: req.ID, ExecID: req.ExecID, Pid: uint32(p.pid), Status: status,
		Stdin: p.stdinPath, Stdout: p.stdoutPath, Stderr: p.stderrPath, Terminal: p.terminal,
		ExitStatus: p.exitStatus,
	}
	if p.exited {
		resp.ExitedAt = timestamppb.New(p.exitedAt)
	}
	return resp, nil
}

func (s *taskService) Wait(ctx context.Context, req *taskapi.WaitRequest) (*taskapi.WaitResponse, error) {
	_, p, err := s.find(req.ID, req.ExecID)
	if err != nil {
		return nil, err
	}
	select {
	case <-p.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return &taskapi.WaitResponse{ExitStatus: p.exitStatus, ExitedAt: timestamppb.New(p.exitedAt)}, nil
}

func (s *taskService) Kill(ctx context.Context, req *taskapi.KillRequest) (*emptypb.Empty, error) {
	_, p, err := s.find(req.ID, req.ExecID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	exited, pid := p.exited, p.pid
	s.mu.Unlock()
	if exited || pid == 0 {
		return &emptypb.Empty{}, nil
	}
	if req.ExecID == "" {
		err = newRunc().Kill(ctx, req.ID, int(req.Signal), &runc.KillOpts{All: req.All})
		if err != nil && !strings.Contains(err.Error(), "not running") && !strings.Contains(err.Error(), "does not exist") {
			return nil, err
		}
		return &emptypb.Empty{}, nil
	}
	if err := unix.Kill(pid, unix.Signal(req.Signal)); err != nil && err != unix.ESRCH {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *taskService) Delete(ctx context.Context, req *taskapi.DeleteRequest) (*taskapi.DeleteResponse, error) {
	c, p, err := s.find(req.ID, req.ExecID)
	if err != nil {
		return nil, err
	}
	if req.ExecID != "" {
		s.mu.Lock()
		delete(c.execs, req.ExecID)
		s.mu.Unlock()
		_ = os.RemoveAll(filepath.Join(c.bundle, "exec-"+req.ExecID))
	} else {
		_ = newRunc().Delete(ctx, req.ID, &runc.DeleteOpts{Force: true})
		s.nets.detach(req.ID)
		s.mu.Lock()
		delete(s.containers, req.ID)
		s.mu.Unlock()
		_ = os.RemoveAll(c.bundle)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &taskapi.DeleteResponse{Pid: uint32(p.pid), ExitStatus: p.exitStatus}
	if p.exited {
		resp.ExitedAt = timestamppb.New(p.exitedAt)
	}
	return resp, nil
}

func (s *taskService) Pids(ctx context.Context, req *taskapi.PidsRequest) (*taskapi.PidsResponse, error) {
	pids, err := newRunc().Ps(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	resp := &taskapi.PidsResponse{}
	for _, pid := range pids {
		resp.Processes = append(resp.Processes, &taskpb.ProcessInfo{Pid: uint32(pid)})
	}
	return resp, nil
}

func (s *taskService) Stats(ctx context.Context, req *taskapi.StatsRequest) (*taskapi.StatsResponse, error) {
	c, _, err := s.find(req.ID, "")
	if err != nil {
		return nil, err
	}
	mgr, err := cgroup2.Load(c.spec.Linux.CgroupsPath)
	if err != nil {
		return nil, err
	}
	m, err := mgr.Stat()
	if err != nil {
		return nil, err
	}
	// Without a limit the VM's memory is the limit (Docker shows the host's).
	var si unix.Sysinfo_t
	if m.Memory != nil && unix.Sysinfo(&si) == nil {
		if total := uint64(si.Totalram) * uint64(si.Unit); m.Memory.UsageLimit == 0 || m.Memory.UsageLimit > total {
			m.Memory.UsageLimit = total
		}
	}
	a, err := typeurl.MarshalAnyToProto(m)
	if err != nil {
		return nil, err
	}
	return &taskapi.StatsResponse{Stats: a}, nil
}

func (s *taskService) ResizePty(ctx context.Context, req *taskapi.ResizePtyRequest) (*emptypb.Empty, error) {
	_, p, err := s.find(req.ID, req.ExecID)
	if err != nil {
		return nil, err
	}
	if p.console != nil {
		_ = p.console.Resize(console.WinSize{Height: uint16(req.Height), Width: uint16(req.Width)})
	}
	return &emptypb.Empty{}, nil
}

func (s *taskService) CloseIO(ctx context.Context, req *taskapi.CloseIORequest) (*emptypb.Empty, error) {
	_, p, err := s.find(req.ID, req.ExecID)
	if err != nil {
		return nil, err
	}
	if req.Stdin && p.stdin != nil && p.console == nil {
		_ = p.stdin.Close()
	}
	return &emptypb.Empty{}, nil
}

func (s *taskService) Update(ctx context.Context, req *taskapi.UpdateTaskRequest) (*emptypb.Empty, error) {
	if req.Resources == nil {
		return &emptypb.Empty{}, nil
	}
	v, err := typeurl.UnmarshalAny(req.Resources)
	if err != nil {
		return nil, err
	}
	res, ok := v.(*specs.LinuxResources)
	if !ok {
		return nil, fmt.Errorf("update: resources are %T", v)
	}
	return &emptypb.Empty{}, newRunc().Update(ctx, req.ID, res)
}

func (s *taskService) Pause(ctx context.Context, req *taskapi.PauseRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, newRunc().Pause(ctx, req.ID)
}

func (s *taskService) Resume(ctx context.Context, req *taskapi.ResumeRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, newRunc().Resume(ctx, req.ID)
}

func (s *taskService) Checkpoint(ctx context.Context, req *taskapi.CheckpointTaskRequest) (*emptypb.Empty, error) {
	return nil, fmt.Errorf("checkpoint is not supported in the air/OS Linux VM")
}

func (s *taskService) Connect(ctx context.Context, req *taskapi.ConnectRequest) (*taskapi.ConnectResponse, error) {
	resp := &taskapi.ConnectResponse{ShimPid: uint32(os.Getpid())}
	if _, p, err := s.find(req.ID, ""); err == nil {
		resp.TaskPid = uint32(p.pid)
	}
	return resp, nil
}

func (s *taskService) Shutdown(ctx context.Context, req *taskapi.ShutdownRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

// dial connects a host stream to a port of a container: "dial tcp <id> 80".
// UDP datagrams travel length-prefixed (uint16, big-endian).
func (s *taskService) dial(st *mux.Stream, proto, id, port string) {
	if _, err := strconv.Atoi(port); err != nil || (proto != "tcp" && proto != "udp") {
		st.Close()
		return
	}
	host := "127.0.0.1"
	s.mu.Lock()
	c := s.containers[id]
	s.mu.Unlock()
	if c == nil {
		st.Close()
		return
	}
	if strings.HasPrefix(c.network.Mode, "container:") {
		id = strings.TrimPrefix(c.network.Mode, "container:")
	}
	if ip := s.nets.ip(id); ip != nil {
		host = ip.String()
	}
	conn, err := net.DialTimeout(proto, net.JoinHostPort(host, port), 5*time.Second)
	if err != nil {
		st.Close()
		return
	}
	if proto == "tcp" {
		go pipe(conn.(*net.TCPConn), st)
		pipe(st, conn)
		conn.Close()
		return
	}
	go func() {
		defer conn.Close()
		var lb [2]byte
		for {
			if _, err := io.ReadFull(st, lb[:]); err != nil {
				return
			}
			b := make([]byte, int(lb[0])<<8|int(lb[1]))
			if _, err := io.ReadFull(st, b); err != nil {
				return
			}
			_, _ = conn.Write(b)
		}
	}()
	buf := make([]byte, 65535)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
		n, err := conn.Read(buf)
		if err != nil {
			break
		}
		if _, err := st.Write(append([]byte{byte(n >> 8), byte(n)}, buf[:n]...)); err != nil {
			break
		}
	}
	st.Close()
	conn.Close()
}

var _ taskapi.TTRPCTaskService = (*taskService)(nil)
