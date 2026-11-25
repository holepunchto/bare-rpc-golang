package barerpc_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	barerpc "github.com/holepunchto/bare-rpc-golang/v2"
)

func pair(t *testing.T, handler barerpc.Handler) (client, server *barerpc.RPC) {
	t.Helper()

	a, b := net.Pipe()

	server = barerpc.New(b, &barerpc.Options{Handler: handler})
	client = barerpc.New(a, nil)

	t.Cleanup(func() {
		client.Close()
		server.Close()
	})

	return client, server
}

// A peer that speaks the wire format by hand.
type peer struct {
	t    *testing.T
	conn net.Conn
}

func withPeer(t *testing.T, opts *barerpc.Options) (*barerpc.RPC, peer) {
	t.Helper()

	a, b := net.Pipe()

	r := barerpc.New(a, opts)

	t.Cleanup(func() { r.Close() })
	t.Cleanup(func() { b.Close() })

	return r, peer{t, b}
}

func (p peer) send(frame string) {
	p.t.Helper()

	b, _ := hex.DecodeString(frame)

	if _, err := p.conn.Write(b); err != nil {
		p.t.Fatal(err)
	}
}

func (p peer) expect(want string) {
	p.t.Helper()

	header := make([]byte, 4)

	if _, err := io.ReadFull(p.conn, header); err != nil {
		p.t.Fatal(err)
	}

	body := make([]byte, binary.LittleEndian.Uint32(header))

	if _, err := io.ReadFull(p.conn, body); err != nil {
		p.t.Fatal(err)
	}

	if got := hex.EncodeToString(append(header, body...)); got != want {
		p.t.Fatalf("got frame %s, want %s", got, want)
	}
}

func (p peer) drain() {
	go io.Copy(io.Discard, p.conn)
}

func asError(t *testing.T, err error) *barerpc.Error {
	t.Helper()

	var e *barerpc.Error

	if !errors.As(err, &e) {
		t.Fatalf("got %v, want an *Error", err)
	}

	return e
}

func echo() *barerpc.Router {
	var router barerpc.Router

	router.OnRequest(7, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
		return append([]byte("echo:"), req.Data...), nil
	})

	return &router
}

func TestRequest(t *testing.T) {
	client, server := pair(t, echo())

	got, err := client.Request(context.Background(), 7, []byte("ping"))
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != "echo:ping" {
		t.Fatalf("got %q", got)
	}

	if !client.Idle() || !server.Idle() {
		t.Fatal("not idle after the reply")
	}
}

// The frames are those that bare-rpc writes for the same messages.
func TestWireFormat(t *testing.T) {
	t.Run("outgoing", func(t *testing.T) {
		r, p := withPeer(t, nil)

		replies := make(chan error, 1)

		go func() {
			data, err := r.Request(context.Background(), 7, []byte("ping"))
			if err == nil && string(data) != "pong" {
				err = errors.New("unexpected reply " + string(data))
			}

			replies <- err
		}()

		p.expect("09000000010107000470696e67")
		p.send("090000000201000004706f6e67")

		if err := <-replies; err != nil {
			t.Fatal(err)
		}

		go func() {
			_, err := r.Request(context.Background(), 7, []byte("ping"))

			replies <- err
		}()

		p.expect("09000000010207000470696e67")
		p.send("0f00000002020100046e6f7065044e4f504503")

		if e := asError(t, <-replies); e.Message != "nope" || e.Code != "NOPE" || e.Errno != -2 {
			t.Fatalf("got %+v", e)
		}

		ctx, cancel := context.WithCancel(context.Background())

		go func() {
			_, err := r.Request(ctx, 7, []byte("ping"))

			replies <- err
		}()

		p.expect("09000000010307000470696e67")

		cancel()

		p.expect("2300000004031552657175657374207761732063616e63656c6c65640943414e43454c4c454400")

		<-replies

		if err := r.Event(3, []byte("x")); err != nil {
			t.Fatal(err)
		}

		p.expect("06000000010003000178")
	})

	t.Run("incoming", func(t *testing.T) {
		router := echo()

		router.OnRequest(8, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
			return nil, &barerpc.Error{Message: "nope", Code: "NOPE", Errno: -2}
		})

		_, p := withPeer(t, &barerpc.Options{Handler: router})

		p.send("09000000010107000470696e67")
		p.expect("0e00000002010000096563686f3a70696e67")

		p.send("050000000102080000")
		p.expect("0f00000002020100046e6f7065044e4f504503")
	})
}

