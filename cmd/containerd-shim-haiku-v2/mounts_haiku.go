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
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// linkBindMounts realizes the spec's bind mounts (volumes, -v host paths,
// and the daemon's /etc/hosts, /etc/hostname and /etc/resolv.conf) in the
// container's root filesystem. Haiku has no bind mounts, so each destination
// becomes a symlink to its source; containers run with the rootfs as their
// working directory (see runInit), so paths through the destination reach
// the source. What the image had at a destination is removed from the
// container's own snapshot first, as a mount would hide it. Read-only binds
// cannot be enforced this way.
//
// The rootfs is the image's, so it is opened as an os.Root: a symlink in it
// cannot lead the shim to create or remove anything outside it.
func linkBindMounts(rootfs string, mounts []specs.Mount) error {
	var root *os.Root
	for _, m := range mounts {
		if m.Type != "bind" || !filepath.IsAbs(m.Source) {
			continue
		}
		if root == nil {
			r, err := os.OpenRoot(rootfs)
			if err != nil {
				return err
			}
			defer r.Close()
			root = r
		}
		if err := linkBindMount(root, m.Destination, m.Source); err != nil {
			return fmt.Errorf("bind mount %s: %w", m.Destination, err)
		}
	}
	return nil
}

func linkBindMount(root *os.Root, dest, source string) error {
	name := strings.TrimPrefix(path.Clean("/"+dest), "/")
	if name == "" {
		return errors.New("cannot replace the root filesystem")
	}
	if dir := path.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if current, err := root.Readlink(name); err == nil && current == source {
		return nil // already linked, as on a restart
	}
	if err := root.RemoveAll(name); err != nil {
		return err
	}
	return root.Symlink(source, name)
}
