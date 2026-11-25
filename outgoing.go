package barerpc

import "context"

// An OutgoingRequest is a request to the remote end. It is sent either with
// Send, as a single message, or with CreateRequestStream, as a stream, and is
// answered either with a reply, read with Reply, or with a stream, read from
// CreateResponseStream.
//
// The request is cancelled if its context is done before it is answered,
// failing with a CANCELLED or TIMEOUT *Error that wraps the context error.
type OutgoingRequest struct {
	rpc *RPC
	ctx context.Context
	ex  exchange

	sent      bool
	received  bool
	responded bool
	stop      func() bool

	settled bool
	data    []byte
	err     error

	requestStream  *OutgoingStream
	responseStream *IncomingStream
}

// NewRequest creates a request for command, governed by ctx, without sending
// it.
func (r *RPC) NewRequest(ctx context.Context, command uint) *OutgoingRequest {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.id++

	return &OutgoingRequest{rpc: r, ctx: ctx, ex: exchange{id: r.id, command: command}}
}

// Send sends the request with data. It fails with an ALREADY_SENT *Error if
// the request has been sent, and with the reason that the request failed if
// it can't be sent, such as the channel having closed.
func (req *OutgoingRequest) Send(data []byte) error {
	r := req.rpc

	r.mu.Lock()
	defer r.mu.Unlock()

	if req.sent {
		return newError(CodeAlreadySent, "Request has already been sent")
	}

	req.cancelIfDone()

	req.sent = true

	if err := req.failure(); err != nil {
		return err
	}

	req.track()

	r.enqueue(&message{typ: typeRequest, id: req.ex.id, command: req.ex.command, data: data})

	return nil
}

// CreateRequestStream sends the request as a stream instead of with Send. It
// fails with an ALREADY_SENT *Error if the request has been sent.
func (req *OutgoingRequest) CreateRequestStream() (*OutgoingStream, error) {
	r := req.rpc

	r.mu.Lock()
	defer r.mu.Unlock()

	if req.sent {
		return nil, newError(CodeAlreadySent, "Request has already been sent")
	}

	req.cancelIfDone()

	req.sent = true

	s := newOutgoingStream(r, &req.ex, typeRequest, func() { req.collect() })

	req.requestStream = s

	if err := req.failure(); err != nil {
		s.destroy(err)

		return s, nil
	}

	req.track()

	// The stream is announced right away rather than once it opens, so that
	// the remote end also learns of a stream destroyed before it opens.
	s.sendOpen()

	return s, nil
}

// CreateResponseStream reads the answer of the request as a stream instead of
// with Reply. A reply that isn't a stream arrives as a single chunk. Closing
// the stream before the answer has arrived cancels the request. It fails with
// an ALREADY_RECEIVED *Error if the answer is already being received.
func (req *OutgoingRequest) CreateResponseStream() (*IncomingStream, error) {
	r := req.rpc

	r.mu.Lock()
	defer r.mu.Unlock()

	if req.received {
		return nil, newError(CodeAlreadyReceived, "Response has already been received")
	}

	req.received = true

	s := newIncomingStream(r, &req.ex, typeResponse, func(s *IncomingStream) {
		delete(r.incomingResponses, req.ex.id)

		// A response stream closed before the answer has arrived leaves no
		// way to read it, so the request is cancelled.
		req.cancel(s.err)
	})

	req.responseStream = s

	if err := req.failure(); err != nil {
		s.destroy(err)

		return s, nil
	}

	r.incomingResponses[req.ex.id] = req

	s.open()

	return s, nil
}

// Reply waits for the reply to the request. It fails with the error that the
// remote end rejected the request with, or with the reason that the request
// failed, such as its context being done or the channel having closed.
func (req *OutgoingRequest) Reply() ([]byte, error) {
	r := req.rpc

	r.mu.Lock()
	defer r.mu.Unlock()

	if req.received {
		return nil, newError(CodeAlreadyReceived, "Response is already being received")
	}

	if !req.sent {
		return nil, errNotSent
	}

	req.received = true

	if r.closed && !req.responded {
		req.settle(nil, r.err)
	}

	for !req.settled {
		r.cond.Wait()
	}

	return req.data, req.err
}

// Cancel cancels the request unless it has been answered, telling the remote
// end if it has been sent. The request fails with cause, which defaults to a
// CANCELLED *Error.
func (req *OutgoingRequest) Cancel(cause error) {
	r := req.rpc

	r.mu.Lock()
	defer r.mu.Unlock()

	req.cancel(cause)
}

func (req *OutgoingRequest) cancel(cause error) {
	r := req.rpc

	// A request that has its answer has finished, even while its request
	// stream is still open, and so has one that was sent but is no longer
	// tracked.
	if req.ex.reason != nil || req.responded {
		return
	}

	if req.sent && r.outgoingRequests[req.ex.id] != req {
		return
	}

	if cause == nil {
		cause = newError(CodeCancelled, "Request was cancelled")
	}

	req.ex.reason = cause

	if req.sent {
		req.ex.cancelled = true

		delete(r.outgoingRequests, req.ex.id)

		req.detach()

		r.enqueue(&message{typ: typeCancel, id: req.ex.id, err: cancelError(cause)})
	}

	req.settle(nil, cause)

	if req.requestStream != nil {
		req.requestStream.destroy(cause)
	}

	if req.responseStream != nil {
		req.responseStream.destroy(cause)
	}
}

func (req *OutgoingRequest) cancelIfDone() {
	if req.ctx.Err() != nil {
		req.cancel(contextError(req.ctx))
	}
}

// The error that keeps the request from using the channel, if any.
func (req *OutgoingRequest) failure() error {
	if req.rpc.closed {
		return req.rpc.err
	}

	return req.ex.reason
}

func (req *OutgoingRequest) track() {
	r := req.rpc

	r.outgoingRequests[req.ex.id] = req

	req.stop = context.AfterFunc(req.ctx, func() {
		req.Cancel(contextError(req.ctx))
	})
}

func (req *OutgoingRequest) detach() {
	if req.stop != nil {
		req.stop()
		req.stop = nil
	}
}

func (req *OutgoingRequest) settle(data []byte, err error) {
	if req.settled {
		return
	}

	req.settled = true
	req.data = data
	req.err = err

	req.rpc.cond.Broadcast()
}

// Settles the request with its answer.
func (req *OutgoingRequest) respond(data []byte, err error) {
	req.onResponded()
	req.settle(data, err)
}

func (req *OutgoingRequest) onResponded() {
	req.responded = true
	req.detach()
	req.collect()
}

// The request is forgotten once no message can arrive for it: Once it has
// been answered and, if it was sent as a stream, once that has closed.
func (req *OutgoingRequest) collect() {
	if !req.responded {
		return
	}

	if req.requestStream != nil && !req.requestStream.destroyed {
		return
	}

	delete(req.rpc.outgoingRequests, req.ex.id)
}

// Once the request is answered, a request stream that the remote end never
// opened will never be read, and would otherwise wait for its open forever.
func (req *OutgoingRequest) releaseRequestStream(err error) {
	if s := req.requestStream; s != nil && !s.acknowledged {
		s.destroy(err)
	}
}
