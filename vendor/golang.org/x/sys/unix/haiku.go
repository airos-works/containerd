// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build haiku

// Minimal Haiku port of golang.org/x/sys/unix, backed by the Haiku Go
// toolchain's syscall package (gen2brain/go). It provides the subset of
// symbols needed by the Docker CLI and its dependencies; extend as needed.
//
// Haiku numbering differs from POSIX for signals (SIGCHLD is 5, not 17),
// so everything is aliased from package syscall rather than hardcoded.

package unix

import (
	"syscall"
	"time"
	"unsafe"
)

type Signal = syscall.Signal
type Errno = syscall.Errno
type SysProcAttr = syscall.SysProcAttr
type Stat_t = syscall.Stat_t
type Statfs_t = syscall.Statfs_t
type Rusage = syscall.Rusage
type Rlimit = syscall.Rlimit
type ProcAttr = syscall.ProcAttr
type Credential = syscall.Credential
type Sockaddr = syscall.Sockaddr
type SockaddrInet4 = syscall.SockaddrInet4
type SockaddrInet6 = syscall.SockaddrInet6
type SockaddrUnix = syscall.SockaddrUnix
type SockaddrDatalink = syscall.SockaddrDatalink
type RawSockaddrAny = syscall.RawSockaddrAny
type RawConn = syscall.RawConn
type Conn = syscall.Conn
type Timeval = syscall.Timeval
type Timespec = syscall.Timespec
type FdSet = syscall.FdSet
type IPMreq = syscall.IPMreq
type IPv6Mreq = syscall.IPv6Mreq
type Linger = syscall.Linger
type Msghdr = syscall.Msghdr
type Cmsghdr = syscall.Cmsghdr
type Ucred = syscall.Ucred
type ICMPv6Filter = syscall.ICMPv6Filter
type Utsname = syscall.Utsname
type Flock_t = syscall.Flock_t

// Termios mirrors Haiku's struct termios; field order from
// headers/posix/termios.h (encoded in the toolchain's ztypes_haiku_*.go).
type Termios = syscall.Termios

// Winsize mirrors Haiku's struct winsize (headers/posix/termios.h).
type Winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

