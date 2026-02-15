package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"time"

	bare_rpc "github.com/holepunchto/bare-rpc-golang"
	"github.com/holepunchto/bare-rpc-golang/example/schema"
	c "github.com/holepunchto/compact-encoding-golang"
)

type BareRPCSock struct {
	*bare_rpc.RPC
	path string
	conn net.Conn
}

func NewBareRPCSock() *BareRPCSock {
	socketPath := "/tmp/bare-rpc.sock"

	// Remove old socket if exists
	os.Remove(socketPath)

	// Start Bare server
	cmd := exec.Command("bare", "hrpc-server.js", socketPath)
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

	rpc := bare_rpc.NewRPC(conn)

	return &BareRPCSock{
		rpc,
		socketPath,
		conn,
	}
}

func (b *BareRPCSock) Stop() {
	b.conn.Close()
	os.Remove(b.path)
}

// var docStyle = lipgloss.NewStyle().Margin(1, 2)

// func (i Item) Title() string       { return i.title }
// func (i Item) Description() string { return i.desc }
// func (i Item) FilterValue() string { return i.title }

// type model struct {
// 	list list.Model
// 	rpc  *BareRPCSock
// }

// func (m model) Init() tea.Cmd {
// 	return nil
// }

// func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
// 	switch msg := msg.(type) {
// 	case tea.KeyMsg:
// 		if msg.String() == "ctrl+c" {
// 			return m, tea.Quit
// 		}
// 	case tea.WindowSizeMsg:
// 		h, v := docStyle.GetFrameSize()
// 		m.list.SetSize(msg.Width-h, msg.Height-v)
// 	}

// 	var cmd tea.Cmd
// 	m.list, cmd = m.list.Update(msg)
// 	return m, cmd
// }

// func (m model) View() string {
// 	return docStyle.Render(m.list.View())
// }

type HRPCHandler[T any] struct {
	onRequest func(data any) (any, error)
	encode    func(data any) ([]byte, error)
	decode    func(data []byte) (any, error)
}

type HRPC struct {
	handlers map[uint]HRPCHandler[any]
}

func main() {
	rpc := NewBareRPCSock()
	defer rpc.Stop()

	hrpc := &HRPC{
		handlers: map[uint]HRPCHandler[any]{
			0: {
				onRequest: func(data any) (any, error) {
					log.Printf("Go: got request 0: %v\n", data.(*schema.ExampleHelloRequest))

					return &schema.ExampleHelloResponse{
						Reply: "hello world!",
					}, nil
				},
				encode: func(data any) ([]byte, error) {
					value := data.(*schema.ExampleHelloResponse)
					state := c.NewState()
					value.Preencode(state)
					state.Allocate()
					if err := value.Encode(state); err != nil {
						return nil, err
					}
					return state.Buffer, nil
				},
				decode: func(data []byte) (any, error) {
					var value schema.ExampleHelloRequest
					state := c.NewState()
					state.End = uint(len(data))
					state.Buffer = data
					if err := value.Decode(state); err != nil {
						return nil, err
					}
					return &value, nil
				},
			},
		},
	}

	rpc.Listen(func(msg *bare_rpc.Request) {
		log.Printf("Go: got request: %v\n", msg.Data)

		handler, ok := hrpc.handlers[msg.Command]
		if !ok {
			fmt.Printf("command not found: %v", msg.Command)
			return
		}

		fmt.Println("request ID", msg.ID)

		data, err := handler.decode(msg.Data)
		if err != nil {
			fmt.Printf("failed to parse request for: %v: %v", msg.Command, err)
		}

		res, err := handler.onRequest(data)
		if err != nil {
			fmt.Printf("failed to handle request for: %v: %v", msg.Command, err)
		}

		resEncoded, err := handler.encode(res)
		if err != nil {
			fmt.Printf("failed to encode response for: %v: %v", msg.Command, err)
		}

		fmt.Println("replying", resEncoded)
		err = msg.Reply(resEncoded)
		if err != nil {
			fmt.Printf("failed to send reply for: %v: %v", msg.Command, err)
		}
	})
}
