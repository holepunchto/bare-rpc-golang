package bare_rpc

import (
	"errors"
	"io"
	"sync"
)

// defaultHighWater is the number of buffered bytes on an IncomingStream that
// triggers a PAUSE back to the remote writer. It mirrors bare-stream's default
// readable high-water mark closely enough for compatible flow control.
const defaultHighWater = 16 * 1024

var (
	errStreamClosed    = errors.New("rpc: stream closed")
	errStreamDestroyed = errors.New("rpc: stream destroyed")
)

// IncomingStream is the readable end of an RPC stream. It implements
// io.ReadCloser. Data arrives via the connection's read loop (push); the
// consumer drains it with Read. When buffered data crosses the high-water mark
// a PAUSE is sent to the remote writer, and a RESUME once it drains again.
type IncomingStream struct {
	rpc  *RPC
	id   uint
	mask uint // StreamRequest or StreamResponse

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

// open sends the OPEN acknowledgement that lets the remote writer start flowing.
func (s *IncomingStream) open() error {
	return s.rpc.sendStream(s.id, s.mask|StreamOpen, nil)
}

// push enqueues a DATA frame and pauses the remote writer if we're over budget.
func (s *IncomingStream) push(data []byte) {
	s.mu.Lock()
	if s.closed || s.ended || s.err != nil {
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

// pushEOF marks a graceful end; pending reads drain then return io.EOF.
func (s *IncomingStream) pushEOF() {
	s.mu.Lock()
	s.ended = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

// destroyFromRemote aborts the stream because the writer signalled CLOSE|ERROR.
func (s *IncomingStream) destroyFromRemote(err error) {
	s.mu.Lock()
	if s.err == nil {
		if err != nil {
			s.err = err
		} else {
			s.err = errStreamDestroyed
		}
	}
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *IncomingStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	for len(s.queue) == 0 && !s.ended && !s.closed && s.err == nil {
		s.cond.Wait()
	}

	if len(s.queue) == 0 {
		err := s.err
		closed := s.closed
		s.mu.Unlock()
		if err != nil {
			return 0, err
		}
		if closed {
			return 0, errStreamClosed
		}
		return 0, io.EOF
	}

	chunk := s.queue[0]
	n := copy(p, chunk)
	if n < len(chunk) {
		s.queue[0] = chunk[n:]
	} else {
		s.queue = s.queue[1:]
	}
	s.buffered -= n

	resume := s.paused && s.buffered < s.highWater
	if resume {
		s.paused = false
	}
	s.mu.Unlock()

	if resume {
		s.rpc.sendStream(s.id, s.mask|StreamResume, nil)
	}
	return n, nil
}

// Close tears down the readable end and signals the remote writer via DESTROY.
func (s *IncomingStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()

	s.rpc.removeIncoming(s.id, s.mask)
	return s.rpc.sendStream(s.id, s.mask|StreamDestroy, nil)
}

// OutgoingStream is the writable end of an RPC stream. It implements
// io.WriteCloser. The first Write blocks until the remote reader has
// acknowledged the OPEN handshake; subsequent writes block while the remote
// reader has us paused (cork/uncork).
type OutgoingStream struct {
	rpc     *RPC
	id      uint
	command uint
	typ     uint // TypeRequest or TypeResponse, used for the OPEN frame
	mask    uint // StreamRequest or StreamResponse

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

// open kicks off the handshake by sending a bare OPEN on a REQUEST/RESPONSE
// frame. The matching reader replies with mask|OPEN, which lands in
// continueOpen and unblocks writes.
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

// destroyFromRemote aborts the writer because the reader sent DESTROY.
func (s *OutgoingStream) destroyFromRemote(err error) {
	s.mu.Lock()
	if s.err == nil {
		if err != nil {
			s.err = err
		} else {
			s.err = errStreamDestroyed
		}
	}
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

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

	// Copy: Send encodes synchronously, but the caller may reuse p afterwards.
	data := make([]byte, len(p))
	copy(data, p)
	if err := s.rpc.sendStream(s.id, s.mask|StreamData, data); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close ends the stream gracefully: END (flush) followed by CLOSE (teardown).
func (s *OutgoingStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	err := s.err
	s.cond.Broadcast()
	s.mu.Unlock()

	s.rpc.removeOutgoing(s.id, s.mask)
	if err != nil {
		return err
	}
	if e := s.rpc.sendStream(s.id, s.mask|StreamEnd, nil); e != nil {
		return e
	}
	return s.rpc.sendStream(s.id, s.mask|StreamClose, nil)
}

// Destroy aborts the stream, signalling the remote reader with CLOSE|ERROR.
func (s *OutgoingStream) Destroy(cause error) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	if s.err == nil {
		s.err = errStreamDestroyed
	}
	s.cond.Broadcast()
	s.mu.Unlock()

	s.rpc.removeOutgoing(s.id, s.mask)
	if cause != nil {
		return s.rpc.sendStreamError(s.id, s.mask|StreamClose, cause)
	}
	return s.rpc.sendStream(s.id, s.mask|StreamClose, nil)
}
