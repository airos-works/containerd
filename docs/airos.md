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
  `docker logs`, `docker rm` behave as on Linux; `docker run -i` streams
  stdin with proper EOF; detached runs, `docker stop`, and `docker exec`
  (including `exec -i`) work; `docker compose up` runs multi-service stacks
  on host networking (the bridge driver is not ported).
- **Terminals:** `docker run -t` and `docker exec -t` get a Haiku pty (opened
  with `O_NOCTTY`; the process leads its own session with the pty as its
  controlling terminal, so ^C and job control work); `ResizePty` covers execs.
- **Bind mounts:** the spec's `bind` mounts (volumes, `-v` host paths, the
  daemon's `/etc/hosts`, `/etc/hostname`, `/etc/resolv.conf`) become symlinks
  in the container's rootfs, created through an `os.Root` so an image's
  symlinks cannot reach outside it. Read-only binds are not enforced.
- **Stats:** `Stats` reports cgroup v2-shaped metrics from the kernel kit:
  CPU time (`get_team_usage_info`) and RAM (`get_next_area_info`) of the
  container's team trees, their team count, and the machine's RAM as limit.
- **Processes:** each container process leads its own process group (a
  session on a pty), which `Kill` signals; when the init exits, the rest of
  the container (its group and the exec processes) is killed, as when a pid
  namespace's init dies. `Pids` returns every team in the container's trees
  (`docker top`). Exit events are published with a context that outlives the
  Create/Exec request, so `docker stop`/`rm -f` see the exit at once.
- **Hard links:** BFS has none, so layer extraction (`pkg/archive`) copies the
  file when `link(2)` is refused (images like busybox grow accordingly).
