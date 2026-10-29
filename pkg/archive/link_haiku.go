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

package archive

import (
	"errors"
	"io"
	"os"
	"syscall"
)

// link creates newname as a hard link to oldname. BFS, Haiku's native file
// system, has no hard links and refuses them with EPERM, so there newname
// becomes a copy of oldname instead (a symlink stays a symlink, as link(2)
// does not follow it). The files read the same; unlike hard links, writing
// one later does not change the other, and each takes its own space.
func link(oldname, newname string) error {
	err := os.Link(oldname, newname)
	if err == nil || !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	if cerr := copyLinkTarget(oldname, newname); cerr != nil {
		return &os.LinkError{Op: "link (copy)", Old: oldname, New: newname, Err: cerr}
	}
	return nil
}

func copyLinkTarget(oldname, newname string) error {
	fi, err := os.Lstat(oldname)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(oldname)
		if err != nil {
			return err
		}
		return os.Symlink(target, newname)
	}
	if !fi.Mode().IsRegular() {
		return syscall.EPERM
	}
	src, err := os.Open(oldname)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(newname, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(newname)
		return err
	}
	return dst.Close()
}
