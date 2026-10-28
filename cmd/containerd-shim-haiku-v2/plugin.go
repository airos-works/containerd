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

	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
)

// runtimeName is the containerd runtime this shim serves. It answers to
// containerd's default runtime name so moby needs no configuration changes;
// on air/OS "runc" semantics are provided by chroot execution.
const runtimeName = "io.containerd.runc.v2"

// RunShimHaiku boots the shim with the haiku manager.
func RunShimHaiku(ctx context.Context) {
	shim.RunShim(ctx, newHaikuManager(runtimeName))
}

// registerTaskPlugin wires the TaskService into the shim's plugin registry
// so the server mode exposes it over ttrpc.
func registerTaskPlugin() {
	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin,
		ID:   "task",
		Requires: []plugin.Type{
			plugins.EventPlugin,
			plugins.InternalPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			pp, err := ic.GetByID(plugins.EventPlugin, "publisher")
			if err != nil {
				return nil, err
			}
			svc := newHaikuTaskService(pp.(events.Publisher))
			return &taskServicePlugin{svc: svc}, nil
		},
	})
}

// taskServicePlugin adapts the TaskService to the ttrpc registration used
// by pkg/shim's server mode.
type taskServicePlugin struct {
	svc *haikuTaskService
}

func (p *taskServicePlugin) RegisterTTRPC(server *ttrpc.Server) error {
	taskapi.RegisterTTRPCTaskService(server, p.svc)
	return nil
}
