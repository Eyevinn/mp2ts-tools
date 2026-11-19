//go:build unix

package internal

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// mapFile maps a whole file read-only.
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
	if st.Size() == 0 {
		return nil, func() {}, nil
	}
	if int64(int(st.Size())) != st.Size() {
		return nil, nil, fmt.Errorf("%q is too large to map", path)
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_PRIVATE)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot map %q: %w", path, err)
	}
	return data, func() { _ = syscall.Munmap(data) }, nil
}

// setMulticastTTL sets the TTL of multicast datagrams sent on conn.
func setMulticastTTL(conn net.Conn, ttl int) error {
	uc, ok := conn.(*net.UDPConn)
	if !ok {
		return nil
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_TTL, ttl)
	}); err != nil {
		return err
	}
	if serr != nil {
		return fmt.Errorf("set multicast TTL %d: %w", ttl, serr)
	}
	return nil
}