func TestUnknownCommand(t *testing.T) {
	client, _ := pair(t, nil)

	_, err := client.Request(context.Background(), 1, nil)

	if e := asError(t, err); e.Code != barerpc.CodeUnknownCommand {
		t.Fatalf("got %+v", e)
	}
}

func TestHandlerErrors(t *testing.T) {
	var router barerpc.Router

	router.OnRequest(1, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
		return nil, &barerpc.Error{Message: "nope", Code: "NOPE", Errno: 3}
	})

	router.OnRequest(2, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
		return nil, errors.New("boom")
	})

	router.OnRequest(3, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
		if err := req.Reply([]byte("ok")); err != nil {
			return nil, err
		}

		if err := req.Reply([]byte("again")); err == nil || asError(t, err).Code != barerpc.CodeAlreadySent {
			t.Errorf("second reply = %v, want ALREADY_SENT", err)
		}

		return nil, errors.New("too late")
	})

	client, _ := pair(t, &router)

	_, err := client.Request(context.Background(), 1, nil)

	if e := asError(t, err); e.Message != "nope" || e.Code != "NOPE" || e.Errno != 3 {
		t.Fatalf("got %+v", e)
	}

	_, err = client.Request(context.Background(), 2, nil)

	if e := asError(t, err); e.Message != "boom" || e.Code != "" {
		t.Fatalf("got %+v", e)
	}

	got, err := client.Request(context.Background(), 3, nil)
	if err != nil || string(got) != "ok" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestEvents(t *testing.T) {
	var router barerpc.Router

	events := make(chan string, 1)

	router.OnEvent(1, func(ctx context.Context, event *barerpc.Event) error {
		events <- string(event.Data)

		return nil
	})

	router.OnEvent(2, func(ctx context.Context, event *barerpc.Event) error {
		return errors.New("bad event")
	})

	client, server := pair(t, &router)

	if err := client.Event(1, []byte("hello")); err != nil {
		t.Fatal(err)
	}

	if got := <-events; got != "hello" {
		t.Fatalf("got %q", got)
	}

	if err := client.Event(2, nil); err != nil {
		t.Fatal(err)
	}

	if err := server.Wait(); err == nil || err.Error() != "bad event" {
		t.Fatalf("server ended with %v, want bad event", err)
	}

	if err := client.Wait(); err != nil {
		t.Fatalf("client ended with %v, want a clean close", err)
	}

	if _, err := client.Request(context.Background(), 1, nil); !errors.Is(err, barerpc.ErrChannelClosed) {
		t.Fatalf("request after the end = %v, want ErrChannelClosed", err)
	}
}

func TestRequestStream(t *testing.T) {
	var router barerpc.Router

	router.OnRequest(1, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
		s, err := req.CreateRequestStream()
		if err != nil {
			return nil, err
		}

		data, err := io.ReadAll(s)

		return bytes.ToUpper(data), err
	})

	client, server := pair(t, &router)

	req := client.NewRequest(context.Background(), 1)

	s, err := req.CreateRequestStream()
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		s.Write([]byte("hello"))
		s.Write([]byte(" world"))
		s.Close()
	}()

	got, err := req.Reply()
	if err != nil || string(got) != "HELLO WORLD" {
		t.Fatalf("got %q, %v", got, err)
	}

	waitIdle(t, client, server)
}

func TestResponseStream(t *testing.T) {
	var router barerpc.Router

	router.OnRequest(1, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
		s, err := req.CreateResponseStream()
		if err != nil {
			return nil, err
		}

		for _, chunk := range []string{"one", "two two", "3"} {
			if _, err := s.Write([]byte(chunk)); err != nil {
				return nil, err
			}
		}

		return nil, s.Close()
	})

	router.OnRequest(2, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
		return []byte("plain"), nil
	})

	client, server := pair(t, &router)

	if got := readChunks(t, client, 1); got != "one|two two|3" {
		t.Fatalf("got %q", got)
	}

	// A plain reply arrives as a single chunk.
	if got := readChunks(t, client, 2); got != "plain" {
		t.Fatalf("got %q", got)
	}

	waitIdle(t, client, server)
}

