// Go client for the JavaScript bare-rpc server in server.js.
//
// It spawns `bare server.js <socket>`, connects over a unix socket and makes
// two plain requests: command 0 replies with a compact-encoded array of items,
// command 42 replies with a string.
package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"time"

	bare_rpc "github.com/holepunchto/bare-rpc-golang"
	c "github.com/holepunchto/compact-encoding-golang"
)

type Item struct {
	Title string
	Desc  string
}

func main() {
	socketPath := "/tmp/bare-rpc.sock"
	os.Remove(socketPath)

	cmd := exec.Command("bare", "server.js", socketPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Fatalf("failed to start bare: %v", err)
	}
	defer cmd.Process.Kill()

	conn := dial(socketPath)
	defer conn.Close()

	rpc := bare_rpc.NewRPC(conn)
	go rpc.Listen(nil)

	buf, err := rpc.Request(0, nil)
	if err != nil {
		log.Fatalf("request 0: %v", err)
	}
	var items []Item
	if err := c.Unmarshal(buf, &items); err != nil {
		log.Fatalf("decode items: %v", err)
	}
	for _, item := range items {
		fmt.Printf("%-24s %s\n", item.Title, item.Desc)
	}

	buf, err = rpc.Request(42, nil)
	if err != nil {
		log.Fatalf("request 42: %v", err)
	}
	fmt.Printf("\ncommand 42 replied: %s\n", buf)
}

// dial retries until the JavaScript side has bound the socket.
func dial(socketPath string) net.Conn {
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			log.Fatalf("dial error: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
