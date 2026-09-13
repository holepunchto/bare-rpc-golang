package bare_rpc

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"

	c "github.com/holepunchto/compact-encoding-golang"
)

// ErrChannelClosed fails pending work when the transport closes cleanly (bare-rpc's CHANNEL_CLOSED).
var ErrChannelClosed = &RPCError{Message: "Channel closed", Code: "CHANNEL_CLOSED"}

// ErrEventReply is returned when replying to an event, which has no reply slot.
var ErrEventReply = errors.New("rpc: events cannot be replied to")

// ErrFrameTooLarge tears the channel down when a frame exceeds MaxFrameSize.
var ErrFrameTooLarge = errors.New("rpc: frame exceeds MaxFrameSize")

var errNoReply = errors.New("rpc: no unary reply: response was streamed")

// DefaultMaxFrameSize caps what an untrusted length prefix can make Receive allocate.
const DefaultMaxFrameSize = 16 << 20

// RPC multiplexes requests, events and streams over one io.ReadWriter.
type RPC struct {
	stream io.ReadWriter

	// MaxFrameSize is the largest frame body Receive accepts; set before Listen.
	MaxFrameSize uint

	mu      sync.Mutex // request ids, closed flag and the registries below
	writeMu sync.Mutex // serializes frame writes, separate so reads never wait on a slow write

	id           uint
	messageCodec *MessageCodec

	closed   bool
	closeErr error

	pending map[uint]chan *Message // unary replies; closed, never sent on, when no reply can come

	// Registries keyed by request id; the direction mask picks the map, the local role picks incoming or outgoing.
	outgoingRequests  map[uint]*OutgoingStream
	outgoingResponses map[uint]*OutgoingStream
	incomingRequests  map[uint]*IncomingStream
	incomingResponses map[uint]*IncomingStream

	// OPEN acks that arrived before the local writer registered (bare-rpc's _pendingRequests/_pendingResponses).
	pendingOpenRequests  map[uint]bool
	pendingOpenResponses map[uint]bool
}

// NewRPC wraps a transport. Run Listen to dispatch what arrives on it.
func NewRPC(stream io.ReadWriter) *RPC {
	return &RPC{
		stream:            stream,
		MaxFrameSize:      DefaultMaxFrameSize,
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

// Idle reports whether nothing is in flight: no pending reply and no open stream.
func (r *RPC) Idle() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending) == 0 &&
		len(r.outgoingRequests) == 0 &&
		len(r.outgoingResponses) == 0 &&
		len(r.incomingRequests) == 0 &&
		len(r.incomingResponses) == 0 &&
		len(r.pendingOpenRequests) == 0 &&
		len(r.pendingOpenResponses) == 0
}

// Send encodes and writes one frame. It fails fast once the channel is closed.
func (r *RPC) Send(m *Message) error {
	r.mu.Lock()
	closed, closeErr := r.closed, r.closeErr
	r.mu.Unlock()
	if closed {
		return closeErr
	}

	buf, err := c.Encode(r.messageCodec, m)
	if err != nil {
		return err
	}

	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	_, err = r.stream.Write(buf)
	return err
}

func (r *RPC) sendStream(id, flags uint, data []byte) error {
	return r.Send(&Message{Type: TypeStream, ID: id, Stream: flags, Data: data})
}

func (r *RPC) sendStreamError(id, flags uint, cause error) error {
	return r.Send(&Message{Type: TypeStream, ID: id, Stream: flags | StreamError, Error: toRPCError(cause)})
}

// toRPCError keeps a (wrapped) RPCError's code and errno; any other error crosses the wire as its message.
func toRPCError(err error) *RPCError {
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr
	}
	return &RPCError{Message: err.Error()}
}