func readChunks(t *testing.T, client *barerpc.RPC, command uint) string {
	t.Helper()

	req := client.NewRequest(context.Background(), command)

	s, err := req.CreateResponseStream()
	if err != nil {
		t.Fatal(err)
	}

	if err := req.Send(nil); err != nil {
		t.Fatal(err)
	}

	var chunks []string

	for {
		chunk, err := s.ReadChunk()
		if err == io.EOF {
			return strings.Join(chunks, "|")
		}

		if err != nil {
			t.Fatal(err)
		}

		chunks = append(chunks, string(chunk))
	}
}

func TestBidirectionalStream(t *testing.T) {
	var router barerpc.Router

	router.OnRequest(1, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
		in, err := req.CreateRequestStream()
		if err != nil {
			return nil, err
		}

		out, err := req.CreateResponseStream()
		if err != nil {
			return nil, err
		}

		if _, err := io.Copy(out, in); err != nil {
			return nil, err
		}

		return nil, out.Close()
	})

	client, server := pair(t, &router)

	req := client.NewRequest(context.Background(), 1)

	out, _ := req.CreateResponseStream()
	in, _ := req.CreateRequestStream()

	go func() {
		in.Write([]byte("abc"))
		in.Write([]byte("de"))
		in.Close()
	}()

	got, err := io.ReadAll(out)
	if err != nil || string(got) != "abcde" {
		t.Fatalf("got %q, %v", got, err)
	}

	waitIdle(t, client, server)
}

// Writes far more than a stream buffers to a slow reader, which only
// completes if the writer pauses and resumes.
func TestBackpressure(t *testing.T) {
	const chunks = 200

	chunk := bytes.Repeat([]byte("x"), 1024)

	var router barerpc.Router

	router.OnRequest(1, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
		s, err := req.CreateResponseStream()
		if err != nil {
			return nil, err
		}

		for range chunks {
			if _, err := s.Write(chunk); err != nil {
				return nil, err
			}
		}

		return nil, s.Close()
	})

	a, b := net.Pipe()

	server := barerpc.New(b, &barerpc.Options{Handler: &router, MaxBufferedBytes: 64 * 1024})
	client := barerpc.New(a, &barerpc.Options{MaxBufferedBytes: 64 * 1024})

	t.Cleanup(func() {
		client.Close()
		server.Close()
	})

	req := client.NewRequest(context.Background(), 1)

	s, _ := req.CreateResponseStream()
	req.Send(nil)

	total := 0
	buf := make([]byte, 4096)

	for {
		n, err := s.Read(buf)

		total += n

		if err == io.EOF {
			break
		}

		if err != nil {
			t.Fatal(err)
		}

		time.Sleep(100 * time.Microsecond)
	}

	if total != chunks*len(chunk) {
		t.Fatalf("read %d bytes, want %d", total, chunks*len(chunk))
	}
}

func TestStreamOverflow(t *testing.T) {
	var router barerpc.Router

	router.OnRequest(7, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
		if _, err := req.CreateRequestStream(); err != nil {
			return nil, err
		}

		<-ctx.Done()

		return nil, nil
	})

	r, p := withPeer(t, &barerpc.Options{Handler: &router, MaxBufferedBytes: 4})

	p.send("0400000001010701")
	p.expect("050000000301fd0101")

	// More data than the stream may buffer.
	p.send("090000000301fd100103616263")
	p.send("090000000301fd100103616263")

	p.drain()

	if e := asError(t, r.Wait()); e.Code != barerpc.CodeStreamOverflow {
		t.Fatalf("got %+v", e)
	}
}

func TestCancel(t *testing.T) {
	var router barerpc.Router

	started := make(chan struct{})
	causes := make(chan error, 1)

	router.OnRequest(1, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
		close(started)

		<-ctx.Done()

		causes <- context.Cause(ctx)

		return nil, nil
	})

	client, server := pair(t, &router)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		<-started
		cancel()
	}()

	_, err := client.Request(ctx, 1, nil)

	if e := asError(t, err); e.Code != barerpc.CodeCancelled || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %+v", e)
	}

	if e := asError(t, <-causes); e.Code != barerpc.CodeCancelled {
		t.Fatalf("handler cancelled with %+v", e)
	}

	waitIdle(t, client, server)
}

