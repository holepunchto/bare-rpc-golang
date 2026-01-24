package main

import (
	"log"
	"net"
	"os"
	"os/exec"
	"time"

	"holepunch.to/bare_rpc"
	c "holepunch.to/compactencoding"
)

type BareMessage struct {
	Type  int
	Value string
}

type BareMessageEncoding struct{}

func (m *BareMessageEncoding) Preencode(state *c.State, msg *BareMessage) {
	c.NewInt().Preencode(state, msg.Type)
	c.NewString().Preencode(state, msg.Value)
}

func (m *BareMessageEncoding) Encode(state *c.State, msg *BareMessage) error {
	if err := c.NewInt().Encode(state, msg.Type); err != nil {
		return err
	}
	if err := c.NewString().Encode(state, msg.Value); err != nil {
		return err
	}

	return nil
}

func (m *BareMessageEncoding) Decode(state *c.State) (*BareMessage, error) {
	var err error
	msg := &BareMessage{}

	if msg.Type, err = c.NewInt().Decode(state); err != nil {
		return nil, err
	}
	if msg.Value, err = c.NewString().Decode(state); err != nil {
		return nil, err
	}

	return msg, nil
}

func main() {
	socketPath := "/tmp/bare-rpc.sock"

	// Remove old socket if exists
	os.Remove(socketPath)
	defer func() {
		os.Remove(socketPath)
	}()

	// Start Bare server
	cmd := exec.Command("bare", "server.js", socketPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		log.Fatalf("failed to start bare: %v", err)
	}

	// Wait for server to start
	time.Sleep(500 * time.Millisecond)

	// Connect to Unix socket
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		log.Fatalf("dial error: %v", err)
	}
	defer conn.Close()

	rpc := bare_rpc.NewRPC(conn)

	go func() {
		time.Sleep(50 * time.Millisecond)

		response, err := rpc.Request(42, []byte("hello from Go"))
		if err != nil {
			log.Fatalf("Go: request error: %v", err)
		}

		log.Printf("Go: got response: %s\n", string(response))
	}()

	err = rpc.Listen(func(req *bare_rpc.Request) {
		msg, err := c.Decode(&BareMessageEncoding{}, req.Data)
		log.Println(msg.Value, err)
	})
	if err != nil {
		log.Printf("Go: HandleMessages error: %v", err)
	}
}
