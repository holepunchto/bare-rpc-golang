package bare_rpc

import (
	"errors"
	"io"
	"sync"
)

// defaultHighWater is the buffered byte count at which a reader pauses the remote writer.
const defaultHighWater = 16 * 1024

var (
	errStreamClosed    = errors.New("rpc: stream closed")
	errStreamDestroyed = errors.New("rpc: stream destroyed")
)

// IncomingStream is the readable end of a stream; it pauses and resumes the remote writer as it buffers.
type IncomingStream struct {
	rpc  *RPC
	id   uint
	mask uint

	mu        sync.Mutex
	cond      *sync.Cond
	queue     [][]byte
	buffered  int
	highWater int
	paused    bool
	ended     bool
	closed    bool
	err       error
}

func newIncomingStream(rpc *RPC, id, mask uint) *IncomingStream {
	s := &IncomingStream{rpc: rpc, id: id, mask: mask, highWater: defaultHighWater}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *IncomingStream) open() error {
	return s.rpc.sendStream(s.id, s.mask|StreamOpen, nil)
}

func (s *IncomingStream) done() bool {
	return s.closed || s.ended || s.err != nil
}

func (s *IncomingStream) push(data []byte) {
	s.mu.Lock()
	if s.done() {
		s.mu.Unlock()
		return
	}
	if len(data) > 0 {
		s.queue = append(s.queue, data)
		s.buffered += len(data)
	}
	pause := !s.paused && s.buffered >= s.highWater
	if pause {
		s.paused = true
	}
	s.cond.Broadcast()
	s.mu.Unlock()

	if pause {
		s.rpc.sendStream(s.id, s.mask|StreamPause, nil)
	}
}

func (s *IncomingStream) pushEOF() {
	s.mu.Lock()
	if s.done() {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.cond.Broadcast()
	s.mu.Unlock()

	s.rpc.incomingClosed(s.id, s.mask)
}

// abort fails the stream; buffered data stays readable, then reads return err.
func (s *IncomingStream) abort(err error) {
	s.mu.Lock()
	if s.done() {
		s.mu.Unlock()
		return
	}
	if err == nil {
		err = errStreamDestroyed
	}
	s.err = err
	s.cond.Broadcast()
	s.mu.Unlock()

	s.rpc.incomingClosed(s.id, s.mask)
}

func (s *IncomingStream) wait() error {
	for len(s.queue) == 0 && !s.done() {
		s.cond.Wait()
	}
	if len(s.queue) > 0 {
		return nil
	}
	if s.err != nil {
		return s.err
	}
	if s.closed {
		return errStreamClosed
	}
	return io.EOF
}

func (s *IncomingStream) consumed(n int) bool {
	s.buffered -= n
	resume := s.paused && s.buffered < s.highWater && !s.done()
	if resume {
		s.paused = false
	}
	return resume
}

// Read implements io.Reader over the stream's bytes without preserving frame boundaries.
func (s *IncomingStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	if err := s.wait(); err != nil {
		s.mu.Unlock()
		return 0, err
	}

	chunk := s.queue[0]
	n := copy(p, chunk)
	if n < len(chunk) {
		s.queue[0] = chunk[n:]
	} else {
		s.queue = s.queue[1:]
	}
	resume := s.consumed(n)
	s.mu.Unlock()

	if resume {
		s.rpc.sendStream(s.id, s.mask|StreamResume, nil)
	}
	return n, nil
}

// ReadChunk returns the next DATA frame whole, or io.EOF after a graceful end.
func (s *IncomingStream) ReadChunk() ([]byte, error) {
	s.mu.Lock()
	if err := s.wait(); err != nil {
		s.mu.Unlock()
		return nil, err
	}

	chunk := s.queue[0]
	s.queue = s.queue[1:]
	resume := s.consumed(len(chunk))
	s.mu.Unlock()

	if resume {
		s.rpc.sendStream(s.id, s.mask|StreamResume, nil)
	}
	return chunk, nil
}

// Close stops reading and sends DESTROY to the remote writer.
func (s *IncomingStream) Close() error {
	return s.Destroy(nil)
}

// Destroy stops reading and sends DESTROY|ERROR carrying cause (bare DESTROY when nil).
func (s *IncomingStream) Destroy(cause error) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	aborted := s.err != nil
	s.cond.Broadcast()
	s.mu.Unlock()

	s.rpc.incomingClosed(s.id, s.mask)

	if aborted {
		return nil
	}
	if cause != nil {
		return s.rpc.sendStreamError(s.id, s.mask|StreamDestroy, cause)
	}
	return s.rpc.sendStream(s.id, s.mask|StreamDestroy, nil)
}

// OutgoingStream is the writable end of a stream; writes wait for the OPEN ack and honour remote pauses.
type OutgoingStream struct {
	rpc     *RPC
	id      uint
	command uint
	typ     uint
	mask    uint

	mu     sync.Mutex
	cond   *sync.Cond
	opened bool
	corked bool
	closed bool
	err    error
}

func newOutgoingStream(rpc *RPC, id, command, typ, mask uint) *OutgoingStream {
	s := &OutgoingStream{rpc: rpc, id: id, command: command, typ: typ, mask: mask}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *OutgoingStream) open() error {
	switch s.typ {
	case TypeRequest:
		return s.rpc.Send(&Message{Type: TypeRequest, ID: s.id, Command: s.command, Stream: StreamOpen})
	case TypeResponse:
		return s.rpc.Send(&Message{Type: TypeResponse, ID: s.id, Stream: StreamOpen})
	}
	return nil
}

func (s *OutgoingStream) continueOpen() {
	s.mu.Lock()
	s.opened = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *OutgoingStream) cork() {
	s.mu.Lock()
	s.corked = true
	s.mu.Unlock()
}

func (s *OutgoingStream) uncork() {
	s.mu.Lock()
	s.corked = false
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *OutgoingStream) abort(err error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if err == nil {
		err = errStreamDestroyed
	}
	s.closed = true
	s.err = err
	s.cond.Broadcast()
	s.mu.Unlock()

	s.rpc.outgoingClosed(s.id, s.mask)
}

// Write sends p as one DATA frame, blocking while the stream is unopened or paused.
func (s *OutgoingStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	for {
		if s.err != nil {
			err := s.err
			s.mu.Unlock()
			return 0, err
		}
		if s.closed {
			s.mu.Unlock()
			return 0, errStreamClosed
		}
		if s.opened && !s.corked {
			break
		}
		s.cond.Wait()
	}
	s.mu.Unlock()

	data := make([]byte, len(p))
	copy(data, p)
	if err := s.rpc.sendStream(s.id, s.mask|StreamData, data); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close ends the stream gracefully with END then CLOSE.
func (s *OutgoingStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()

	s.rpc.outgoingClosed(s.id, s.mask)
	if err := s.rpc.sendStream(s.id, s.mask|StreamEnd, nil); err != nil {
		return err
	}
	return s.rpc.sendStream(s.id, s.mask|StreamClose, nil)
}

// Destroy aborts the stream, sending CLOSE|ERROR carrying cause (bare CLOSE when nil).
func (s *OutgoingStream) Destroy(cause error) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.err = errStreamDestroyed
	s.cond.Broadcast()
	s.mu.Unlock()

	s.rpc.outgoingClosed(s.id, s.mask)
	if cause != nil {
		return s.rpc.sendStreamError(s.id, s.mask|StreamClose, cause)
	}
	return s.rpc.sendStream(s.id, s.mask|StreamClose, nil)
}
