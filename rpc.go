package bare_rpc

import (
	"io"
	"sync"

	c "github.com/holepunchto/compact-encoding-golang"
)

type RPC struct {
	stream io.ReadWriter

	// mu guards request ids and the stream/pending maps. It is held only
	// briefly and never across a blocking write.
	mu sync.Mutex
	// writeMu serializes frame writes so encoded frames don't interleave on
	// the wire. It is separate from mu so the read loop can keep dispatching
	// (which needs mu) while another goroutine is blocked writing to a slow or
	// synchronous transport.
	writeMu sync.Mutex

	id           uint
	messageCodec *MessageCodec

	// pending holds channels awaiting a unary response, keyed by request id.
	pending map[uint]chan *Message

	// Stream registries. The direction of a stream is the request id plus a
	// direction mask (StreamRequest / StreamResponse); the operation flag tells
	// whether the local end is the writer (outgoing*) or reader (incoming*).
	outgoingRequests  map[uint]*OutgoingStream // request-stream writers (we initiated)
	outgoingResponses map[uint]*OutgoingStream // response-stream writers (we handle)
	incomingRequests  map[uint]*IncomingStream // request-stream readers (we handle)
	incomingResponses map[uint]*IncomingStream // response-stream readers (we initiated)

	// pendingOpen* remember an OPEN ack that arrived before the local writer
	// was registered, so the handshake still completes (the writer checks
	// these on creation). Mirrors bare-rpc's _pendingRequests/_pendingResponses.
	pendingOpenRequests  map[uint]bool
	pendingOpenResponses map[uint]bool
}

func NewRPC(stream io.ReadWriter) *RPC {
	return &RPC{
		stream:            stream,
		id:                0,
		messageCodec:      NewMessageCodec(),
		pending:           make(map[uint]chan *Message),
		outgoingRequests:  make(map[uint]*OutgoingStream),
		outgoingResponses: make(map[uint]*OutgoingStream),
		incomingRequests:  make(map[uint]*IncomingStream),
		incomingResponses: make(map[uint]*IncomingStream),

		pendingOpenRequests:  make(map[uint]bool),
		pendingOpenResponses: make(map[uint]bool),
	}
}

// Send encodes and writes a message to the stream. Encoding happens without any
// lock; only the write itself is serialized (via writeMu), so the read loop can
// keep dispatching while a write blocks on a slow/synchronous transport.
func (r *RPC) Send(m *Message) error {
	buf, err := c.Encode(&MessageCodec{}, m)
	if err != nil {
		return err
	}

	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	_, err = r.stream.Write(buf)
	return err
}

// sendStream is a helper for stream control/data frames.
func (r *RPC) sendStream(id, flags uint, data []byte) error {
	return r.Send(&Message{Type: TypeStream, ID: id, Stream: flags, Data: data})
}

// sendStreamError sends a stream frame carrying an error (CLOSE|ERROR etc).
func (r *RPC) sendStreamError(id, flags uint, cause error) error {
	rpcErr := &RPCError{Message: cause.Error()}
	if e, ok := cause.(*RPCError); ok {
		rpcErr = e
	}
	return r.Send(&Message{Type: TypeStream, ID: id, Stream: flags | StreamError, Error: rpcErr})
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

// Request sends a request and returns the response (unary).
func (r *RPC) Request(command uint, data []byte) ([]byte, error) {
	req := r.NewRequest(command)
	if err := req.Send(data); err != nil {
		r.mu.Lock()
		delete(r.pending, req.id)
		r.mu.Unlock()
		return nil, err
	}
	return req.Reply()
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

func (r *Request) ReplyError(err error) error {
	return r.rpc.ReplyError(r.ID, err)
}

// CreateRequestStream opens the readable request stream (initiator -> handler).
func (r *Request) CreateRequestStream() *IncomingStream {
	s := newIncomingStream(r.rpc, r.ID, StreamRequest)

	r.rpc.mu.Lock()
	r.rpc.incomingRequests[r.ID] = s
	r.rpc.mu.Unlock()

	s.open()
	return s
}

// CreateResponseStream opens the writable response stream (handler -> initiator).
func (r *Request) CreateResponseStream() *OutgoingStream {
	s := newOutgoingStream(r.rpc, r.ID, r.Command, TypeResponse, StreamResponse)

	acked := r.rpc.registerOutgoing(r.ID, StreamResponse, s)
	s.open()
	if acked {
		s.continueOpen()
	}
	return s
}

// Listen reads messages and dispatches them. onRequest is called for incoming
// requests (including stream-opening requests, which carry no data).
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
			r.onResponse(msg)

		case TypeStream:
			r.onStream(msg)
		}
	}
}

