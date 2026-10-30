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

## Linux containers: the Linux VM

Containers from Linux images run in a small Linux VM, as Docker Desktop
runs them on macOS and Windows, with Linux's namespaces, cgroup v2,
seccomp, capabilities and networks. The VM itself (QEMU with NVMM, the
kernel and the initramfs) is the `linuxvm` package, built by
[airos-works/linuxvm](https://github.com/airos-works/linuxvm); this fork
has the three programs that run containers in it:

- **The haiku shim** forwards a container whose spec carries the
  `works.airos.platform=linux` annotation (the Engine sets it for Linux
  images) to the VM's agent: the TaskService over ttrpc, its stdio between
  the Engine's fifos and agent streams, its exit events, and its published
  ports, for which it listens on the host and opens a stream per TCP
  connection or UDP peer (`cmd/containerd-shim-haiku-v2/linux.go`). Paths
  in the spec become paths on the VM's share of the host (symlinks
  resolved on the host), and Haiku's signal numbers Linux's.
- **`containerd-linuxvm`** (`cmd/containerd-linuxvm`, installed in
  `/boot/system/bin`) boots the VM on demand: the shim starts it for the
  first Linux container. It owns the VM's one virtio-serial channel and
  multiplexes it (`pkg/linuxvm/mux`, per-stream flow control): each
  connection to `/var/run/containerd-linuxvm.sock` sends a header line and
  becomes one stream to the agent. `status` and `stop` are answered by the
  supervisor itself. It sets the VM's clock from the host's every minute and
  boots the kernel with `tsc=reliable` (the VCPUs run on the host's TSC;
  Linux's own check refuses the few hundred cycles between a Threadripper's
  dies, and its fallback, the HPET, is a VM exit per clock read).
  `containerd-linuxvm run ROOTFS COMMAND...` runs a container straight from a
  directory, to check the VM without the Engine.
- **`containerd-linuxvm-agent`** (`cmd/containerd-linuxvm-agent`, Linux, the
  VM's PID 1) copies the initramfs to a tmpfs root (runc cannot pivot away
  from the initial rootfs), mounts the kernel file systems, cgroup v2 with
  every controller, and the host's root directory twice over virtio-9p
  (`/host`, `cache=mmap`, for bind mounts; `/hostc`, `cache=loose`, for root
  file systems and what is under the Engine's data root, which only
  containers change; it syncs `/hostc` when a container stops). It runs
  containers with runc (go-runc, the containerd reaper), resolves the image
  user against the container's `/etc/passwd`, applies the default seccomp
  profile, gives each container a private cgroup namespace, and implements
  `docker top` (`ps` stream), stats (cgroup v2 metrics, the VM's memory as
  the limit when there is none), pause, update and exec. Each Docker network
  is a bridge with its own /16 (`bridge` is `docker0`, 172.17.0.0/16), NAT
  out of the VM, no forwarding between networks, and a DNS server on its
  gateway for container names and aliases that forwards the rest to the
  host's servers.

Verified on the X399 (Threadripper 1950X, NVMM): the VM boots in about a
second and a warm container starts in about 0.35 s; `docker run -it`,
`-d -p` (TCP and UDP), `-v` host paths and volumes (`:ro` enforced), `-u`,
`--network` (user networks with name resolution and isolation, `host`,
`none`, `container:`), `--privileged`, `--cap-add/--cap-drop`, `--memory`,
`--cpus`, `--pids-limit`, `--read-only`, `--tmpfs`, `--sysctl`, `--ulimit`,
`exec -u`, `logs`, `stop` with the image's stop signal, `kill -s`, `top`,
`stats`, `pause`, `update`, restart policies, and Compose stacks
(PostgreSQL, Redis, Python with pip). Unix sockets and FIFOs work on 9p.
