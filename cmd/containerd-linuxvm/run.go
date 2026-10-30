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
	"fmt"
	"io"
	"os"
	"path/filepath"

	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/core/containers"
	_ "github.com/containerd/containerd/v2/core/runtime" // the spec's type URL
	"github.com/containerd/containerd/v2/pkg/linuxvm"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/ttrpc"
	typeurl "github.com/containerd/typeurl/v2"
)

// runContainer is "containerd-linuxvm run ROOTFS COMMAND...": a container
// straight from a root filesystem directory, without the Engine, to check
// the VM.
func runContainer(socket string, args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: containerd-linuxvm run ROOTFS COMMAND...")
		return 2
	}
	rootfs, err := filepath.EvalSymlinks(args[0])
	if err == nil {
		rootfs, err = filepath.Abs(rootfs)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx := namespaces.WithNamespace(context.Background(), "airos")
	id := token()
	spec, err := oci.GenerateSpecWithPlatform(ctx, nil, "linux/"+goarch(), &containers.Container{ID: id},
		oci.WithRootFSPath(linuxvm.VMPath(rootfs)), oci.WithProcessArgs(args[1:]...),
		oci.WithHostname("linuxvm-run"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	spec.Annotations = map[string]string{linuxvm.AnnotationPlatform: "linux"}
	opts, err := typeurl.MarshalAnyToProto(spec)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	outTok := token()
	out, err := linuxvm.Dial(socket, linuxvm.HeaderIO+" "+outTok)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(os.Stdout, out)
		close(done)
	}()
	conn, err := linuxvm.Dial(socket, linuxvm.HeaderTTRPC)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	client := ttrpc.NewClient(conn)
	defer client.Close()
	tasks := taskapi.NewTTRPCTaskClient(client)
	if _, err := tasks.Create(ctx, &taskapi.CreateTaskRequest{
		ID: id, Stdout: linuxvm.IOPrefix + outTok, Stderr: linuxvm.IOPrefix + outTok, Options: opts,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "create:", err)
		return 1
	}
	if _, err := tasks.Start(ctx, &taskapi.StartRequest{ID: id}); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		return 1
	}
	w, err := tasks.Wait(ctx, &taskapi.WaitRequest{ID: id})
	if err != nil {
		fmt.Fprintln(os.Stderr, "wait:", err)
		return 1
	}
	<-done
	_, _ = tasks.Delete(ctx, &taskapi.DeleteRequest{ID: id})
	return int(w.ExitStatus)
}

func token() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
