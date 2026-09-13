package ipc

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	bare_rpc "github.com/holepunchto/bare-rpc-golang"
)

func TestSocketpairCarriesRPC(t *testing.T) {
	parent, child, err := Socketpair()
	if err != nil {
		t.Fatal(err)
	}

	// The child side does what a spawned process does with its inherited fd.
	childConn, err := Inherited(int(child.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	child.Close()
	parentConn, err := net.FileConn(parent)
	if err != nil {
		t.Fatal(err)
	}
	parent.Close()
	t.Cleanup(func() { childConn.Close(); parentConn.Close() })

	server := bare_rpc.NewRPC(childConn)
	go server.Listen(func(req *bare_rpc.Request) error {
		return req.Reply(append([]byte("pong: "), req.Data...))
	})
	client := bare_rpc.NewRPC(parentConn)
	go client.Listen(nil)

	got, err := client.Request(1, []byte("ping"))
	if err != nil || string(got) != "pong: ping" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestInheritedRejectsClosedFD(t *testing.T) {
	parent, child, err := Socketpair()
	if err != nil {
		t.Fatal(err)
	}
	parent.Close()
	fd := int(child.Fd())
	child.Close()
	if _, err := Inherited(fd); err == nil {
		t.Fatal("expected an error for a closed fd")
	}
}

func TestDialWaitsForListener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "late.sock")
	go func() {
		time.Sleep(100 * time.Millisecond)
		l, err := net.Listen("unix", path)
		if err != nil {
			t.Error(err)
			return
		}
		defer l.Close()
		if c, err := l.Accept(); err == nil {
			c.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := Dial(ctx, "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

func TestDialGivesUpWithContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := Dial(ctx, "unix", filepath.Join(t.TempDir(), "never.sock"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}
