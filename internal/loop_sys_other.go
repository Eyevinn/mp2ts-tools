//go:build !unix

package internal

import (
	"fmt"
	"net"
	"os"
)

// mapFile reads a whole file.
func mapFile(path string) ([]byte, func(), error) {
	data, err := os.ReadFile(path)
	return data, func() {}, err
}

// setMulticastTTL supports only the default TTL of 1 here.
func setMulticastTTL(_ net.Conn, ttl int) error {
	if ttl != 1 {
		return fmt.Errorf("setting the multicast TTL is not supported on this platform")
	}
	return nil
}
