//go:build interop

// Cross-runtime tests against the JavaScript bare-rpc reference. They spawn
// `bare example/stream-server.js` and talk to it over a unix socket, proving
// wire compatibility of the streaming protocol.
//
// Run with: go test -tags interop -run TestInterop -v ./...
// Requires `bare` on PATH and `npm i` already run in ./example.

package bare_rpc

import (
	"bytes"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func startJSPeer(t *testing.T) *RPC {
	t.Helper()

	socketPath := filepath.Join(t.TempDir(), "interop.sock")
	os.Remove(socketPath)

	cmd := exec.Command("bare", "stream-server.js", socketPath)
	cmd.Dir = "example"
	// Capture (don't inherit) the child's stderr so killing it doesn't leave the
	// test harness waiting on a shared fd; surface it only if the test fails.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		t.Skipf("bare not runnable: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() && stderr.Len() > 0 {
			t.Logf("JS peer stderr:\n%s", stderr.String())
		}
	})

	// Wait for the JS server to bind the socket.
	var conn net.Conn
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("unix", socketPath)
		if err == nil {
			conn = c
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out connecting to JS peer: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Cleanup(func() { conn.Close() })

	rpc := NewRPC(conn)
	go func() { _ = rpc.Listen(nil) }()
	return rpc
}

func TestInteropResponseStream(t *testing.T) {
	rpc := startJSPeer(t)

	req := rpc.NewRequest(1)
	if err := req.Send(nil); err != nil {
		t.Fatal(err)
	}
	rs := req.CreateResponseStream()

	got, err := io.ReadAll(rs)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "abc" {
		t.Fatalf("got %q, want %q", got, "abc")
	}
}

func TestInteropRequestStream(t *testing.T) {
	rpc := startJSPeer(t)

	req := rpc.NewRequest(2)
	ws := req.CreateRequestStream()

	go func() {
		ws.Write([]byte("hello"))
		ws.Write([]byte(" world"))
		ws.Close()
	}()

	got, err := req.Reply()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "HELLO WORLD" {
		t.Fatalf("got %q, want %q", got, "HELLO WORLD")
	}
}

func TestInteropBidirectional(t *testing.T) {
	rpc := startJSPeer(t)

	req := rpc.NewRequest(3)
	out := req.CreateResponseStream()
	in := req.CreateRequestStream()

	go func() {
		in.Write([]byte("abc"))
		in.Write([]byte("de"))
		in.Close()
	}()

	got, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "cbaed" {
		t.Fatalf("got %q, want %q", got, "cbaed")
	}
}
