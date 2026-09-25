package httpx

import (
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"net/netip"
)

// Conn is the connection abstraction used by the server core. It is backed by
// a non-blocking socket registered with the Go runtime poller (os.File), which
// gives real read/write deadlines without importing package net.
type Conn interface {
	io.Reader
	io.Writer
	io.Closer
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	RemoteAddr() string
}

type sockConn struct {
	f     *os.File
	fd    int
	raddr string
}

func (c *sockConn) Read(p []byte) (int, error)  { return c.f.Read(p) }
func (c *sockConn) Write(p []byte) (int, error) { return c.f.Write(p) }
func (c *sockConn) Close() error                { return c.f.Close() }

func (c *sockConn) SetReadDeadline(t time.Time) error  { return c.f.SetReadDeadline(t) }
func (c *sockConn) SetWriteDeadline(t time.Time) error { return c.f.SetWriteDeadline(t) }
func (c *sockConn) RemoteAddr() string                 { return c.raddr }

// isTimeout reports whether err is a deadline expiry (os.ErrDeadlineExceeded),
// replacing net.Error.Timeout() from the previous implementation.
func isTimeout(err error) bool { return errors.Is(err, os.ErrDeadlineExceeded) }

// Listener is a minimal TCP listener (replaces net.Listener).
type Listener struct {
	fd     int
	addr   string // announced bind address ("host:port" or ":port")
	port   int
	wake   string // address to dial in order to unblock Accept on Close
	closed bool
}

// Listen parses host:port (host may be empty, "localhost" or an IP literal;
// DNS names are deliberately not supported to avoid pulling package net) and
// starts listening. An empty host binds to all interfaces (dual-stack when
// possible).
func Listen(addr string) (*Listener, error) {
	host, portStr, err := splitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := parsePort(portStr)
	if err != nil {
		return nil, err
	}
	switch {
	case host == "":
		// Dual-stack wildcard with IPv4 fallback.
		fd, err := bindWildcard(port)
		if err == nil {
			return newListener(fd, addr, port, "127.0.0.1")
		}
		fd4, err4 := bindHost(netip.AddrFrom4([4]byte{}), port, false)
		if err4 != nil {
			return nil, err
		}
		return newListener(fd4, addr, port, "127.0.0.1")
	case strings.EqualFold(host, "localhost"):
		return listenHost(netip.AddrFrom4([4]byte{127, 0, 0, 1}), addr, port)
	default:
		a, err := netip.ParseAddr(host)
		if err != nil {
			return nil, errors.New("httpx: listen address host must be an IP literal or \"localhost\" (no DNS)")
		}
		return listenHost(a, addr, port)
	}
}

func listenHost(a netip.Addr, addr string, port int) (*Listener, error) {
	fd, err := bindHost(a, port, a.Is6() && !a.Is4())
	if err != nil {
		return nil, err
	}
	wake := a.String()
	if a.Is4In6() {
		wake = a.Unmap().String()
	}
	if a.IsUnspecified() {
		wake = "127.0.0.1"
	}
	return newListener(fd, addr, port, wake)
}

func newListener(fd int, addr string, port int, wake string) (*Listener, error) {
	return &Listener{fd: fd, addr: addr, port: port, wake: wake}, nil
}

// bindWildcard listens on [::] with dual-stack when the kernel allows it.
func bindWildcard(port int) (int, error) {
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM, 0)
	if err != nil {
		return -1, err
	}
	if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 0); err != nil {
		// Best effort: some kernels refuse; keep whatever default they have.
		_ = err
	}
	sa := &syscall.SockaddrInet6{Port: port} // unspecified ::
	if err := bindListen(fd, sa); err != nil {
		syscall.Close(fd)
		return -1, err
	}
	return fd, nil
}

func bindHost(a netip.Addr, port int, is6 bool) (int, error) {
	if is6 {
		fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM, 0)
		if err != nil {
			return -1, err
		}
		var sa syscall.Sockaddr = &syscall.SockaddrInet6{Port: port}
		s6 := sa.(*syscall.SockaddrInet6)
		copy(s6.Addr[:], a.AsSlice())
		if err := bindListen(fd, s6); err != nil {
			syscall.Close(fd)
			return -1, err
		}
		return fd, nil
	}
	a = a.Unmap()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return -1, err
	}
	s4 := &syscall.SockaddrInet4{Port: port}
	copy(s4.Addr[:], a.AsSlice())
	if err := bindListen(fd, s4); err != nil {
		syscall.Close(fd)
		return -1, err
	}
	return fd, nil
}

