// Talks to a Bare child over an inherited file descriptor instead of a socket path.
//
// A Unix socketpair gives one duplex fd per side. The child's end is passed
// through ExtraFiles, which makes it fd 3 in the child, where pipe-server.js
// opens it with `new Pipe(3)`. The parent's end becomes a net.Conn.
package main

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	bare_rpc "github.com/holepunchto/bare-rpc-golang"
	"github.com/holepunchto/bare-rpc-golang/ipc"
)

func main() {
	parent, child, err := ipc.Socketpair()
	if err != nil {
		log.Fatal(err)
	}

	bare, err := bareBinary()
	if err != nil {
		log.Fatal(err)
	}
	cmd := exec.Command(bare, "pipe-server.js")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{child} // fd 3 in the child
	if err := cmd.Start(); err != nil {
		log.Fatalf("failed to start bare: %v", err)
	}
	child.Close() // the child holds its own copy now
	defer cmd.Process.Kill()

	conn, err := net.FileConn(parent)
	if err != nil {
		log.Fatal(err)
	}
	parent.Close() // FileConn dup'd it
	defer conn.Close()

	rpc := bare_rpc.NewRPC(conn)
	go rpc.Listen(nil)

	reply, err := rpc.Request(1, []byte("ping"))
	if err != nil {
		log.Fatalf("request: %v", err)
	}
	fmt.Printf("Go: bare replied %q over fd 3\n", reply)
}

// bareBinary finds the native Bare runtime. The `bare` npm package installs a Node shim
// that respawns the runtime with only stdio 0-2, which would drop fd 3, so the shim is
// asked where the real binary lives. Set BARE to point at a binary directly.
func bareBinary() (string, error) {
	if bare := os.Getenv("BARE"); bare != "" {
		return bare, nil
	}
	shim, err := exec.LookPath("bare")
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(shim)
	if err != nil {
		return "", err
	}
	head, err := os.ReadFile(real)
	if err != nil {
		return "", err
	}
	if !bytes.HasPrefix(head, []byte("#!")) {
		return real, nil
	}
	resolve := exec.Command("node", "-p", "require('bare-runtime')()")
	resolve.Dir = filepath.Dir(real)
	out, err := resolve.Output()
	if err != nil {
		return "", fmt.Errorf("resolving bare runtime behind %s: %w", shim, err)
	}
	return strings.TrimSpace(string(out)), nil
}