const (
	// Signals (Haiku numbering, from the toolchain).
	SIGHUP    = syscall.SIGHUP
	SIGINT    = syscall.SIGINT
	SIGQUIT   = syscall.SIGQUIT
	SIGILL    = syscall.SIGILL
	SIGTRAP   = syscall.SIGTRAP
	SIGABRT   = syscall.SIGABRT
	SIGIOT    = syscall.SIGABRT
	SIGFPE    = syscall.SIGFPE
	SIGKILL   = syscall.SIGKILL
	SIGBUS    = syscall.SIGBUS
	SIGSEGV   = syscall.SIGSEGV
	SIGSYS    = syscall.SIGSYS
	SIGPIPE   = syscall.SIGPIPE
	SIGALRM   = syscall.SIGALRM
	SIGTERM   = syscall.SIGTERM
	SIGURG    = syscall.SIGURG
	SIGSTOP   = syscall.SIGSTOP
	SIGTSTP   = syscall.SIGTSTP
	SIGCONT   = syscall.SIGCONT
	SIGCHLD   = syscall.SIGCHLD
	SIGCLD    = syscall.SIGCHLD
	SIGTTIN   = syscall.SIGTTIN
	SIGTTOU   = syscall.SIGTTOU
	SIGIO     = syscall.SIGPOLL
	SIGPOLL   = syscall.SIGPOLL
	SIGXCPU   = syscall.SIGXCPU
	SIGXFSZ   = syscall.SIGXFSZ
	SIGVTALRM = syscall.SIGVTALRM
	SIGPROF   = syscall.SIGPROF
	SIGWINCH  = syscall.SIGWINCH
	SIGUSR1   = syscall.SIGUSR1
	SIGUSR2   = syscall.SIGUSR2

	// File modes.
	S_IFMT   = syscall.S_IFMT
	S_IFBLK  = syscall.S_IFBLK
	S_IFCHR  = syscall.S_IFCHR
	S_IFDIR  = syscall.S_IFDIR
	S_IFIFO  = syscall.S_IFIFO
	S_IFLNK  = syscall.S_IFLNK
	S_IFREG  = syscall.S_IFREG
	S_IFSOCK = syscall.S_IFSOCK
	S_ISUID  = syscall.S_ISUID
	S_ISGID  = syscall.S_ISGID
	S_ISVTX  = syscall.S_ISVTX

	// Open flags.
	O_RDONLY   = syscall.O_RDONLY
	O_WRONLY   = syscall.O_WRONLY
	O_RDWR     = syscall.O_RDWR
	O_APPEND   = syscall.O_APPEND
	O_CREAT    = syscall.O_CREAT
	O_EXCL     = syscall.O_EXCL
	O_SYNC     = syscall.O_SYNC
	O_TRUNC    = syscall.O_TRUNC
	O_NONBLOCK = syscall.O_NONBLOCK
	O_CLOEXEC  = syscall.O_CLOEXEC
	O_DIRECTORY = syscall.O_DIRECTORY
	O_NOFOLLOW = syscall.O_NOFOLLOW

	// File descriptor flags.
	F_DUPFD   = syscall.F_DUPFD
	F_GETFD   = syscall.F_GETFD
	F_SETFD   = syscall.F_SETFD
	F_GETFL   = syscall.F_GETFL
	F_SETFL   = syscall.F_SETFL
	FD_CLOEXEC = syscall.FD_CLOEXEC

	// at(2) flags.
	AT_FDCWD            = syscall.AT_FDCWD
	AT_REMOVEDIR        = syscall.AT_REMOVEDIR
	AT_SYMLINK_NOFOLLOW = syscall.AT_SYMLINK_NOFOLLOW

	// Socket domains/types/options.
	AF_UNIX     = syscall.AF_UNIX
	AF_INET     = syscall.AF_INET
	AF_INET6    = syscall.AF_INET6
	AF_UNSPEC   = syscall.AF_UNSPEC
	SOCK_STREAM = syscall.SOCK_STREAM
	SOCK_DGRAM  = syscall.SOCK_DGRAM
	SOCK_RAW    = syscall.SOCK_RAW
	SOL_SOCKET  = syscall.SOL_SOCKET
	SO_KEEPALIVE = syscall.SO_KEEPALIVE
	SO_REUSEADDR = syscall.SO_REUSEADDR
	SO_ERROR    = syscall.SO_ERROR
	SO_TYPE     = syscall.SO_TYPE
	SO_RCVBUF   = syscall.SO_RCVBUF
	SO_SNDBUF   = syscall.SO_SNDBUF
	SO_RCVTIMEO = syscall.SO_RCVTIMEO
	SO_SNDTIMEO = syscall.SO_SNDTIMEO
	MSG_PEEK    = syscall.MSG_PEEK
	MSG_DONTWAIT = syscall.MSG_DONTWAIT
	MSG_WAITALL = syscall.MSG_WAITALL

	// mmap.
	PROT_READ  = syscall.PROT_READ
	PROT_WRITE = syscall.PROT_WRITE
	PROT_EXEC  = syscall.PROT_EXEC
	PROT_NONE  = syscall.PROT_NONE
	MAP_SHARED = syscall.MAP_SHARED
	MAP_PRIVATE = syscall.MAP_PRIVATE

	// Path access (unistd.h; not reexported by the toolchain's syscall).
	F_OK = 0
	R_OK = 0x4
	W_OK = 0x2
	X_OK = 0x1

	// Terminal ioctls (Haiku termios.h; values already in the toolchain).
	TIOCGWINSZ = syscall.TIOCGWINSZ
	TIOCSWINSZ = syscall.TIOCSWINSZ

	// Termios; Haiku has no TCGETS/TCSETS ioctls, but moby/term (and other
	// portable code) keys off these names. IoctlGetTermios/IoctlSetTermios
	// below dispatch to tcgetattr/tcsetattr and only use the request to
	// pick the tcsetattr action when it is a TCS* value.
	TCGETS = 0x01
	TCSETS = 0x01

	TCSANOW   = syscall.TCSANOW
	TCSADRAIN = syscall.TCSADRAIN
	TCSAFLUSH = syscall.TCSAFLUSH

	// c_lflag bits.
	ECHO    = syscall.ECHO
	ECHONL  = syscall.ECHONL
	ECHOE   = syscall.ECHOE
	ECHOK   = syscall.ECHOK
	ICANON  = syscall.ICANON
	ISIG    = syscall.ISIG
	IEXTEN  = syscall.IEXTEN
	NOFLSH  = syscall.NOFLSH
	TOSTOP  = syscall.TOSTOP

	// c_iflag bits.
	IGNBRK = syscall.IGNBRK
	BRKINT = syscall.BRKINT
	PARMRK = syscall.PARMRK
	ISTRIP = syscall.ISTRIP
	INLCR  = syscall.INLCR
	IGNCR  = syscall.IGNCR
	ICRNL  = syscall.ICRNL
	IXON   = syscall.IXON
	IXOFF  = syscall.IXOFF
	IXANY  = syscall.IXANY
	INPCK  = syscall.INPCK
	IGNPAR = syscall.IGNPAR

	// c_oflag bits.
	OPOST  = syscall.OPOST
	ONLCR  = syscall.ONLCR
	OCRNL  = syscall.OCRNL
	ONOCR  = syscall.ONOCR
	ONLRET = syscall.ONLRET

	// c_cflag bits.
	CSIZE  = syscall.CSIZE
	CS5    = syscall.CS5
	CS6    = syscall.CS6
	CS7    = syscall.CS7
	CS8    = syscall.CS8
	CSTOPB = syscall.CSTOPB
	CREAD  = syscall.CREAD
	PARENB = syscall.PARENB
	PARODD = syscall.PARODD
	HUPCL  = syscall.HUPCL
	CLOCAL = syscall.CLOCAL

	// c_cc indices.
	VMIN  = syscall.VMIN
	VTIME = syscall.VTIME
	VEOF  = syscall.VEOF
	VEOL  = syscall.VEOL
	VINTR = syscall.VINTR
	VQUIT = syscall.VQUIT
	VERASE = syscall.VERASE
	VKILL = syscall.VKILL
	VSTART = syscall.VSTART
	VSTOP = syscall.VSTOP
	VSUSP = syscall.VSUSP
)

