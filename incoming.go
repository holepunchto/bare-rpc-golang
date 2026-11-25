package barerpc

import "context"

// A Request is a request from the remote end, as passed to a Handler. It is
// answered either with Reply or with CreateResponseStream, and a request sent
// as a stream is read from CreateRequestStream.
type Request struct {
	// ID is the ID of the request, unique among those in flight.
	ID uint
	// Command is the command of the request.
	Command uint
	// Data is the data that the request was sent with, or nil if it was sent
	// as a stream.
	Data []byte

	rpc    *RPC
	ex     exchange
	cancel context.CancelCauseFunc

	sent     bool
	received bool

	requestStream  *IncomingStream
	responseStream *OutgoingStream
}

// Reply answers the request with data. It fails with an ALREADY_SENT *Error
// if the request has been answered. A reply to a request that was cancelled
// or whose channel has closed is dropped.
func (req *Request) Reply(data []byte) error {
	r := req.rpc

	r.mu.Lock()
	defer r.mu.Unlock()

	if req.sent {
		return newError(CodeAlreadySent, "Response has already been sent")
	}

	req.sent = true

	if r.isUnanswered(req) {
		r.onAnswered(req)
		r.enqueue(&message{typ: typeResponse, id: req.ex.id, data: data})
	}

	return nil
}

// Sent reports whether the request has been answered.
func (req *Request) Sent() bool {
	r := req.rpc

	r.mu.Lock()
	defer r.mu.Unlock()

	return req.sent
}

// CreateResponseStream answers the request with a stream instead of with
// Reply. It fails with an ALREADY_SENT *Error if the request has been
// answered.
func (req *Request) CreateResponseStream() (*OutgoingStream, error) {
	r := req.rpc

	r.mu.Lock()
	defer r.mu.Unlock()

	if req.sent {
		return nil, newError(CodeAlreadySent, "Response has already been sent")
	}

	req.sent = true

	s := newOutgoingStream(r, &req.ex, typeResponse, func() {
		delete(r.outgoingResponses, req.ex.id)
	})

	req.responseStream = s

	if err := req.failure(); err != nil {
		s.destroy(err)

		return s, nil
	}

	r.outgoingResponses[req.ex.id] = req

	s.sendOpen()

	if r.onAnswered(req) {
		s.acknowledge()
	}

	return s, nil
}

// CreateRequestStream reads a request that was sent as a stream. It fails
// with an ALREADY_RECEIVED *Error if the request is already being received.
func (req *Request) CreateRequestStream() (*IncomingStream, error) {
	r := req.rpc

	r.mu.Lock()
	defer r.mu.Unlock()

	if req.received {
		return nil, newError(CodeAlreadyReceived, "Request has already been received")
	}

	req.received = true

	s := newIncomingStream(r, &req.ex, typeRequest, func(*IncomingStream) {
		delete(r.incomingRequests, req.ex.id)
	})

	req.requestStream = s

	if err := req.failure(); err != nil {
		s.destroy(err)

		return s, nil
	}

	r.incomingRequests[req.ex.id] = req

	s.open()

	return s, nil
}

func (req *Request) failure() error {
	if req.rpc.closed {
		return req.rpc.err
	}

	return req.ex.reason
}

func (req *Request) abort(reason error) {
	if req.ex.reason != nil {
		return
	}

	req.ex.reason = reason

	req.cancel(reason)
}

// An Event is an event from the remote end, as passed to a Handler. It gets
// no reply.
type Event struct {
	// Command is the command of the event.
	Command uint
	// Data is the data that the event was sent with.
	Data []byte

	cancel context.CancelCauseFunc
}

func (event *Event) abort(reason error) {
	event.cancel(reason)
}