func (r *RPC) onResponse(msg *Message) {
	if msg.ID == 0 {
		return
	}

	r.mu.Lock()
	ch, ok := r.pending[msg.ID]
	r.mu.Unlock()
	if !ok {
		return
	}

	// A response with a stream flag set is just the response-stream OPEN
	// signal; the unary waiter only resolves on an error or stream==0 reply.
	if msg.Error != nil || msg.Stream == 0 {
		ch <- msg
	}
}

func (r *RPC) onStream(msg *Message) {
	if msg.ID == 0 {
		return
	}

	switch {
	case msg.Stream&StreamOpen != 0:
		r.onStreamOpen(msg)
	case msg.Stream&StreamClose != 0:
		r.onStreamClose(msg)
	case msg.Stream&StreamPause != 0:
		r.onStreamPause(msg)
	case msg.Stream&StreamResume != 0:
		r.onStreamResume(msg)
	case msg.Stream&StreamData != 0:
		r.onStreamData(msg)
	case msg.Stream&StreamEnd != 0:
		r.onStreamEnd(msg)
	case msg.Stream&StreamDestroy != 0:
		r.onStreamDestroy(msg)
	}
}

func (r *RPC) onStreamOpen(msg *Message) {
	r.mu.Lock()
	var s *OutgoingStream
	if msg.Stream&StreamRequest != 0 {
		if s = r.outgoingRequests[msg.ID]; s == nil {
			r.pendingOpenRequests[msg.ID] = true
		}
	} else if msg.Stream&StreamResponse != 0 {
		if s = r.outgoingResponses[msg.ID]; s == nil {
			r.pendingOpenResponses[msg.ID] = true
		}
	}
	r.mu.Unlock()

	if s != nil {
		s.continueOpen()
	}
}

// registerOutgoing records a writer and reports whether an OPEN ack already
// arrived for it (in which case the caller should open the stream immediately).
func (r *RPC) registerOutgoing(id, mask uint, s *OutgoingStream) (acked bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if mask&StreamRequest != 0 {
		r.outgoingRequests[id] = s
		acked = r.pendingOpenRequests[id]
		delete(r.pendingOpenRequests, id)
	} else {
		r.outgoingResponses[id] = s
		acked = r.pendingOpenResponses[id]
		delete(r.pendingOpenResponses, id)
	}
	return acked
}

func (r *RPC) onStreamClose(msg *Message) {
	s := r.incomingStream(msg.ID, msg.Stream)
	if s == nil {
		return
	}
	if msg.Error != nil {
		s.destroyFromRemote(msg.Error)
	} else {
		s.pushEOF()
	}
}

func (r *RPC) onStreamPause(msg *Message) {
	if s := r.outgoingStream(msg.ID, msg.Stream); s != nil {
		s.cork()
	}
}

func (r *RPC) onStreamResume(msg *Message) {
	if s := r.outgoingStream(msg.ID, msg.Stream); s != nil {
		s.uncork()
	}
}

func (r *RPC) onStreamData(msg *Message) {
	if s := r.incomingStream(msg.ID, msg.Stream); s != nil {
		s.push(msg.Data)
	}
}

func (r *RPC) onStreamEnd(msg *Message) {
	if s := r.incomingStream(msg.ID, msg.Stream); s != nil {
		s.pushEOF()
	}
}

func (r *RPC) onStreamDestroy(msg *Message) {
	s := r.outgoingStream(msg.ID, msg.Stream)
	if s == nil {
		return
	}
	var err error
	if msg.Error != nil {
		err = msg.Error
	}
	s.destroyFromRemote(err)
}

// outgoingStream returns the writer registered for (id, direction), if any.
func (r *RPC) outgoingStream(id, flags uint) *OutgoingStream {
	r.mu.Lock()
	defer r.mu.Unlock()
	if flags&StreamRequest != 0 {
		return r.outgoingRequests[id]
	}
	if flags&StreamResponse != 0 {
		return r.outgoingResponses[id]
	}
	return nil
}

// incomingStream returns the reader registered for (id, direction), if any.
func (r *RPC) incomingStream(id, flags uint) *IncomingStream {
	r.mu.Lock()
	defer r.mu.Unlock()
	if flags&StreamRequest != 0 {
		return r.incomingRequests[id]
	}
	if flags&StreamResponse != 0 {
		return r.incomingResponses[id]
	}
	return nil
}

func (r *RPC) removeOutgoing(id, mask uint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if mask&StreamRequest != 0 {
		delete(r.outgoingRequests, id)
	} else {
		delete(r.outgoingResponses, id)
	}
}

func (r *RPC) removeIncoming(id, mask uint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if mask&StreamRequest != 0 {
		delete(r.incomingRequests, id)
	} else {
		delete(r.incomingResponses, id)
	}
}