// IoctlGetTermios returns the terminal attributes of fd. Haiku serves
// termios through tcgetattr(3), so req is accepted for API compatibility
// and ignored.
func IoctlGetTermios(fd int, req int) (*Termios, error) {
	t := &Termios{}
	if err := syscall.Tcgetattr(fd, t); err != nil {
		return nil, err
	}
	return t, nil
}

// IoctlSetTermios sets the terminal attributes of fd. req may be TCSETS
// (TCSANOW), TCSADRAIN or TCSAFLUSH to select the tcsetattr action.
func IoctlSetTermios(fd int, req int, t *Termios) error {
	action := req
	switch req {
	case TCSETS:
		action = syscall.TCSANOW
	case TCSADRAIN:
		action = syscall.TCSADRAIN
	case TCSAFLUSH:
		action = syscall.TCSAFLUSH
	}
	return syscall.Tcsetattr(fd, action, t)
}

// IoctlGetWinsize returns the window size of the terminal at fd, using req
// (one of TIOCGWINSZ, TIOCSWINSZ) as the ioctl request.
func IoctlGetWinsize(fd int, req int) (*Winsize, error) {
	w := &Winsize{}
	if err := syscall.Ioctl(fd, req, uintptr(unsafe.Pointer(w))); err != nil {
		return nil, err
	}
	return w, nil
}

// IoctlSetWinsize sets the window size of the terminal at fd.
func IoctlSetWinsize(fd int, req int, w *Winsize) error {
	return syscall.Ioctl(fd, req, uintptr(unsafe.Pointer(w)))
}

// Process identity wrappers (package syscall has the libc-backed calls; the
// x/sys/unix names are expected by portable code).
func Getuid() int  { return syscall.Getuid() }
func Geteuid() int { return syscall.Geteuid() }
func Getgid() int  { return syscall.Getgid() }
func Getegid() int { return syscall.Getegid() }
func Getpid() int  { return syscall.Getpid() }
func Getppid() int { return syscall.Getppid() }

// SetsockoptInt sets a socket option holding an int.
func SetsockoptInt(fd, level, opt int, value int) error {
	return syscall.SetsockoptInt(fd, level, opt, value)
}

// GetsockoptInt gets a socket option holding an int.
func GetsockoptInt(fd, level, opt int) (int, error) {
	return syscall.GetsockoptInt(fd, level, opt)
}

