package bare_rpc

import (
	"io"
	"sync"

	c "github.com/holepunchto/compact-encoding-golang"
)

type RPC struct {
	stream io.ReadWriter
	mu     sync.Mutex

	id               uint
	outgoingRequests map[uint]chan *Message
	messageCodec     *MessageCodec
}

func NewRPC(stream io.ReadWriter) *RPC {
	rpc := &RPC{
		stream:           stream,
		id:               0,
		outgoingRequests: make(map[uint]chan *Message),
		messageCodec:     NewMessageCodec(),
	}
	return rpc
}

// Send encodes and writes a message to the stream
func (r *RPC) Send(m *Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	state := c.NewState()
	r.messageCodec.Preencode(state, m)

	state.Buffer = make([]byte, state.End)
	state.Start = 0

	if err := r.messageCodec.Encode(state, m); err != nil {
		return err
	}

	_, err := r.stream.Write(state.Buffer)
	return err
}

// Receive reads and decodes a message from the stream
func (r *RPC) Receive() (*Message, error) {
	// Read frame length (4 bytes LE uint32)
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(r.stream, lenBuf); err != nil {
		return nil, err
	}

	frameLen := uint(lenBuf[0]) |
		uint(lenBuf[1])<<8 |
		uint(lenBuf[2])<<16 |
		uint(lenBuf[3])<<24

	// Read the frame
	buf := make([]byte, 4+frameLen)
	copy(buf, lenBuf)
	_, err := io.ReadFull(r.stream, buf[4:])
	if err != nil {
		return nil, err
	}

	state := c.NewState()
	state.Buffer = buf
	state.Start = 0
	state.End = uint(len(buf))

	return r.messageCodec.Decode(state)
}

// Request sends a request and returns the response
func (r *RPC) Request(command uint, data []byte) ([]byte, error) {
	r.mu.Lock()
	r.id++
	id := r.id
	respChan := make(chan *Message, 1)
	r.outgoingRequests[id] = respChan
	r.mu.Unlock()

	msg := &Message{
		Type:    TypeRequest,
		ID:      id,
		Command: command,
		Stream:  0,
		Data:    data,
	}

	if err := r.Send(msg); err != nil {
		r.mu.Lock()
		delete(r.outgoingRequests, id)
		r.mu.Unlock()
		return nil, err
	}

	resp := <-respChan

	r.mu.Lock()
	delete(r.outgoingRequests, id)
	r.mu.Unlock()

	if resp.Error != nil {
		return nil, resp.Error
	}

	return resp.Data, nil
}

// Event sends a one-way event (request with ID 0)
func (r *RPC) Event(command uint, data []byte) error {
	msg := &Message{
		Type:    TypeRequest,
		ID:      0,
		Command: command,
		Stream:  0,
		Data:    data,
	}
	return r.Send(msg)
}

// Reply sends a response to a request
func (r *RPC) Reply(id uint, data []byte) error {
	msg := &Message{
		Type:   TypeResponse,
		ID:     id,
		Stream: 0,
		Data:   data,
	}
	return r.Send(msg)
}

// ReplyError sends an error response to a request
func (r *RPC) ReplyError(id uint, err error) error {
	rpcErr := &RPCError{
		Message: err.Error(),
		Code:    "",
		Errno:   0,
	}

	// If it's already an RPCError, use it directly
	if e, ok := err.(*RPCError); ok {
		rpcErr = e
	}

	msg := &Message{
		Type:   TypeResponse,
		ID:     id,
		Stream: 0,
		Error:  rpcErr,
	}
	return r.Send(msg)
}

type Request struct {
	*Message
	rpc *RPC
}

func (r *Request) Reply(data []byte) error {
	return r.rpc.Reply(r.ID, data)
}

// HandleMessages reads messages and dispatches them
// onRequest is called for incoming requests
func (r *RPC) Listen(onRequest func(msg *Request)) error {
	for {
		msg, err := r.Receive()
		if err != nil {
			return err
		}

		switch msg.Type {
		case TypeRequest:
			if onRequest != nil {
				go onRequest(&Request{msg, r})
			}

		case TypeResponse:
			r.mu.Lock()
			ch, ok := r.outgoingRequests[msg.ID]
			r.mu.Unlock()
			if ok {
				ch <- msg
			}

		case TypeStream:
			// TODO: handle streaming
		}
	}
}