// Receive reads and decodes one frame.
func (r *RPC) Receive() (*Message, error) {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(r.stream, lenBuf); err != nil {
		return nil, err
	}

	frameLen := uint(lenBuf[0]) |
		uint(lenBuf[1])<<8 |
		uint(lenBuf[2])<<16 |
		uint(lenBuf[3])<<24

	if frameLen > r.MaxFrameSize {
		return nil, ErrFrameTooLarge
	}

	buf := make([]byte, 4+frameLen)
	copy(buf, lenBuf)
	if _, err := io.ReadFull(r.stream, buf[4:]); err != nil {
		return nil, err
	}

	state := c.NewState()
	state.Buffer = buf
	state.End = uint(len(buf))

	return r.messageCodec.Decode(state)
}

// Request sends a unary request and waits for its reply.
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

// Event sends a one-way request (id 0) that gets no reply.
func (r *RPC) Event(command uint, data []byte) error {
	return r.Send(&Message{Type: TypeRequest, ID: 0, Command: command, Data: data})
}

// Reply sends a unary response.
func (r *RPC) Reply(id uint, data []byte) error {
	return r.Send(&Message{Type: TypeResponse, ID: id, Data: data})
}

// ReplyError sends an error response; see toRPCError for what crosses the wire.
func (r *RPC) ReplyError(id uint, err error) error {
	return r.Send(&Message{Type: TypeResponse, ID: id, Error: toRPCError(err)})
}

// Request is an incoming request or event as handed to the Listen handler.
type Request struct {
	*Message
	rpc  *RPC
	sent atomic.Bool
}

// IsEvent reports whether this is a one-way event (id 0).
func (r *Request) IsEvent() bool {
	return r.ID == 0
}

// Reply sends the unary response.
func (r *Request) Reply(data []byte) error {
	if r.IsEvent() {
		return ErrEventReply
	}
	r.sent.Store(true)
	return r.rpc.Reply(r.ID, data)
}

// ReplyError sends an error response.
func (r *Request) ReplyError(err error) error {
	if r.IsEvent() {
		return ErrEventReply
	}
	r.sent.Store(true)
	return r.rpc.ReplyError(r.ID, err)
}

// CreateRequestStream opens the readable request stream (initiator -> handler).
func (r *Request) CreateRequestStream() *IncomingStream {
	s := newIncomingStream(r.rpc, r.ID, StreamRequest)
	r.rpc.openIncoming(s)
	return s
}

// CreateResponseStream opens the writable response stream (handler -> initiator).
func (r *Request) CreateResponseStream() *OutgoingStream {
	r.sent.Store(true)
	s := newOutgoingStream(r.rpc, r.ID, r.Command, TypeResponse, StreamResponse)
	r.rpc.openOutgoing(s)
	return s
}

// Listen dispatches frames until the transport ends or fails, then fails everything still in flight.
//
// onRequest runs on its own goroutine per request or event. An error it returns becomes the
// error reply unless a reply was already started; for an event it fails the channel instead.
// A clean end reaches waiters as ErrChannelClosed while Listen itself returns io.EOF.
func (r *RPC) Listen(onRequest func(req *Request) error) error {
	for {
		msg, err := r.Receive()
		if err != nil {
			cause := err
			if err == io.EOF {
				cause = ErrChannelClosed
			}
			r.teardown(cause)
			return err
		}

		switch msg.Type {
		case TypeRequest:
			if onRequest != nil {
				go r.handle(&Request{Message: msg, rpc: r}, onRequest)
			}

		case TypeResponse:
			r.onResponse(msg)

		case TypeStream:
			r.onStream(msg)
		}
	}
}

func (r *RPC) handle(req *Request, onRequest func(*Request) error) {
	err := onRequest(req)
	if err == nil {
		return
	}
	if req.IsEvent() {
		r.fail(err)
		return
	}
	if !req.sent.Load() {
		req.ReplyError(err)
	}
}

