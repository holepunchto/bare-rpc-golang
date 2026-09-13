package bare_rpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// newPair wires two endpoints over net.Pipe with read loops on both sides.
func newPair(t *testing.T, onRequest func(*Request) error) (client *RPC, server *RPC) {
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
	client, _ := newPair(t, func(req *Request) error {
		if req.Command != 7 {
			t.Errorf("command = %d, want 7", req.Command)
		}
		req.Reply(append([]byte("echo:"), req.Data...))
		return nil
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
	client, _ := newPair(t, func(req *Request) error {
		rs := req.CreateRequestStream()
		data, err := io.ReadAll(rs)
		if err != nil {
			t.Errorf("ReadAll: %v", err)
		}
		req.Reply(append([]byte("got:"), data...))
		return nil
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
	client, _ := newPair(t, func(req *Request) error {
		if string(req.Data) != "go" {
			t.Errorf("data = %q, want %q", req.Data, "go")
		}
		rs := req.CreateResponseStream()
		rs.Write([]byte("chunk1"))
		rs.Write([]byte("chunk2"))
		rs.Close()
		return nil
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
	client, _ := newPair(t, func(req *Request) error {
		in := req.CreateRequestStream()
		out := req.CreateResponseStream()
		go func() {
			data, _ := io.ReadAll(in)
			out.Write(append([]byte("reply:"), data...))
			out.Close()
		}()
		return nil
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

// TestBackpressure pushes far more than the high-water mark through a slow reader.
func TestBackpressure(t *testing.T) {
	const chunks = 200
	payload := bytes.Repeat([]byte("x"), 1024) // 200 KiB total, well over high-water

	client, _ := newPair(t, func(req *Request) error {
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
		return nil
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

// waitIdle polls Idle, since registry cleanup can trail the consumer by a moment.
func waitIdle(t *testing.T, name string, r *RPC) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !r.Idle() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not idle", name)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestErrorReplyCarriesCodeAndErrno(t *testing.T) {
	client, _ := newPair(t, func(req *Request) error {
		req.ReplyError(fmt.Errorf("wrapped: %w", &RPCError{Message: "nope", Code: "NOPE", Errno: 3}))
		return nil
	})

	_, err := client.Request(1, nil)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("err = %v, want *RPCError", err)
	}
	if rpcErr.Message != "nope" || rpcErr.Code != "NOPE" || rpcErr.Errno != 3 {
		t.Fatalf("got %+v", rpcErr)
	}
}

// An error reply must fail the initiator's response stream too.
func TestErrorReplyAbortsResponseStream(t *testing.T) {
	client, _ := newPair(t, func(req *Request) error {
		req.ReplyError(errors.New("boom"))
		return nil
	})

	req := client.NewRequest(1)
	rs := req.CreateResponseStream() // register the reader before the request can be answered
	if err := req.Send(nil); err != nil {
		t.Fatal(err)
	}

	_, err := io.ReadAll(rs)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v, want boom", err)
	}
	waitIdle(t, "client", client)
}

func TestIncomingDestroyCarriesError(t *testing.T) {
	writerErr := make(chan error, 1)
	client, _ := newPair(t, func(req *Request) error {
		out := req.CreateResponseStream()
		for {
			if _, err := out.Write([]byte("more")); err != nil {
				writerErr <- err
				return nil
			}
		}
	})

	req := client.NewRequest(1)
	req.Send(nil)
	rs := req.CreateResponseStream()
	if _, err := rs.Read(make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	rs.Destroy(&RPCError{Message: "enough", Code: "ENOUGH"})

	var rpcErr *RPCError
	if err := <-writerErr; !errors.As(err, &rpcErr) || rpcErr.Code != "ENOUGH" {
		t.Fatalf("writer err = %v, want ENOUGH", err)
	}
}

func TestIdle(t *testing.T) {
	client, server := newPair(t, func(req *Request) error {
		switch req.Command {
		case 1:
			req.Reply([]byte("pong"))
		case 2:
			out := req.CreateResponseStream()
			out.Write([]byte("a"))
			out.Close()
		case 3:
			in := req.CreateRequestStream()
			data, _ := io.ReadAll(in)
			req.Reply(data)
		}
		return nil
	})

	if _, err := client.Request(1, nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, "client after reply", client)
	waitIdle(t, "server after reply", server)

	req := client.NewRequest(2)
	req.Send(nil)
	if _, err := io.ReadAll(req.CreateResponseStream()); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, "client after response stream", client)
	waitIdle(t, "server after response stream", server)

	req = client.NewRequest(3)
	ws := req.CreateRequestStream()
	go func() {
		ws.Write([]byte("x"))
		ws.Close()
	}()
	if _, err := req.Reply(); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, "client after request stream", client)
	waitIdle(t, "server after request stream", server)
}

func TestTeardownFailsPendingOnClose(t *testing.T) {
	c1, c2 := net.Pipe()
	client := NewRPC(c1)
	go client.Listen(nil)
	go NewRPC(c2).Listen(func(*Request) error { return nil }) // drains the request, never replies
	t.Cleanup(func() { c1.Close(); c2.Close() })

	req := client.NewRequest(1)
	if err := req.Send(nil); err != nil {
		t.Fatal(err)
	}
	c2.Close()

	if _, err := req.Reply(); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("err = %v, want ErrChannelClosed", err)
	}
	waitIdle(t, "client", client)
}

func TestTeardownAbortsStreamsOnClose(t *testing.T) {
	c1, c2 := net.Pipe()
	client := NewRPC(c1)
	go client.Listen(nil)
	go NewRPC(c2).Listen(func(req *Request) error {
		req.CreateRequestStream()  // ack the request stream, then never read
		req.CreateResponseStream() // open the response stream, then never write
		return nil
	})
	t.Cleanup(func() { c1.Close(); c2.Close() })

	req := client.NewRequest(1)
	rs := req.CreateResponseStream()
	ws := req.CreateRequestStream()
	if _, err := ws.Write([]byte("hello")); err != nil { // returns once the OPEN handshake is done
		t.Fatal(err)
	}
	c2.Close()

	if _, err := io.ReadAll(rs); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("read err = %v, want ErrChannelClosed", err)
	}
	if _, err := ws.Write([]byte("x")); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("write err = %v, want ErrChannelClosed", err)
	}
	waitIdle(t, "client", client)
}

func TestRequestsAfterCloseFail(t *testing.T) {
	c1, c2 := net.Pipe()
	client := NewRPC(c1)
	done := make(chan error, 1)
	go func() { done <- client.Listen(nil) }()
	t.Cleanup(func() { c1.Close() })

	c2.Close()
	if err := <-done; err != io.EOF {
		t.Fatalf("Listen = %v, want io.EOF", err)
	}

	req := client.NewRequest(1)
	if err := req.Send(nil); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("Send = %v, want ErrChannelClosed", err)
	}
	if _, err := req.Reply(); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("Reply = %v, want ErrChannelClosed", err)
	}
	if _, err := client.NewRequest(2).CreateRequestStream().Write([]byte("x")); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("Write = %v, want ErrChannelClosed", err)
	}
	if _, err := client.NewRequest(3).CreateResponseStream().Read(make([]byte, 1)); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("Read = %v, want ErrChannelClosed", err)
	}
	if !client.Idle() {
		t.Fatal("client not idle")
	}
}

func TestHandlerErrorBecomesErrorReply(t *testing.T) {
	client, _ := newPair(t, func(req *Request) error {
		return &RPCError{Message: "nope", Code: "NOPE"}
	})

	_, err := client.Request(1, nil)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != "NOPE" {
		t.Fatalf("err = %v, want NOPE", err)
	}
}

func TestHandlerErrorAfterReplyIsNotResent(t *testing.T) {
	client, _ := newPair(t, func(req *Request) error {
		req.Reply([]byte("ok"))
		return errors.New("too late")
	})

	got, err := client.Request(1, nil)
	if err != nil || string(got) != "ok" {
		t.Fatalf("got %q, %v", got, err)
	}
	// A second request proves nothing stray was delivered.
	if got, err = client.Request(1, nil); err != nil || string(got) != "ok" {
		t.Fatalf("second: got %q, %v", got, err)
	}
}

func TestEventHandlerErrorFailsChannel(t *testing.T) {
	c1, c2 := net.Pipe()
	client := NewRPC(c1)
	server := NewRPC(c2)
	serverDone := make(chan error, 1)
	clientDone := make(chan error, 1)
	go func() { clientDone <- client.Listen(nil) }()
	go func() {
		serverDone <- server.Listen(func(req *Request) error {
			if !req.IsEvent() {
				t.Errorf("expected an event, got id %d", req.ID)
			}
			if err := req.Reply(nil); err != ErrEventReply {
				t.Errorf("Reply on event = %v, want ErrEventReply", err)
			}
			return errors.New("bad event")
		})
	}()
	t.Cleanup(func() { c1.Close(); c2.Close() })

	if err := client.Event(5, []byte("x")); err != nil {
		t.Fatal(err)
	}

	// The server tears down and closes its transport, so both read loops end.
	if err := <-serverDone; err == nil {
		t.Fatal("server Listen returned nil")
	}
	if err := <-clientDone; err != io.EOF {
		t.Fatalf("client Listen = %v, want io.EOF", err)
	}
	if !server.Idle() {
		t.Fatal("server not idle")
	}
	if _, err := client.Request(1, nil); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("client Request = %v, want ErrChannelClosed", err)
	}
}

