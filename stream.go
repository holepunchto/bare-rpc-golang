package barerpc

import "io"

// The most that an incoming stream buffers before it pauses the remote end.
const highWaterMark = 16 * 1024

// The state that a request shares with its streams.
type exchange struct {
	id      uint
	command uint
	// Why the request was cancelled or aborted, if it was.
	reason error
	// Whether the cancellation crossed the wire.
	cancelled bool
}

// An IncomingStream reads a stream from the remote end, either a request sent
// as a stream or the answer to a request. The remote end is paused while the
// stream buffers more than it has read.
type IncomingStream struct {
	rpc     *RPC
	ex      *exchange
	typ     uint
	mask    uint
	onClose func(*IncomingStream)

	opened    bool
	chunks    [][]byte
	buffered  int
	paused    bool
	ended     bool
	destroyed bool
	err       error
}

func newIncomingStream(r *RPC, ex *exchange, typ uint, onClose func(*IncomingStream)) *IncomingStream {
	return &IncomingStream{rpc: r, ex: ex, typ: typ, mask: maskOf(typ), onClose: onClose}
}

func maskOf(typ uint) uint {
	if typ == typeRequest {
		return streamRequest
	}

	return streamResponse
}

// Read reads the bytes of the stream, returning io.EOF once the remote end
// has ended it. It fails with the error that the stream was destroyed with,
// or io.ErrClosedPipe if there is none.
func (s *IncomingStream) Read(b []byte) (int, error) {
	r := s.rpc

	r.mu.Lock()
	defer r.mu.Unlock()

	if err := s.wait(); err != nil {
		return 0, err
	}

	n := copy(b, s.chunks[0])

	if n == len(s.chunks[0]) {
		s.chunks = s.chunks[1:]
	} else {
		s.chunks[0] = s.chunks[0][n:]
	}

	s.consumed(n)

	return n, nil
}

// ReadChunk reads the next chunk of the stream whole, as written by a single
// write of the remote end, returning io.EOF once it has ended the stream.
func (s *IncomingStream) ReadChunk() ([]byte, error) {
	r := s.rpc

	r.mu.Lock()
	defer r.mu.Unlock()

	if err := s.wait(); err != nil {
		return nil, err
	}

	chunk := s.chunks[0]

	s.chunks = s.chunks[1:]

	s.consumed(len(chunk))

	return chunk, nil
}

// Close stops reading the stream, telling the remote end to stop writing it.
func (s *IncomingStream) Close() error {
	return s.Destroy(nil)
}

// Destroy stops reading the stream, telling the remote end to stop writing it
// with cause, if not nil.
func (s *IncomingStream) Destroy(cause error) error {
	r := s.rpc

	r.mu.Lock()
	defer r.mu.Unlock()

	s.destroy(cause)

	return nil
}

func (s *IncomingStream) wait() error {
	r := s.rpc

	for len(s.chunks) == 0 && !s.ended && !s.destroyed {
		r.cond.Wait()
	}

	if len(s.chunks) > 0 {
		return nil
	}

	if s.destroyed {
		if s.err != nil {
			return s.err
		}

		return io.ErrClosedPipe
	}

	s.close()

	return io.EOF
}

func (s *IncomingStream) consumed(n int) {
	s.buffered -= n

	if s.paused && s.buffered < highWaterMark {
		s.paused = false

		s.rpc.enqueue(&message{typ: typeStream, id: s.ex.id, stream: s.mask | streamResume})
	}
}

func (s *IncomingStream) open() {
	s.opened = true

	s.rpc.enqueue(&message{typ: typeStream, id: s.ex.id, stream: s.mask | streamOpen})
}

func (s *IncomingStream) pushable() bool {
	return !s.destroyed && !s.ended
}

func (s *IncomingStream) push(data []byte) {
	s.chunks = append(s.chunks, data)
	s.buffered += len(data)

	s.rpc.cond.Broadcast()
}

func (s *IncomingStream) pushEnd() {
	if !s.pushable() {
		return
	}

	s.ended = true

	s.rpc.cond.Broadcast()
}

// A streamed answer has arrived once its end arrives, even if it hasn't been
// read to the end yet.
func (s *IncomingStream) finish() {
	if s.typ != typeResponse {
		return
	}

	if req := s.rpc.incomingResponses[s.ex.id]; req != nil {
		req.onResponded()
	}
}

