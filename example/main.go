package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"holepunch.to/bare_rpc"
	c "holepunch.to/compactencoding"
)

type Item struct {
	title string
	desc  string
}

type ItemEncoding struct{}

func (m *ItemEncoding) Preencode(state *c.State, msg *Item) {
	c.NewString().Preencode(state, msg.title)
	c.NewString().Preencode(state, msg.desc)
}

func (m *ItemEncoding) Encode(state *c.State, msg *Item) error {
	if err := c.NewString().Encode(state, msg.title); err != nil {
		return err
	}
	if err := c.NewString().Encode(state, msg.desc); err != nil {
		return err
	}

	return nil
}

func (m *ItemEncoding) Decode(state *c.State) (*Item, error) {
	var err error
	msg := &Item{}

	if msg.title, err = c.NewString().Decode(state); err != nil {
		return nil, err
	}
	if msg.desc, err = c.NewString().Decode(state); err != nil {
		return nil, err
	}

	return msg, nil
}

type BareRPCSock struct {
	*bare_rpc.RPC
	path string
	conn net.Conn
}

func NewBareRPCSock() *BareRPCSock {
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

var docStyle = lipgloss.NewStyle().Margin(1, 2)

func (i Item) Title() string       { return i.title }
func (i Item) Description() string { return i.desc }
func (i Item) FilterValue() string { return i.title }

type model struct {
	list list.Model
	rpc  *BareRPCSock
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		h, v := docStyle.GetFrameSize()
		m.list.SetSize(msg.Width-h, msg.Height-v)
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m model) View() string {
	return docStyle.Render(m.list.View())
}

func main() {
	rpc := NewBareRPCSock()
	defer rpc.Stop()

	go func() {
		rpc.Listen(func(msg *bare_rpc.Request) {
			log.Printf("Go: got request: %v\n", msg)
		})
	}()

	buf, err := rpc.Request(0, []byte{})
	if err != nil {
		log.Println("Request failed", err)
	}
	res, err := c.Decode(c.NewArray(&ItemEncoding{}), buf)
	if err != nil {
		log.Println("Request failed decode", err)
	}

	items := make([]list.Item, 0, len(res))
	for _, item := range res {
		items = append(items, item)
	}

	m := model{list: list.New(items, list.NewDefaultDelegate(), 0, 0)}
	m.list.Title = "My Fave Things"

	p := tea.NewProgram(m, tea.WithAltScreen())

	if _, err := p.Run(); err != nil {
		fmt.Println("Error running program:", err)
		os.Exit(1)
	}
}
