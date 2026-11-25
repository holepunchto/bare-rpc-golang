// Package barerpc is RPC over any byte transport, the Go counterpart of
// bare-rpc and wire compatible with it. It sends requests, events, and
// streams in both directions over one transport.
package barerpc

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// The defaults of Options.
const (
	DefaultMaxFrameSize     = 16 * 1024 * 1024
	DefaultMaxBufferedBytes = 16 * 1024 * 1024
)

// The most response streams that the remote end may open before their request
// arrives, as bare-rpc allows.
const maxPendingResponses = 1024

// Options configures New. The zero value holds the defaults.
type Options struct {
	// Handler handles the requests and events of the remote end. Defaults to
	// rejecting every request with UNKNOWN_COMMAND and dropping every event.
	Handler Handler
	// MaxFrameSize is the largest frame that the remote end may send,
	// including its 4-byte length. Zero means DefaultMaxFrameSize.
	MaxFrameSize int
	// MaxBufferedBytes is the most that an incoming stream may buffer before
	// the remote end is deemed to ignore its pauses. Zero means
	// DefaultMaxBufferedBytes.
	MaxBufferedBytes int
}

// An RPC is one end of an RPC channel over a transport.
//
// The channel ends when the transport ends or fails, when the remote end
// breaks the protocol, or when Close is called. Everything still in flight
// then fails: Requests in flight fail with the cause, their streams are
// destroyed, and the contexts of running handlers are cancelled with it.
type RPC struct {
	transport        io.ReadWriteCloser
	handler          Handler
	maxFrameSize     int
	maxBufferedBytes int

	reading chan struct{}
	writing chan struct{}

	closeOnce sync.Once
	closeErr  error

	mu   sync.Mutex
	cond *sync.Cond

	closed bool
	err    error

	queue   [][]byte
	queued  uint64
	written uint64

	id uint

	// The requests of this end, by id, that may still receive messages.
	outgoingRequests  map[uint]*OutgoingRequest
	incomingResponses map[uint]*OutgoingRequest

	// The requests of the remote end, by id, that may still receive messages.
	unansweredRequests map[uint]*Request
	incomingRequests   map[uint]*Request
	outgoingResponses  map[uint]*Request
	pendingResponses   map[uint]bool
	highestRequestID   uint

	// The requests and events of the remote end whose handlers are running,
	// and the requests among them by id, which a cancel still reaches once
	// they have been answered.
	handling map[aborter]bool
	running  map[uint]*Request
}

type aborter interface {
	abort(reason error)
}

// New starts an RPC channel over transport, which it reads and writes until
// the channel ends. A nil opts holds the defaults.
func New(transport io.ReadWriteCloser, opts *Options) *RPC {
	var o Options

	if opts != nil {
		o = *opts
	}

	if o.Handler == nil {
		o.Handler = &Router{}
	}

	if o.MaxFrameSize <= 0 {
		o.MaxFrameSize = DefaultMaxFrameSize
	}

	if o.MaxBufferedBytes <= 0 {
		o.MaxBufferedBytes = DefaultMaxBufferedBytes
	}

	r := &RPC{
		transport:          transport,
		handler:            o.Handler,
		maxFrameSize:       o.MaxFrameSize,
		maxBufferedBytes:   o.MaxBufferedBytes,
		reading:            make(chan struct{}),
		writing:            make(chan struct{}),
		outgoingRequests:   make(map[uint]*OutgoingRequest),
		incomingResponses:  make(map[uint]*OutgoingRequest),
		unansweredRequests: make(map[uint]*Request),
		incomingRequests:   make(map[uint]*Request),
		outgoingResponses:  make(map[uint]*Request),
		pendingResponses:   make(map[uint]bool),
		handling:           make(map[aborter]bool),
		running:            make(map[uint]*Request),
	}

	r.cond = sync.NewCond(&r.mu)

	go r.read()
	go r.write()

	return r
}

// Request sends a request and waits for its reply. If ctx is done first, the
// request is cancelled and fails with a CANCELLED or TIMEOUT *Error that
// wraps the context error.
func (r *RPC) Request(ctx context.Context, command uint, data []byte) ([]byte, error) {
	req := r.NewRequest(ctx, command)

	if err := req.Send(data); err != nil {
		return nil, err
	}

	return req.Reply()
}

// Event sends an event, which gets no reply.
func (r *RPC) Event(command uint, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return r.err
	}

	r.enqueue(&message{typ: typeRequest, command: command, data: data})

	return nil
}