func TestFrameTooLarge(t *testing.T) {
	c1, c2 := net.Pipe()
	client := NewRPC(c1)
	client.MaxFrameSize = 8
	done := make(chan error, 1)
	go func() { done <- client.Listen(nil) }()
	t.Cleanup(func() { c1.Close(); c2.Close() })

	// A 9-byte frame: one over the limit.
	go c2.Write([]byte{9, 0, 0, 0, 1, 1, 0, 0, 0, 0, 0, 0, 0})

	if err := <-done; !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("Listen = %v, want ErrFrameTooLarge", err)
	}
}

func TestReadChunkPreservesBoundaries(t *testing.T) {
	client, _ := newPair(t, func(req *Request) error {
		out := req.CreateResponseStream()
		out.Write([]byte("one"))
		out.Write([]byte("two two"))
		out.Write([]byte("3"))
		return out.Close()
	})

	req := client.NewRequest(1)
	req.Send(nil)
	rs := req.CreateResponseStream()

	var got []string
	for {
		chunk, err := rs.ReadChunk()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, string(chunk))
	}
	want := []string{"one", "two two", "3"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func TestReplyContextCancel(t *testing.T) {
	client, _ := newPair(t, func(req *Request) error { return nil }) // never replies

	req := client.NewRequest(1)
	if err := req.Send(nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := req.ReplyContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if !client.Idle() {
		t.Fatal("cancelled request still pending")
	}
}
