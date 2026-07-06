package gogohttp

import (
	"bufio"
	"net/http"
	"testing"
)

func TestSugarNotFound(t *testing.T) {
	h := HandlerFunc(func(req *Req, resp *Resp) {
		resp.NotFound()
	})
	addr, stop := startServer(t, h, DefaultRegistry())
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	c.Write([]byte("GET /missing HTTP/1.1\r\nHost: x\r\n\r\n"))
	resp, _ := http.ReadResponse(bufio.NewReader(c), nil)
	if resp.StatusCode != 404 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestSugarRedirect(t *testing.T) {
	h := HandlerFunc(func(req *Req, resp *Resp) {
		resp.Redirect("/new", false)
	})
	addr, stop := startServer(t, h, DefaultRegistry())
	defer stop()

	c := dial(t, addr)
	defer c.Close()
	c.Write([]byte("GET /old HTTP/1.1\r\nHost: x\r\n\r\n"))
	resp, _ := http.ReadResponse(bufio.NewReader(c), nil)
	if resp.StatusCode != 302 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/new" {
		t.Fatalf("Location=%q", loc)
	}
}

func TestDefaultRegistryStatusOnly(t *testing.T) {
	r := DefaultRegistry()
	if len(r) != int(HeadIDUserStart) {
		t.Fatalf("registry size=%d want=%d", len(r), HeadIDUserStart)
	}
	// All status entries populated.
	for _, id := range []uint16{
		HeadID101_WSUpgrade, HeadID204, HeadID301, HeadID302, HeadID304,
		HeadID400, HeadID401, HeadID403, HeadID404, HeadID405,
		HeadID500, HeadID503,
	} {
		if r[id] == nil {
			t.Errorf("missing entry for HeadID %d", id)
		}
	}
}

func TestRegistryBuilder(t *testing.T) {
	rb := NewRegistryBuilder()

	id1 := rb.AddMime("text/html")
	id2 := rb.AddMime("application/json")
	id3 := rb.AddMime("text/html") // duplicate
	if id1 != id3 {
		t.Errorf("AddMime not idempotent: %d vs %d", id1, id3)
	}
	if id1 == id2 {
		t.Errorf("distinct mimes got same ID")
	}
	if id1 < HeadIDUserStart {
		t.Errorf("mime IDs should be >= HeadIDUserStart, got %d", id1)
	}

	if rb.MimeID("text/html") != id1 {
		t.Error("MimeID lookup failed")
	}
	if rb.MimeID("nonexistent/mime") != 0 {
		t.Error("MimeID for absent mime should be 0")
	}

	custom := rb.Add([]byte("HTTP/1.1 200 OK\r\nServer: Gogogo\r\nX-Custom: yes\r\n"))
	if custom <= id2 {
		t.Errorf("Add should return higher ID, got %d after %d", custom, id2)
	}

	final := rb.Build()
	if len(final) <= int(HeadIDUserStart) {
		t.Fatalf("Build returned undersized registry: %d", len(final))
	}
	if final[id1] == nil {
		t.Error("mime entry not in built registry")
	}
}