func TestTimeout(t *testing.T) {
	var router barerpc.Router

	causes := make(chan error, 1)

	router.OnRequest(1, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
		<-ctx.Done()

		causes <- context.Cause(ctx)

		return nil, nil
	})

	client, _ := pair(t, &router)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := client.Request(ctx, 1, nil)

	if e := asError(t, err); e.Code != barerpc.CodeTimeout || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %+v", e)
	}

	if e := asError(t, <-causes); e.Code != barerpc.CodeTimeout {
		t.Fatalf("handler cancelled with %+v", e)
	}
}

// A response stream closed before the answer has arrived cancels the request.
func TestClosingResponseStreamCancels(t *testing.T) {
	var router barerpc.Router

	causes := make(chan error, 1)

	router.OnRequest(1, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
		s, err := req.CreateResponseStream()
		if err != nil {
			return nil, err
		}

		for {
			if _, err := s.Write([]byte("more")); err != nil {
				break
			}
		}

		// The remote end destroys the stream before it cancels the request.
		<-ctx.Done()

		causes <- context.Cause(ctx)

		return nil, nil
	})

	client, _ := pair(t, &router)

	req := client.NewRequest(context.Background(), 1)

	s, _ := req.CreateResponseStream()
	req.Send(nil)

	if _, err := s.ReadChunk(); err != nil {
		t.Fatal(err)
	}

	s.Close()

	if e := asError(t, <-causes); e.Code != barerpc.CodeCancelled {
		t.Fatalf("handler cancelled with %+v", e)
	}
}

func TestUnexpectedStream(t *testing.T) {
	var router barerpc.Router

	router.OnRequest(1, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
		s, err := req.CreateResponseStream()
		if err != nil {
			return nil, err
		}

		s.Write([]byte("unread"))

		return nil, nil
	})

	router.OnRequest(2, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
		return []byte("ok"), nil
	})

	client, _ := pair(t, &router)

	_, err := client.Request(context.Background(), 1, nil)

	if e := asError(t, err); e.Code != barerpc.CodeUnexpectedStream {
		t.Fatalf("got %+v", e)
	}

	if got, err := client.Request(context.Background(), 2, nil); err != nil || string(got) != "ok" {
		t.Fatalf("got %q, %v after an unexpected stream", got, err)
	}
}

func TestClose(t *testing.T) {
	var router barerpc.Router

	started := make(chan struct{})
	causes := make(chan error, 1)

	router.OnRequest(1, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
		close(started)

		<-ctx.Done()

		causes <- context.Cause(ctx)

		return nil, nil
	})

	client, server := pair(t, &router)

	replies := make(chan error, 1)

	go func() {
		_, err := client.Request(context.Background(), 1, nil)

		replies <- err
	}()

	<-started

	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	if err := <-replies; !errors.Is(err, barerpc.ErrChannelClosed) {
		t.Fatalf("request in flight = %v, want ErrChannelClosed", err)
	}

	if err := <-causes; !errors.Is(err, barerpc.ErrChannelClosed) {
		t.Fatalf("handler cancelled with %v, want ErrChannelClosed", err)
	}

	if _, err := client.Request(context.Background(), 1, nil); !errors.Is(err, barerpc.ErrChannelClosed) {
		t.Fatalf("request after Close = %v, want ErrChannelClosed", err)
	}

	if err := server.Wait(); err != nil {
		t.Fatalf("server ended with %v, want a clean close", err)
	}
}

func TestProtocolErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame string
		code  string
	}{
		{"unknown message", "020000000501", barerpc.CodeUnknownMessage},
		{"response without an id", "050000000200000000", barerpc.CodeInvalidMessage},
		{"malformed message", "03000000010107", barerpc.CodeInvalidMessage},
		{"frame too large", "ff000000", barerpc.CodeFrameTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, p := withPeer(t, &barerpc.Options{MaxFrameSize: 64})

			p.drain()
			p.send(tc.frame)

			if e := asError(t, r.Wait()); e.Code != tc.code {
				t.Fatalf("got %+v", e)
			}
		})
	}
}

// Cleanup that follows a stream can trail the read that ends it.
func waitIdle(t *testing.T, rpcs ...*barerpc.RPC) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)

	for _, r := range rpcs {
		for !r.Idle() {
			if time.Now().After(deadline) {
				t.Fatal("not idle")
			}

			time.Sleep(time.Millisecond)
		}
	}
}