// Idle reports whether nothing is in flight, so that the transport can be
// closed without failing anything.
func (r *RPC) Idle() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.outgoingRequests) == 0 &&
		len(r.outgoingResponses) == 0 &&
		len(r.incomingRequests) == 0 &&
		len(r.incomingResponses) == 0 &&
		len(r.pendingResponses) == 0
}

// Close ends the channel, failing everything in flight with ErrChannelClosed.
// It writes what was sent before it was called, closes the transport, and
// waits for the channel to end.
func (r *RPC) Close() error {
	r.teardown(ErrChannelClosed)

	<-r.writing
	<-r.reading

	return r.closeErr
}

// Wait waits for the channel to end. It returns nil if the channel closed
// cleanly, and otherwise the error that ended it.
func (r *RPC) Wait() error {
	<-r.writing
	<-r.reading

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.err == ErrChannelClosed {
		return nil
	}

	return r.err
}

func (r *RPC) closeTransport() {
	r.closeOnce.Do(func() {
		r.closeErr = r.transport.Close()
	})
}

// Queues a message for the writer, unless the channel has ended, and returns
// its sequence number for waitWritten. Must be called with r.mu held.
func (r *RPC) enqueue(m *message) uint64 {
	if r.closed {
		return 0
	}

	r.queue = append(r.queue, encodeFrame(m))
	r.queued++
	r.cond.Broadcast()

	return r.queued
}

// Waits for a queued message to be written, or for the channel to end. Must
// be called with r.mu held.
func (r *RPC) waitWritten(seq uint64) {
	for r.written < seq && !r.closed {
		r.cond.Wait()
	}
}

// Messages are written by a goroutine of their own, so that the reader never
// blocks on writing. Once the channel ends cleanly, what was queued is still
// written before the transport is closed.
func (r *RPC) write() {
	defer close(r.writing)
	defer r.closeTransport()

	r.mu.Lock()

	for {
		for len(r.queue) == 0 && !r.closed {
			r.cond.Wait()
		}

		if len(r.queue) == 0 {
			r.mu.Unlock()

			return
		}

		queue := r.queue
		r.queue = nil

		r.mu.Unlock()

		for _, frame := range queue {
			if _, err := r.transport.Write(frame); err != nil {
				r.fail(err)

				return
			}
		}

		r.mu.Lock()

		r.written += uint64(len(queue))
		r.cond.Broadcast()
	}
}

func (r *RPC) read() {
	defer close(r.reading)

	for {
		body, err := r.readFrame()
		if err != nil {
			if err == io.EOF {
				err = ErrChannelClosed
			}

			r.teardown(err)

			return
		}

		m, err := decodeBody(body)
		if err != nil {
			r.fail(err)
		} else {
			r.onMessage(m)
		}
	}
}

func (r *RPC) readFrame() ([]byte, error) {
	var header [4]byte

	if _, err := io.ReadFull(r.transport, header[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			err = newError(CodeInvalidMessage, "Truncated frame")
		}

		return nil, err
	}

	size := 4 + int64(binary.LittleEndian.Uint32(header[:]))

	if size > int64(r.maxFrameSize) {
		return nil, newError(CodeFrameTooLarge, fmt.Sprintf("Frame of %d bytes exceeds the limit of %d bytes", size, r.maxFrameSize))
	}

	body := make([]byte, size-4)

	if _, err := io.ReadFull(r.transport, body); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			err = newError(CodeInvalidMessage, "Truncated frame")
		}

		return nil, err
	}

	return body, nil
}

// Ends the channel because of a failure, without writing what is queued.
func (r *RPC) fail(err error) {
	r.teardown(err)
	r.closeTransport()
}