// SetsockoptByte, SetsockoptString and friends delegate to package syscall.
func SetsockoptByte(fd, level, opt int, value byte) error {
	return syscall.SetsockoptByte(fd, level, opt, value)
}

func SetsockoptString(fd, level, opt int, s string) error {
	return syscall.SetsockoptString(fd, level, opt, s)
}

func GetsockoptByte(fd, level, opt int) (byte, error) {
	v, err := syscall.GetsockoptInt(fd, level, opt)
	return byte(v), err
}

// File-system wrappers expected by portable code.
func Openat(dirfd int, path string, flags int, mode uint32) (int, error) {
	return syscall.Openat(dirfd, path, flags, mode)
}

func Fchmodat(dirfd int, path string, mode uint32, flags int) error {
	return syscall.Fchmodat(dirfd, path, mode, flags)
}

func Mknodat(dirfd int, path string, mode uint32, dev int) error {
	return syscall.Mknodat(dirfd, path, mode, dev)
}

func Fchownat(dirfd int, path string, uid int, gid int, flags int) error {
	return syscall.Fchownat(dirfd, path, uid, gid, flags)
}

func Close(fd int) error                { return syscall.Close(fd) }
func Fchmod(fd int, mode uint32) error  { return syscall.Fchmod(fd, mode) }
func Mknod(path string, mode uint32, dev int) error {
	return syscall.Mknod(path, mode, dev)
}
func Umask(mask int) int { return syscall.Umask(mask) }

// Common errnos referenced by portable code.
const (
	EPERM     = syscall.EPERM
	ENOENT    = syscall.ENOENT
	EINTR     = syscall.EINTR
	EIO       = syscall.EIO
	EBADF     = syscall.EBADF
	EACCES    = syscall.EACCES
	EEXIST    = syscall.EEXIST
	ENOTDIR   = syscall.ENOTDIR
	EISDIR    = syscall.EISDIR
	EINVAL    = syscall.EINVAL
	ENOSPC    = syscall.ENOSPC
	EROFS     = syscall.EROFS
	EMFILE    = syscall.EMFILE
	ENFILE    = syscall.ENFILE
	ENOSYS    = syscall.ENOSYS
	ENOTSUP   = syscall.EOPNOTSUPP
	EOPNOTSUPP = syscall.EOPNOTSUPP
	EAFNOSUPPORT = syscall.EAFNOSUPPORT
	ENOPROTOOPT  = syscall.ENOPROTOOPT
	EPROTONOSUPPORT = syscall.EPROTONOSUPPORT
	EADDRINUSE   = syscall.EADDRINUSE
	EADDRNOTAVAIL = syscall.EADDRNOTAVAIL
	ECONNREFUSED = syscall.ECONNREFUSED
	ETIMEDOUT    = syscall.ETIMEDOUT
	EAGAIN       = syscall.EAGAIN
	EWOULDBLOCK  = syscall.EAGAIN
)

// NsecToTimespec converts nanoseconds to a Timespec.
func NsecToTimespec(nsec int64) (ts Timespec) {
	ts.Sec = nsec / 1e9
	ts.Nsec = nsec % 1e9
	return
}

// TimespecToNsec converts a Timespec to nanoseconds.
func TimespecToNsec(ts Timespec) int64 {
	return ts.Sec*1e9 + ts.Nsec
}

// NsecToTimeval converts nanoseconds to a Timeval.
func NsecToTimeval(nsec int64) (tv Timeval) {
	nsec += 999 // round up to microsecond
	tv.Sec = nsec / 1e9
	tv.Usec = int32((nsec % 1e9) / 1e3)
	return
}

// UtimesNanoAt sets the access and modification times of a file, with
// nanosecond resolution, relative to a directory file descriptor. Haiku's
// utimensat(2) serves directory-relative paths; for the common AT_FDCWD case
// UtimesNano provides the same result.
func UtimesNanoAt(dirfd int, path string, ts []Timespec, flags int) error {
	if dirfd == AT_FDCWD && flags == 0 {
		return syscall.UtimesNano(path, ts)
	}
	return ENOSYS
}

// Major returns the major component of a Haiku device number.
func Major(dev uint64) uint32 {
	return uint32((dev >> 8) & 0xff)
}

// Minor returns the minor component of a Haiku device number.
func Minor(dev uint64) uint32 {
	return uint32((dev & 0xff) | ((dev >> 16) & 0xffffff00))
}

