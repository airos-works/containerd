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

package shim

import (
	"github.com/containerd/ttrpc"
)

func newServer(opts ...ttrpc.ServerOpt) (*ttrpc.Server, error) {
	return ttrpc.NewServer(opts...)
}

// subreaper is a no-op: Haiku has no PR_SET_CHILD_SUBREAPER, and the reaper
// package polls Wait4 directly.
func subreaper() error {
	return nil
}