// Close fails everything in flight with ErrChannelClosed and closes the transport when it is an io.Closer.
func (r *RPC) Close() error {
	r.teardown(ErrChannelClosed)
	if closer, ok := r.stream.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// fail tears down from our side and closes the transport when it can be closed.
func (r *RPC) fail(err error) {
	r.teardown(err)
	if closer, ok := r.stream.(io.Closer); ok {
		closer.Close()
	}
}

func (r *RPC) teardown(cause error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.closeErr = cause

	pending := r.pending
	r.pending = make(map[uint]chan *Message)

	var outgoing []*OutgoingStream
	var incoming []*IncomingStream
	for _, s := range r.outgoingRequests {
		outgoing = append(outgoing, s)
	}
	for _, s := range r.outgoingResponses {
		outgoing = append(outgoing, s)
	}
	for _, s := range r.incomingRequests {
		incoming = append(incoming, s)
	}
	for _, s := range r.incomingResponses {
		incoming = append(incoming, s)
	}
	clear(r.outgoingRequests)
	clear(r.outgoingResponses)
	clear(r.incomingRequests)
	clear(r.incomingResponses)
	clear(r.pendingOpenRequests)
	clear(r.pendingOpenResponses)
	r.mu.Unlock()

	for _, ch := range pending {
		close(ch)
	}
	for _, s := range outgoing {
		s.abort(cause)
	}
	for _, s := range incoming {
		s.abort(cause)
	}
}

func (r *RPC) noReplyError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.closeErr
	}
	return errNoReply
}

func (r *RPC) onResponse(msg *Message) {
	if msg.ID == 0 {
		return
	}
	// A response with a stream flag is the response-stream OPEN signal, not a reply.
	if msg.Error == nil && msg.Stream != 0 {
		return
	}

	r.mu.Lock()
	ch, ok := r.pending[msg.ID]
	delete(r.pending, msg.ID)
	var rs *IncomingStream
	var ws *OutgoingStream
	if msg.Error != nil {
		rs = r.incomingResponses[msg.ID]
		ws = r.outgoingRequests[msg.ID]
	}
	r.mu.Unlock()

	if ok {
		ch <- msg
	}
	// An error reply settles the whole request, streams included.
	if rs != nil {
		rs.abort(msg.Error)
	}
	if ws != nil {
		ws.abort(msg.Error)
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

func (r *RPC) openOutgoing(s *OutgoingStream) {
	r.mu.Lock()
	if r.closed {
		err := r.closeErr
		r.mu.Unlock()
		s.abort(err)
		return
	}
	var acked bool
	if s.mask&StreamRequest != 0 {
		r.outgoingRequests[s.id] = s
		acked = r.pendingOpenRequests[s.id]
		delete(r.pendingOpenRequests, s.id)
	} else {
		r.outgoingResponses[s.id] = s
		acked = r.pendingOpenResponses[s.id]
		delete(r.pendingOpenResponses, s.id)
	}
	r.mu.Unlock()

	s.open()
	if acked {
		s.continueOpen()
	}
}

func (r *RPC) openIncoming(s *IncomingStream) {
	r.mu.Lock()
	if r.closed {
		err := r.closeErr
		r.mu.Unlock()
		s.abort(err)
		return
	}
	if s.mask&StreamRequest != 0 {
		r.incomingRequests[s.id] = s
	} else {
		r.incomingResponses[s.id] = s
	}
	r.mu.Unlock()

	s.open()
}

func (r *RPC) onStreamClose(msg *Message) {
	s := r.incomingStream(msg.ID, msg.Stream)
	if s == nil {
		return
	}
	if msg.Error != nil {
		s.abort(msg.Error)
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
	s.abort(err)
}

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

func (r *RPC) outgoingClosed(id, mask uint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if mask&StreamRequest != 0 {
		delete(r.outgoingRequests, id)
	} else {
		delete(r.outgoingResponses, id)
	}
}

// incomingClosed forgets a reader; a finished response stream also settles the request it answered.
func (r *RPC) incomingClosed(id, mask uint) {
	r.mu.Lock()
	var ch chan *Message
	if mask&StreamRequest != 0 {
		delete(r.incomingRequests, id)
	} else {
		delete(r.incomingResponses, id)
		ch = r.pending[id]
		delete(r.pending, id)
	}
	r.mu.Unlock()

	if ch != nil {
		close(ch)
	}
}