func (r *RPC) teardown(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}

	r.closed = true
	r.err = err

	if err != ErrChannelClosed {
		r.queue = nil
	}

	for _, req := range r.outgoingRequests {
		req.detach()

		if req.responseStream == nil {
			req.settle(nil, err)
		}
	}

	for _, req := range r.unansweredRequests {
		req.abort(err)
	}

	for a := range r.handling {
		a.abort(err)
	}

	for _, req := range r.incomingRequests {
		req.abort(err)
	}

	for _, req := range r.outgoingResponses {
		req.abort(err)
	}

	var (
		outgoing []*OutgoingStream
		incoming []*IncomingStream
	)

	for _, req := range r.outgoingRequests {
		if req.requestStream != nil {
			outgoing = append(outgoing, req.requestStream)
		}
	}

	for _, req := range r.outgoingResponses {
		outgoing = append(outgoing, req.responseStream)
	}

	for _, req := range r.incomingRequests {
		incoming = append(incoming, req.requestStream)
	}

	for _, req := range r.incomingResponses {
		incoming = append(incoming, req.responseStream)
	}

	clear(r.outgoingRequests)
	clear(r.outgoingResponses)
	clear(r.incomingRequests)
	clear(r.incomingResponses)
	clear(r.pendingResponses)
	clear(r.unansweredRequests)

	for _, s := range outgoing {
		s.destroy(err)
	}

	for _, s := range incoming {
		s.destroy(err)
	}

	r.cond.Broadcast()
}

func (r *RPC) onMessage(m *message) {
	r.mu.Lock()

	if r.closed {
		r.mu.Unlock()

		return
	}

	var err error

	switch m.typ {
	case typeRequest:
		err = r.onRequest(m)
	case typeResponse:
		r.onResponse(m)
	case typeStream:
		err = r.onStream(m)
	case typeCancel:
		r.onCancel(m)
	}

	r.mu.Unlock()

	if err != nil {
		r.fail(err)
	}
}

func (r *RPC) onRequest(m *message) error {
	if r.unansweredRequests[m.id] != nil || r.incomingRequests[m.id] != nil || r.outgoingResponses[m.id] != nil {
		return newError(CodeInvalidMessage, fmt.Sprintf("Request '%d' is already in flight", m.id))
	}

	ctx, cancel := context.WithCancelCause(context.Background())

	if m.id == 0 {
		event := &Event{Command: m.command, Data: m.data, cancel: cancel}

		r.handling[event] = true

		go r.handleEvent(ctx, event)

		return nil
	}

	req := &Request{
		ID:      m.id,
		Command: m.command,
		Data:    m.data,
		rpc:     r,
		ex:      exchange{id: m.id, command: m.command},
		cancel:  cancel,
	}

	r.unansweredRequests[m.id] = req
	r.highestRequestID = max(r.highestRequestID, m.id)
	r.handling[req] = true
	r.running[m.id] = req

	go r.handleRequest(ctx, req)

	return nil
}

func (r *RPC) handleEvent(ctx context.Context, event *Event) {
	err := r.handler.HandleEvent(ctx, event)

	r.mu.Lock()
	delete(r.handling, event)
	r.mu.Unlock()

	event.cancel(nil)

	if err != nil {
		r.fail(err)
	}
}

func (r *RPC) handleRequest(ctx context.Context, req *Request) {
	err := r.handler.HandleRequest(ctx, req)

	r.mu.Lock()

	delete(r.handling, req)

	if r.running[req.ex.id] == req {
		delete(r.running, req.ex.id)
	}

	if err != nil {
		if req.responseStream != nil {
			req.responseStream.destroy(err)
		} else if !req.sent {
			r.sendError(req, err)
		}
	}

	r.mu.Unlock()

	req.cancel(nil)
}

func (r *RPC) onResponse(m *message) {
	req := r.outgoingRequests[m.id]

	if req == nil {
		return
	}

	switch {
	case m.err != nil:
		req.respond(nil, m.err)

		if req.responseStream != nil {
			req.responseStream.destroy(m.err)
		}

		req.releaseRequestStream(m.err)
	case m.stream == 0:
		req.respond(m.data, nil)

		if s := req.responseStream; s != nil && s.pushable() {
			if len(m.data) > 0 {
				s.push(m.data)
			}

			s.pushEnd()
		}

		req.releaseRequestStream(nil)
	case req.responseStream == nil:
		err := newError(CodeUnexpectedStream, fmt.Sprintf("Request '%d' was answered with a stream", req.ex.id))

		req.respond(nil, err)

		r.enqueue(&message{typ: typeStream, id: m.id, stream: streamResponse | streamDestroy})

		req.releaseRequestStream(err)
	}
}

func (r *RPC) onCancel(m *message) {
	req := r.unansweredRequests[m.id]

	if req == nil {
		req = r.incomingRequests[m.id]
	}

	if req == nil {
		req = r.outgoingResponses[m.id]
	}

	// The response stream of a request may have closed just before its
	// cancel arrived, as the remote end destroys the stream first, while
	// its handler still runs.
	if req == nil {
		req = r.running[m.id]
	}

	if req == nil {
		return
	}

	req.ex.cancelled = true

	r.onAnswered(req)

	req.abort(m.err)

	if req.requestStream != nil {
		req.requestStream.destroy(m.err)
	}

	if req.responseStream != nil {
		req.responseStream.destroy(m.err)
	}
}