func bindListen(fd int, sa syscall.Sockaddr) error {
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		return err
	}
	if err := syscall.Bind(fd, sa); err != nil {
		return err
	}
	return syscall.Listen(fd, 128)
}

// Addr returns the bound address string.
func (l *Listener) Addr() string { return l.addr }

// Accept blocks until a connection arrives or the listener is closed.
func (l *Listener) Accept() (Conn, error) {
	for {
		nfd, sa, err := syscall.Accept(l.fd)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return nil, err
		}
		if err := syscall.SetNonblock(nfd, true); err != nil {
			syscall.Close(nfd)
			return nil, err
		}
		f := os.NewFile(uintptr(nfd), "sock")
		if f == nil {
			syscall.Close(nfd)
			return nil, errors.New("httpx: os.NewFile failed")
		}
		return &sockConn{f: f, fd: nfd, raddr: sockaddrString(sa)}, nil
	}
}

// Close stops the listener and unblocks a pending Accept via a wake-up
// self-connection (closing a fd does not wake a blocked accept on Linux).
func (l *Listener) Close() error {
	if l.closed {
		return nil
	}
	l.closed = true
	if l.wake != "" && l.port != 0 {
		if c, err := dialWake(l.wake, l.port); err == nil {
			_ = c.Close()
		}
	}
	return syscall.Close(l.fd)
}

func dialWake(host string, port int) (io.Closer, error) {
	a, err := netip.ParseAddr(host)
	if err != nil {
		return nil, errors.New("httpx: bad wake address")
	}
	var (
		fd int
		sa syscall.Sockaddr
	)
	if a.Is4() {
		fd, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
		if err != nil {
			return nil, err
		}
		sa = &syscall.SockaddrInet4{Port: port}
		copy(sa.(*syscall.SockaddrInet4).Addr[:], a.AsSlice())
	} else {
		fd, err = syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM, 0)
		if err != nil {
			return nil, err
		}
		sa = &syscall.SockaddrInet6{Port: port}
		copy(sa.(*syscall.SockaddrInet6).Addr[:], a.AsSlice())
	}
	// SOCK_NONBLOCK is Linux-only; SetNonblock (fcntl/FIONBIO) is portable
	// and keeps connect() returning EINPROGRESS instead of blocking.
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	if err := syscall.Connect(fd, sa); err != nil && err != syscall.EINPROGRESS {
		syscall.Close(fd)
		return nil, err
	}
	return &wakeConn{fd: fd}, nil
}

type wakeConn struct{ fd int }

func (w *wakeConn) Close() error { return syscall.Close(w.fd) }

func splitHostPort(addr string) (host, port string, err error) {
	if addr == "" {
		return "", "", errors.New("httpx: empty listen address")
	}
	if addr[0] == '[' {
		i := strings.LastIndexByte(addr, ']')
		if i < 0 || i+1 >= len(addr) || addr[i+1] != ':' {
			return "", "", errors.New("httpx: malformed listen address")
		}
		return addr[1:i], addr[i+2:], nil
	}
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return "", "", errors.New("httpx: missing port in listen address")
	}
	return addr[:i], addr[i+1:], nil
}

func parsePort(s string) (int, error) {
	if s == "" {
		return 0, errors.New("httpx: missing port")
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, errors.New("httpx: bad port")
		}
		n = n*10 + int(c-'0')
		if n > 65535 {
			return 0, errors.New("httpx: port out of range")
		}
	}
	return n, nil
}

func sockaddrString(sa syscall.Sockaddr) string {
	switch a := sa.(type) {
	case *syscall.SockaddrInet4:
		var ip [4]byte
		copy(ip[:], a.Addr[:])
		return netip.AddrFrom4(ip).String() + ":" + itoa(a.Port)
	case *syscall.SockaddrInet6:
		var ip [16]byte
		copy(ip[:], a.Addr[:])
		return "[" + netip.AddrFrom16(ip).String() + "]:" + itoa(a.Port)
	default:
		return "unknown"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
