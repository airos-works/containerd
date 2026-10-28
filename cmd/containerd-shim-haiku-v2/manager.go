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
	"fmt"
	"io"
	"time"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/version"
)

// haikuManager implements the containerd shim manager side: the "start"
// helper that daemonizes the long-running server and reports its address.
type haikuManager struct {
	name string
}

func newHaikuManager(name string) *haikuManager {
	return &haikuManager{name: name}
}

func (m *haikuManager) Name() string {
	return m.name
}

func (m *haikuManager) newCommand(ctx context.Context, id string) (*exec.Cmd, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(self,
		"-namespace", ns,
		"-id", id,
		"-address", addressFlagValue(),
	)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GOMAXPROCS=4")
	return cmd, nil
}

// addressFlagValue returns the -address value containerd passed to the
// start helper; the server re-reads it from its own flags.
func addressFlagValue() string {
	for i, a := range os.Args {
		if a == "-address" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
		if len(a) > 9 && a[:9] == "-address=" {
			return a[9:]
		}
	}
	return ""
}

func (m *haikuManager) Start(ctx context.Context, opts *bootapi.BootstrapParams) (_ *bootapi.BootstrapResult, retErr error) {
	var params bootapi.BootstrapResult
	params.Version = 3
	params.Protocol = "ttrpc"

	id := opts.GetInstanceID()

	cmd, err := m.newCommand(ctx, id)
	if err != nil {
		return nil, err
	}

	socketDir := opts.GetSocketDir()
	if socketDir == "" {
		socketDir = filepath.Join(defaults.DefaultStateDir, "s")
	}
	address, err := shim.CreateSocketAddress(ctx, socketDir, opts.GetContainerdGrpcAddress(), id, false)
	if err != nil {
		return nil, err
	}
	socket, err := shim.NewSocket(address)
	if err != nil {
		if !shim.SocketEaddrinuse(err) {
			return nil, fmt.Errorf("create new shim socket: %w", err)
		}
		if shim.CanConnect(address) {
			params.Address = address
			return &params, nil
		}
		if err := shim.RemoveSocket(address); err != nil {
			return nil, fmt.Errorf("remove pre-existing socket: %w", err)
		}
		if socket, err = shim.NewSocket(address); err != nil {
			return nil, fmt.Errorf("try create new shim socket 2x: %w", err)
		}
	}
	f, err := socket.File()
	if err != nil {
		socket.Close()
		return nil, err
	}
	defer func() {
		if retErr != nil {
			socket.Close()
			f.Close()
		}
	}()
	cmd.ExtraFiles = append(cmd.ExtraFiles, f)

	goruntime.LockOSThread()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	goruntime.UnlockOSThread()

	defer func() {
		if retErr != nil {
			_ = cmd.Process.Kill()
		}
	}()
	// Reap the helper's child status.
	go func() { _ = cmd.Wait() }()

	params.Address = address
	return &params, nil
}

func (m *haikuManager) Stop(ctx context.Context, id string) (shim.StopStatus, error) {
	// The server exits via the Shutdown RPC; the "delete" helper only
	// reports a synthesized status.
	return shim.StopStatus{Pid: os.Getpid(), ExitStatus: 0, ExitedAt: time.Now()}, nil
}

func (m *haikuManager) Info(ctx context.Context, optionsR io.Reader) (*types.RuntimeInfo, error) {
	info := &types.RuntimeInfo{
		Name: m.name,
		Version: &types.RuntimeVersion{
			Version:  version.Version,
			Revision: version.Revision,
		},
	}
	return info, nil
}