func (r *RPC) onStream(m *message) error {
	switch {
	case m.stream&streamOpen != 0:
		return r.onStreamOpen(m)
	case m.stream&streamClose != 0:
		if s := r.incomingStream(m); s != nil {
			s.finish()

			if m.err != nil {
				s.destroy(m.err)
			} else {
				s.pushEnd()
			}
		}
	case m.stream&streamPause != 0:
		if s := r.outgoingStream(m); s != nil {
			s.corked = true
		}
	case m.stream&streamResume != 0:
		if s := r.outgoingStream(m); s != nil {
			s.corked = false
			r.cond.Broadcast()
		}
	case m.stream&streamData != 0:
		return r.onStreamData(m)
	case m.stream&streamEnd != 0:
		if s := r.incomingStream(m); s != nil {
			s.finish()
			s.pushEnd()
		}
	case m.stream&streamDestroy != 0:
		if m.stream&streamResponse != 0 {
			delete(r.pendingResponses, m.id)
		}

		if s := r.outgoingStream(m); s != nil {
			var err error

			if m.err != nil {
				err = m.err
			}

			s.destroy(err)
		}
	}

	return nil
}

func (r *RPC) onStreamOpen(m *message) error {
	if m.stream&streamRequest != 0 {
		if s := r.outgoingStream(m); s != nil {
			s.acknowledge()
		}

		return nil
	}

	if req := r.outgoingResponses[m.id]; req != nil {
		req.responseStream.acknowledge()

		return nil
	}

	// The remote end opens its response stream as soon as it is created,
	// which may be before its request arrives or before the handler here
	// creates the response stream. The open is remembered until either
	// happens, the request is answered, or the remote end destroys its
	// stream. Any other open is for a request that has been answered.
	if r.pendingResponses[m.id] {
		return nil
	}

	if r.unansweredRequests[m.id] == nil && m.id <= r.highestRequestID {
		return nil
	}

	if len(r.pendingResponses) >= maxPendingResponses {
		return newError(CodeInvalidMessage, "Too many response streams opened early")
	}

	r.pendingResponses[m.id] = true

	return nil
}

func (r *RPC) onStreamData(m *message) error {
	s := r.incomingStream(m)

	if s == nil || !s.pushable() {
		return nil
	}

	s.push(m.data)

	if s.buffered > r.maxBufferedBytes {
		return newError(CodeStreamOverflow, fmt.Sprintf("Stream buffered more than the limit of %d bytes", r.maxBufferedBytes))
	}

	if !s.paused && s.buffered >= highWaterMark {
		s.paused = true

		r.enqueue(&message{typ: typeStream, id: m.id, stream: s.mask | streamPause})
	}

	return nil
}

func (r *RPC) incomingStream(m *message) *IncomingStream {
	if m.stream&streamRequest != 0 {
		if req := r.incomingRequests[m.id]; req != nil {
			return req.requestStream
		}
	} else if req := r.incomingResponses[m.id]; req != nil {
		return req.responseStream
	}

	return nil
}

func (r *RPC) outgoingStream(m *message) *OutgoingStream {
	if m.stream&streamRequest != 0 {
		if req := r.outgoingRequests[m.id]; req != nil {
			return req.requestStream
		}
	} else if req := r.outgoingResponses[m.id]; req != nil {
		return req.responseStream
	}

	return nil
}

func (r *RPC) isUnanswered(req *Request) bool {
	return r.unansweredRequests[req.ex.id] == req
}

// Returns whether the remote end had already opened its response stream.
func (r *RPC) onAnswered(req *Request) bool {
	delete(r.unansweredRequests, req.ex.id)

	opened := r.pendingResponses[req.ex.id]

	delete(r.pendingResponses, req.ex.id)

	return opened
}

func (r *RPC) sendError(req *Request, err error) {
	if !r.isUnanswered(req) {
		return
	}

	r.onAnswered(req)

	r.enqueue(&message{typ: typeResponse, id: req.ex.id, err: toError(err)})
}

var errNotSent = errors.New("barerpc: request has not been sent")