// Mkdev composes a Haiku device number from major and minor.
func Mkdev(major, minor uint32) uint64 {
	return (uint64(major) << 8) & 0xff00 | (uint64(minor) & 0xff) | ((uint64(minor) & 0xffffff00) << 16)
}

// ==== Additions for the Engine (moby) port ====

// mmap and memory locking (Haiku libroot; Mlock family is ENOSYS there).
func Mmap(fd int, offset int64, length int, prot int, flags int) ([]byte, error) {
	return syscall.Mmap(fd, offset, length, prot, flags)
}

func Munmap(b []byte) error { return syscall.Munmap(b) }

func Mprotect(b []byte, prot int) error { return syscall.Mprotect(b, prot) }

func Madvise(b []byte, behav int) error { return syscall.Madvise(b, behav) }

func Mlock(b []byte) error      { return syscall.Mlock(b) }
func Munlock(b []byte) error    { return syscall.Munlock(b) }
func Mlockall(flags int) error  { return syscall.Mlockall(flags) }
func Munlockall() error         { return syscall.Munlockall() }

const (
	MAP_ANON      = syscall.MAP_ANON
	MAP_ANONYMOUS = syscall.MAP_ANONYMOUS
)

// File descriptor duplication and flags.
func Dup2(oldfd, newfd int) error { return syscall.Dup2(oldfd, newfd) }

func Dup3(oldfd, newfd, flags int) error { return syscall.Dup3(oldfd, newfd, flags) }

func CloseOnExec(fd int) {
	Fcntl(fd, F_SETFD, FD_CLOEXEC)
}

func Fcntl(fd int, cmd int, arg int) (int, error) { return syscall.Fcntl(fd, cmd, arg) }

// Socketpair creates a connected pair of sockets.
func Socketpair(domain, typ, proto int) ([2]int, error) {
	return syscall.Socketpair(domain, typ, proto)
}

// Stat and friends.
func Lstat(path string, st *Stat_t) error { return syscall.Lstat(path, st) }
func Stat(path string, st *Stat_t) error  { return syscall.Stat(path, st) }
func Fstat(fd int, st *Stat_t) error      { return syscall.Fstat(fd, st) }

// Flock applies or removes an advisory lock.
func Flock(fd int, how int) error { return syscall.Flock(fd, how) }

const (
	LOCK_EX = syscall.LOCK_EX
	LOCK_SH = syscall.LOCK_SH
	LOCK_UN = syscall.LOCK_UN
	LOCK_NB = syscall.LOCK_NB
)

// Mount flags, for packages that parse Linux-style option strings. Haiku
// mounts through fs_info rather than mount(2); the moby/sys/mount haiku
// backend reports these as unsupported at runtime.
// Mount attaches a file system; Haiku has no userland mount(2), so report
// ENOSYS until the Engine's mount backend exists.
func Mount(source string, target string, fstype string, flags uintptr, data string) error {
	return ENOSYS
}

// Unmount detaches a file system; Haiku has no POSIX unmount(2) exported
// to userland, so report ENOSYS until the Engine's mount backend exists.
func Unmount(target string, flags int) error { return ENOSYS }

const (
	MS_RDONLY       = 1
	MS_NOSUID       = 2
	MS_NODEV        = 4
	MS_NOEXEC       = 8
	MS_SYNCHRONOUS  = 16
	MS_REMOUNT      = 32
	MS_MANDLOCK     = 64
	MS_DIRSYNC      = 128
	MS_NOATIME      = 1024
	MS_NODIRATIME   = 2048
	MS_BIND         = 4096
	MS_MOVE         = 8192
	MS_REC          = 16384
	MS_SILENT       = 32768
	MS_POSIXACL     = 1 << 16
	MS_UNBINDABLE   = 1 << 17
	MS_PRIVATE      = 1 << 18
	MS_SLAVE        = 1 << 19
	MS_SHARED       = 1 << 20
	MS_RELATIME     = 1 << 21
	MS_KERNMOUNT    = 1 << 22
	MS_I_VERSION    = 1 << 23
	MS_STRICTATIME  = 1 << 24
	MS_LAZYTIME     = 1 << 25
	MS_ACTIVE       = 1 << 30
	MS_NOUSER       = 1 << 31
	MNT_DETACH      = 2
	MNT_EXPIRE      = 4
	MNT_FORCE       = 1
	UMOUNT_NOFOLLOW = 8
)

