package server

import (
	"context"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// NewReusePortListener creates a TCP listener with SO_REUSEPORT enabled.
// This allows multiple bouncers (processes/threads) to listen on the same port.
func NewReusePortListener(network, address string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var err error
			c.Control(func(fd uintptr) {
				// SO_REUSEPORT (TCP level load balancing)
				err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
				if err != nil {
					return
				}
				// SO_REUSEADDR (Fast restart)
				err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			})
			return err
		},
	}
	return lc.Listen(context.Background(), network, address)
}
