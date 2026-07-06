package gogohttp

import (
	"sync/atomic"
	"time"
)

// HTTP requires a Date header on every response. Computing it per-request
// shows up in flame graphs (time.Now + format = ~80ns); since the date is
// only second-resolution, one goroutine refreshes it once a second and every
// bouncer reads the same atomically-published []byte. After warm-up, the
// pointer is stable across loads, so cache-coherency cost is negligible.

var currentDate atomic.Value // holds []byte

func init() {
	currentDate.Store(formatDate(time.Now()))
	go dateLoop()
}

func dateLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for now := range t.C {
		currentDate.Store(formatDate(now))
	}
}

func formatDate(t time.Time) []byte {
	// IMF-fixdate per RFC 7231 §7.1.1.1, plus the "Date: " prefix and CRLF
	// terminator, so it can be dropped straight into a writev as one segment.
	return []byte("Date: " + t.UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT") + "\r\n")
}

// CurrentDate returns the cached "Date: ...\r\n" line. Refreshed once/sec.
// The returned slice must NOT be modified.
func CurrentDate() []byte {
	return currentDate.Load().([]byte)
}
