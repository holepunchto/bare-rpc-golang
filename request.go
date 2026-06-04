package bare_rpc

// OutgoingRequest is the initiator side of a request. Beyond a plain unary
// Send/Reply it can open a request stream (bytes flowing initiator -> handler)
// and/or a response stream (bytes flowing handler -> initiator).
type OutgoingRequest struct {
	rpc     *RPC
	id      uint
	command uint
	respCh  chan *Message

	requestStream  *OutgoingStream
	responseStream *IncomingStream
}

// NewRequest reserves a request id and returns a builder. Nothing is written
// until Send or one of the CreateStream methods is called.
func (r *RPC) NewRequest(command uint) *OutgoingRequest {
	r.mu.Lock()
	r.id++
	id := r.id
	ch := make(chan *Message, 1)
	r.pending[id] = ch
	r.mu.Unlock()

	return &OutgoingRequest{rpc: r, id: id, command: command, respCh: ch}
}

// ID returns the request id.
func (req *OutgoingRequest) ID() uint { return req.id }

// Send writes the request frame carrying the initial (unary) payload.
func (req *OutgoingRequest) Send(data []byte) error {
	return req.rpc.Send(&Message{
		Type:    TypeRequest,
		ID:      req.id,
		Command: req.command,
		Stream:  0,
		Data:    data,
	})
}

// Reply blocks until the unary response arrives.
func (req *OutgoingRequest) Reply() ([]byte, error) {
	resp := <-req.respCh

	req.rpc.mu.Lock()
	delete(req.rpc.pending, req.id)
	req.rpc.mu.Unlock()

	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Data, nil
}

// CreateRequestStream opens the writable request stream (initiator -> handler).
func (req *OutgoingRequest) CreateRequestStream() *OutgoingStream {
	s := newOutgoingStream(req.rpc, req.id, req.command, TypeRequest, StreamRequest)
	req.requestStream = s

	acked := req.rpc.registerOutgoing(req.id, StreamRequest, s)
	s.open()
	if acked {
		s.continueOpen()
	}
	return s
}

// CreateResponseStream opens the readable response stream (handler -> initiator).
func (req *OutgoingRequest) CreateResponseStream() *IncomingStream {
	s := newIncomingStream(req.rpc, req.id, StreamResponse)
	req.responseStream = s

	req.rpc.mu.Lock()
	req.rpc.incomingResponses[req.id] = s
	req.rpc.mu.Unlock()

	s.open()
	return s
}
