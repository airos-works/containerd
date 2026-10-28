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
- Snapshotter notes for Haiku: `native` needs no kernel mounts and is
  registered for haiku; the **haiku OCI shim** (`cmd/containerd-shim-haiku-v2`,
  installed as `containerd-shim-runc-v2`) implements the shim v3 bootstrap
  and TaskService over ttrpc, running containers as chrooted (or, until
  per-snapshot bfs mounts land, cwd-isolated) processes.
- **Verified (QEMU hrev59866):** `docker run` of an imported haiku/amd64
  image prints container output with the correct exit code; `docker ps -a`,
  `docker logs`, `docker rm` behave as on Linux.
