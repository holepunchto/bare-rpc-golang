package bare_rpc

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

// newPair wires two RPC endpoints over an in-memory duplex pipe and starts a
// Listen loop on the server side with the given handler.
func newPair(t *testing.T, onRequest func(*Request)) (client *RPC, server *RPC) {
	t.Helper()
	c1, c2 := net.Pipe()
	client = NewRPC(c1)
	server = NewRPC(c2)

	go func() { _ = server.Listen(onRequest) }()
	go func() { _ = client.Listen(nil) }() // client still needs its read loop for responses/streams

	t.Cleanup(func() { c1.Close(); c2.Close() })
	return client, server
}

func TestUnaryRequest(t *testing.T) {
	client, _ := newPair(t, func(req *Request) {
		if req.Command != 7 {
			t.Errorf("command = %d, want 7", req.Command)
		}
		req.Reply(append([]byte("echo:"), req.Data...))
	})

	got, err := client.Request(7, []byte("ping"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "echo:ping" {
		t.Fatalf("got %q, want %q", got, "echo:ping")
	}
}

func TestRequestStream(t *testing.T) {
	client, _ := newPair(t, func(req *Request) {
		rs := req.CreateRequestStream()
		data, err := io.ReadAll(rs)
		if err != nil {
			t.Errorf("ReadAll: %v", err)
		}
		req.Reply(append([]byte("got:"), data...))
	})

	req := client.NewRequest(42)
	ws := req.CreateRequestStream()

	go func() {
		ws.Write([]byte("foo"))
		ws.Write([]byte("bar"))
		ws.Close()
	}()

	got, err := req.Reply()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "got:foobar" {
		t.Fatalf("got %q, want %q", got, "got:foobar")
	}
}

func TestResponseStream(t *testing.T) {
	client, _ := newPair(t, func(req *Request) {
		if string(req.Data) != "go" {
			t.Errorf("data = %q, want %q", req.Data, "go")
		}
		rs := req.CreateResponseStream()
		rs.Write([]byte("chunk1"))
		rs.Write([]byte("chunk2"))
		rs.Close()
	})

	req := client.NewRequest(42)
	if err := req.Send([]byte("go")); err != nil {
		t.Fatal(err)
	}
	rs := req.CreateResponseStream()

	got, err := io.ReadAll(rs)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "chunk1chunk2" {
		t.Fatalf("got %q, want %q", got, "chunk1chunk2")
	}
}

func TestRequestAndResponseStream(t *testing.T) {
	client, _ := newPair(t, func(req *Request) {
		in := req.CreateRequestStream()
		out := req.CreateResponseStream()
		go func() {
			data, _ := io.ReadAll(in)
			out.Write(append([]byte("reply:"), data...))
			out.Close()
		}()
	})

	req := client.NewRequest(42)
	out := req.CreateResponseStream()
	in := req.CreateRequestStream()

	go func() {
		in.Write([]byte("hello"))
		in.Close()
	}()

	got, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "reply:hello" {
		t.Fatalf("got %q, want %q", got, "reply:hello")
	}
}

// TestBackpressure pushes far more than the high-water mark through a slow
// reader and confirms every byte arrives intact, exercising PAUSE/RESUME.
func TestBackpressure(t *testing.T) {
	const chunks = 200
	payload := bytes.Repeat([]byte("x"), 1024) // 200 KiB total, well over high-water

	client, _ := newPair(t, func(req *Request) {
		rs := req.CreateResponseStream()
		go func() {
			for i := 0; i < chunks; i++ {
				if _, err := rs.Write(payload); err != nil {
					t.Errorf("write %d: %v", i, err)
					return
				}
			}
			rs.Close()
		}()
	})

	req := client.NewRequest(1)
	req.Send(nil)
	rs := req.CreateResponseStream()

	var total int
	buf := make([]byte, 4096)
	for {
		n, err := rs.Read(buf)
		total += n
		time.Sleep(time.Millisecond) // slow reader -> forces the writer to pause
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if total != chunks*len(payload) {
		t.Fatalf("received %d bytes, want %d", total, chunks*len(payload))
	}
}
