package bare_rpc

import "context"

// OutgoingRequest is the initiator side of a request: a unary Send/Reply and/or a stream in either direction.
type OutgoingRequest struct {
	rpc     *RPC
	id      uint
	command uint
	respCh  chan *Message
}

// NewRequest reserves a request id; nothing is written until Send or a CreateStream method.
func (r *RPC) NewRequest(command uint) *OutgoingRequest {
	r.mu.Lock()
	r.id++
	id := r.id
	ch := make(chan *Message, 1)
	if r.closed {
		close(ch)
	} else {
		r.pending[id] = ch
	}
	r.mu.Unlock()

	return &OutgoingRequest{rpc: r, id: id, command: command, respCh: ch}
}

// ID returns the request id.
func (req *OutgoingRequest) ID() uint { return req.id }

// Send writes the request frame with its unary payload.
func (req *OutgoingRequest) Send(data []byte) error {
	return req.rpc.Send(&Message{Type: TypeRequest, ID: req.id, Command: req.command, Data: data})
}

// Reply waits for the unary response, the remote error, or the teardown cause.
func (req *OutgoingRequest) Reply() ([]byte, error) {
	return req.ReplyContext(context.Background())
}

// ReplyContext is Reply that gives up with ctx.Err() when ctx is done; a late response is dropped.
func (req *OutgoingRequest) ReplyContext(ctx context.Context) ([]byte, error) {
	select {
	case resp, ok := <-req.respCh:
		if !ok {
			return nil, req.rpc.noReplyError()
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Data, nil
	case <-ctx.Done():
		req.rpc.mu.Lock()
		delete(req.rpc.pending, req.id)
		req.rpc.mu.Unlock()
		return nil, ctx.Err()
	}
}

// CreateRequestStream opens the writable request stream (initiator -> handler).
func (req *OutgoingRequest) CreateRequestStream() *OutgoingStream {
	s := newOutgoingStream(req.rpc, req.id, req.command, TypeRequest, StreamRequest)
	req.rpc.openOutgoing(s)
	return s
}

// CreateResponseStream opens the readable response stream (handler -> initiator).
func (req *OutgoingRequest) CreateResponseStream() *IncomingStream {
	s := newIncomingStream(req.rpc, req.id, StreamResponse)
	req.rpc.openIncoming(s)
	return s
}