// Uname returns system information (Haiku's struct utsname).
func Uname(buf *Utsname) error { return syscall.Uname(buf) }

// ByteSliceToString converts a NUL-terminated byte slice to a Go string.
func ByteSliceToString(b []byte) string {
	n := 0
	for n < len(b) && b[n] != 0 {
		n++
	}
	return string(b[:n])
}

const (
	MADV_NORMAL     = syscall.MADV_NORMAL
	MADV_RANDOM     = syscall.MADV_RANDOM
	MADV_SEQUENTIAL = syscall.MADV_SEQUENTIAL
	MADV_WILLNEED   = syscall.MADV_WILLNEED
	MADV_DONTNEED   = syscall.MADV_DONTNEED
	MADV_FREE       = syscall.MADV_FREE
)

// Statfs returns file system statistics; Haiku reports ENOSYS for the
// POSIX statfs shape, which callers treat as "not a cgroup fs".
func Statfs(path string, buf *Statfs_t) error { return syscall.Statfs(path, buf) }

const CGROUP2_SUPER_MAGIC = 0x63677270

// Access and process control additions.
func Access(path string, mode uint32) error { return syscall.Access(path, mode) }

func Kill(pid int, sig Signal) error { return syscall.Kill(pid, syscall.Signal(sig)) }

const ESRCH = syscall.ESRCH

// More errno and attribute symbols used by containerd and fsutil.
const (
	EBUSY    = syscall.EBUSY
	ENODATA  = syscall.ENOTSUP
)

const XATTR_CREATE = 1

// Lsetxattr sets an extended attribute; Haiku's BFS attributes are not
// exposed through the POSIX xattr API in libroot, so report ENOSYS.
func Lsetxattr(path string, attr string, data []byte, flags int) error {
	return ENOSYS
}

// TimeToTimespec converts a time.Time to a Timespec (the fsutil copy
// backend expects an error return for parity with newer x/sys releases).
func TimeToTimespec(t time.Time) (Timespec, error) {
	return Timespec{Sec: t.Unix(), Nsec: int64(t.Nanosecond())}, nil
}

// WaitStatus mirrors x/sys/unix's distinct wait-status type (it is not an
// alias of syscall.WaitStatus upstream).
type WaitStatus uint32

func (w WaitStatus) Exited() bool          { return syscall.WaitStatus(w).Exited() }
func (w WaitStatus) ExitStatus() int       { return syscall.WaitStatus(w).ExitStatus() }
func (w WaitStatus) Signaled() bool        { return syscall.WaitStatus(w).Signaled() }
func (w WaitStatus) Signal() Signal        { return Signal(syscall.WaitStatus(w).Signal()) }
func (w WaitStatus) CoreDump() bool        { return syscall.WaitStatus(w).CoreDump() }
func (w WaitStatus) Stopped() bool         { return syscall.WaitStatus(w).Stopped() }
func (w WaitStatus) Continued() bool       { return syscall.WaitStatus(w).Continued() }
func (w WaitStatus) StopSignal() Signal    { return Signal(syscall.WaitStatus(w).StopSignal()) }
func (w WaitStatus) TrapCause() int        { return syscall.WaitStatus(w).TrapCause() }

// Socket control message helpers for passing file descriptors
// (SCM_RIGHTS); reexported from package syscall.
type SocketControlMessage = syscall.SocketControlMessage

func Unlink(path string) error { return syscall.Unlink(path) }

func CmsgSpace(l int) int { return syscall.CmsgSpace(l) }

func ParseSocketControlMessage(b []byte) ([]SocketControlMessage, error) {
	return syscall.ParseSocketControlMessage(b)
}

func ParseUnixRights(m *SocketControlMessage) ([]int, error) {
	return syscall.ParseUnixRights(m)
}

// Process reaping additions.
const (
	WNOHANG = syscall.WNOHANG
	ECHILD  = syscall.ECHILD
)

// Wait4 waits for a process and returns its resource usage.
func Wait4(pid int, wstatus *WaitStatus, options int, rusage *Rusage) (wpid int, err error) {
	return syscall.Wait4(pid, (*syscall.WaitStatus)(wstatus), options, rusage)
}
