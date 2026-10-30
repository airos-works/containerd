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

// Package linuxvm holds what the air/OS side (the haiku shim and the VM
// supervisor) and the agent in the Linux VM agree on: the annotations the
// Docker Engine sets on Linux containers, the stream headers of the
// multiplexed agent connection, and how host paths appear in the VM.
//
// Linux containers on air/OS run in one lightweight Linux VM (QEMU with
// NVMM). The VM mounts the host's root directory over virtio-9p at
// HostMount, so container root filesystems and bind mounts stay on BFS,
// where the Engine created them.
package linuxvm

import (
	"path"
	"strings"
)

// Annotations on the OCI spec of a container from a Linux image.
const (
	// AnnotationPlatform is "linux" for containers the VM runs.
	AnnotationPlatform = "works.airos.platform"
	// AnnotationUser is the image's or the run's USER, resolved against the
	// container's /etc/passwd and /etc/group in the VM.
	AnnotationUser = "works.airos.user"
	// AnnotationNetwork is the JSON Network the container is attached to.
	AnnotationNetwork = "works.airos.network"
	// AnnotationPorts is the JSON []PortBinding the container publishes.
	AnnotationPorts = "works.airos.ports"
	// AnnotationPrivileged is "true" for docker run --privileged.
	AnnotationPrivileged = "works.airos.privileged"
	// AnnotationDataRoot is the Engine's data root on the host (symlinks
	// resolved): what is under it only containers write, so the VM caches
	// it (CachedMount).
	AnnotationDataRoot = "works.airos.dataroot"
)

// Network describes a container's networking.
type Network struct {
	// Mode is "bridge" (one or more Docker networks), "host" (the VM's own
	// network), "none", or "container:<id>".
	Mode     string     `json:"mode"`
	Hostname string     `json:"hostname,omitempty"`
	Networks []Endpoint `json:"networks,omitempty"`
}

// Endpoint is the container on one Docker network.
type Endpoint struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"`
}

// PortBinding publishes a container port on the host.
type PortBinding struct {
	Proto         string `json:"proto"` // tcp or udp
	ContainerPort int    `json:"containerPort"`
	HostIP        string `json:"hostIP,omitempty"`
	HostPort      int    `json:"hostPort"`
}

// Stream headers: the first line of each multiplexed stream says what the
// host wants from the agent.
const (
	HeaderTTRPC = "ttrpc"             // containerd's TaskService
	HeaderPing  = "ping"              // the agent answers "pong"
	HeaderIO    = "io"                // "io <token>": a stdio stream for a task
	HeaderDial  = "dial"              // "dial tcp|udp <container id> <port>"
	HeaderInfo  = "info"              // the agent writes a JSON Info
	HeaderPs    = "ps"                // the agent writes ps -o pid,user,ppid,stat,time,args
	HeaderTime  = "time"              // "time <unix nanoseconds>": the host's clock
	IOPrefix    = "mux:"              // CreateTaskRequest.Stdin etc: "mux:<token>"
	PortName    = "works.airos.agent" // the virtio serial port
)

// Info describes the VM.
type Info struct {
	Kernel   string `json:"kernel"`
	CPUs     int    `json:"cpus"`
	MemoryKB uint64 `json:"memoryKB"`
	Runc     string `json:"runc"`
}

// HostMount is where the host's root directory is mounted in the VM, with
// no caching beyond mmap: what the host changes shows at once. CachedMount
// is the same directory with the VM's page cache in front of it, for root
// filesystems and volumes, which only containers change.
const (
	HostMount   = "/host"
	CachedMount = "/hostc"
)

// VMPath is the path in the VM of a host path that has no symlinks left
// (the host resolves them first: the VM sees them unresolved).
func VMPath(hostPath string) string {
	return path.Join(HostMount, path.Clean("/"+hostPath))
}

// IsVMPath reports whether p is under HostMount.
func IsVMPath(p string) bool {
	return p == HostMount || strings.HasPrefix(p, HostMount+"/")
}