// Closes a stream that has been read to the end.
func (s *IncomingStream) close() {
	if s.destroyed {
		return
	}

	s.destroyed = true

	s.onClose(s)
}

func (s *IncomingStream) destroy(cause error) {
	if s.destroyed {
		return
	}

	s.destroyed = true
	s.err = cause
	s.chunks = nil
	s.buffered = 0

	r := s.rpc

	// A request cancelled before it was sent only has to take back an open
	// that already went out, as the remote end never learns of the cancel.
	switch {
	case s.ex.reason != nil && (s.ex.cancelled || !s.opened):
	case cause != nil:
		r.enqueue(&message{typ: typeStream, id: s.ex.id, stream: s.mask | streamDestroy | streamError, err: toError(cause)})
	default:
		r.enqueue(&message{typ: typeStream, id: s.ex.id, stream: s.mask | streamDestroy})
	}

	r.cond.Broadcast()

	s.onClose(s)
}

// An OutgoingStream writes a stream to the remote end, either a request sent
// as a stream or the answer to a request. Writes wait for the remote end to
// open the stream, and for it to resume the stream while it is paused.
type OutgoingStream struct {
	rpc     *RPC
	ex      *exchange
	typ     uint
	mask    uint
	onClose func()

	acknowledged bool
	corked       bool
	ended        bool
	destroyed    bool
	err          error
}

func newOutgoingStream(r *RPC, ex *exchange, typ uint, onClose func()) *OutgoingStream {
	return &OutgoingStream{rpc: r, ex: ex, typ: typ, mask: maskOf(typ), onClose: onClose}
}

// Write writes b to the stream as a single chunk and waits for it to be
// written to the transport. It fails with the error that the stream was
// destroyed with, or io.ErrClosedPipe if there is none or the stream has been
// closed.
func (s *OutgoingStream) Write(b []byte) (int, error) {
	r := s.rpc

	r.mu.Lock()
	defer r.mu.Unlock()

	for !s.destroyed && !s.ended && (!s.acknowledged || s.corked) {
		r.cond.Wait()
	}

	if err := s.closedErr(); err != nil {
		return 0, err
	}

	seq := r.enqueue(&message{typ: typeStream, id: s.ex.id, stream: s.mask | streamData, data: append([]byte(nil), b...)})

	r.waitWritten(seq)

	if s.destroyed && s.err != nil {
		return 0, s.err
	}

	return len(b), nil
}

// Close ends the stream once the remote end has opened it.
func (s *OutgoingStream) Close() error {
	r := s.rpc

	r.mu.Lock()
	defer r.mu.Unlock()

	for !s.destroyed && !s.ended && !s.acknowledged {
		r.cond.Wait()
	}

	if s.destroyed || s.ended {
		return s.err
	}

	s.ended = true
	s.destroyed = true

	r.enqueue(&message{typ: typeStream, id: s.ex.id, stream: s.mask | streamEnd})

	s.onClose()

	return nil
}

// Destroy stops writing the stream, telling the remote end that it failed
// with cause, if not nil.
func (s *OutgoingStream) Destroy(cause error) error {
	r := s.rpc

	r.mu.Lock()
	defer r.mu.Unlock()

	s.destroy(cause)

	return nil
}

func (s *OutgoingStream) closedErr() error {
	if s.destroyed && s.err != nil {
		return s.err
	}

	if s.destroyed || s.ended {
		return io.ErrClosedPipe
	}

	return nil
}

func (s *OutgoingStream) sendOpen() {
	switch s.typ {
	case typeRequest:
		s.rpc.enqueue(&message{typ: typeRequest, id: s.ex.id, command: s.ex.command, stream: streamOpen})
	case typeResponse:
		s.rpc.enqueue(&message{typ: typeResponse, id: s.ex.id, stream: streamOpen})
	}
}

func (s *OutgoingStream) acknowledge() {
	s.acknowledged = true

	s.rpc.cond.Broadcast()
}

func (s *OutgoingStream) destroy(cause error) {
	if s.destroyed {
		return
	}

	s.destroyed = true
	s.err = cause

	r := s.rpc

	switch {
	case s.ended || s.ex.reason != nil:
	case cause != nil:
		r.enqueue(&message{typ: typeStream, id: s.ex.id, stream: s.mask | streamClose | streamError, err: toError(cause)})
	default:
		r.enqueue(&message{typ: typeStream, id: s.ex.id, stream: s.mask | streamClose})
	}

	r.cond.Broadcast()

	s.onClose()
}
