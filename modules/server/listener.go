// File: modules/server/listener.go

package server

import (
	"log"
	"net"
	"syscall"
	"time"
)

const (
	tcpFastOpen     = 23  // TCP_FASTOPEN value for Linux
	tcpFastOpenQlen = 256 // Queue length for TFO
)

type tcpKeepAliveListener struct {
	*net.TCPListener
	keepAlivePeriod time.Duration
}

func EnableFastOpen(ln net.Listener) {
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		return
	}

	f, err := tl.File()
	if err != nil {
		return
	}
	defer f.Close()

	fd := int(f.Fd())
	if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, tcpFastOpen, tcpFastOpenQlen); err != nil {
		log.Printf("TCP Fast Open enable failed: %v", err)
	}
}

func (ln *tcpKeepAliveListener) Accept() (net.Conn, error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return nil, err
	}

	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(ln.keepAlivePeriod)
	tc.SetNoDelay(true)

	// Divine Speed: Optimal buffer sizes
	tc.SetReadBuffer(64 * 1024)
	tc.SetWriteBuffer(64 * 1024)

	return tc, nil
}
