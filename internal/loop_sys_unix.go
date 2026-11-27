//go:build unix

package internal

import (
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
)

// mapFile maps a whole file read-only. What cannot be mapped, such as a named
// pipe or /dev/stdin, is read instead.
func mapFile(path string) ([]byte, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if st.Mode().IsRegular() && st.Size() > 0 {
		if int64(int(st.Size())) != st.Size() {
			return nil, nil, fmt.Errorf("%q is too large to map", path)
		}
		data, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_PRIVATE)
		if err == nil {
			return data, func() { _ = syscall.Munmap(data) }, nil
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read %q: %w", path, err)
	}
	return data, func() {}, nil
}

// setMulticastTTL sets the TTL (IPv6: hop limit) of the datagrams sent on conn
// when its destination is a multicast group. A unicast destination has none.
func setMulticastTTL(conn net.Conn, ttl int) error {
	uc, ok := conn.(*net.UDPConn)
	if !ok {
		return nil
	}
	remote, ok := uc.RemoteAddr().(*net.UDPAddr)
	if !ok || !remote.IP.IsMulticast() {
		return nil
	}
	// The option goes by the socket's address family.
	level, option := syscall.IPPROTO_IP, syscall.IP_MULTICAST_TTL
	if remote.IP.To4() == nil {
		level, option = syscall.IPPROTO_IPV6, syscall.IPV6_MULTICAST_HOPS
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), level, option, ttl)
	}); err != nil {
		return err
	}
	if serr != nil {
		return fmt.Errorf("set multicast TTL %d: %w", ttl, serr)
	}
	return nil
}
