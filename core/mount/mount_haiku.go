//go:build haiku

package mount

import "github.com/containerd/errdefs"

// Mount is not implemented on Haiku yet; the snapshotter work adds the
// bind/nullfs backend.
func (m *Mount) mount(target string) error {
	return errdefs.ErrNotImplemented
}
