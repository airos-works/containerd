# containerd for air/OS

air/OS (Haiku) port of containerd, maintained by the airos-works project.
Upstream is [containerd/containerd](https://github.com/containerd/containerd).
This fork's work stays in the fork (upstream's AI/contribution policy does not
accept it); vendored files carry the port's Haiku shims, matching the moby and
docker fork conventions.

## Status

- `cmd/containerd` builds for `haiku/amd64` and `haiku/arm64`
  (`tools/build-haiku.sh`).
- The x/sys/unix Haiku shim is shared with airos-works/moby and
  airos-works/docker (vendored copy here): syscall-backed constants and
  wrappers, mount/xattr stubs reporting ENOSYS, SCM_RIGHTS reexports, and
  Wait4/WaitStatus for the reaper.
- Snapshotter notes for Haiku: `vfs` needs no kernel mounts and works as-is;
  bind/nullfs-style snapshotters need the mount backend, which is ENOSYS in
  this stage. The shim/runtime plugins are not wired (no OCI runtime on
  Haiku yet); content/image services are the next runtime milestone, driven
  by the Engine (airos-works/moby stage 2).
