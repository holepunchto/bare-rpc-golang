package barerpc

import (
	"context"
	"fmt"
)

// A Handler handles the requests and events of the remote end, each on a
// goroutine of its own. The context of a request is cancelled with the
// reason if the remote end cancels it or the channel ends, and the context of
// both is cancelled once the handler returns.
type Handler interface {
	// HandleRequest handles a request, which it answers with Request.Reply or
	// Request.CreateResponseStream. An error it returns answers the request
	// unless it has been answered, and destroys its response stream if it
	// has one.
	HandleRequest(ctx context.Context, req *Request) error
	// HandleEvent handles an event. An error it returns ends the channel.
	HandleEvent(ctx context.Context, event *Event) error
}

// A RequestFunc handles a request for a Router, returning the data to reply
// with unless it answered the request itself.
type RequestFunc func(ctx context.Context, req *Request) ([]byte, error)

// An EventFunc handles an event for a Router.
type EventFunc func(ctx context.Context, event *Event) error

// A Router is a Handler that routes requests and events by their command. It
// rejects a request for a command that has no function with an
// UNKNOWN_COMMAND *Error, and drops such an event. Functions must be set
// before the router is used.
type Router struct {
	requests map[uint]RequestFunc
	events   map[uint]EventFunc
}

// OnRequest sets the function that handles requests for command.
func (rt *Router) OnRequest(command uint, fn RequestFunc) {
	if rt.requests == nil {
		rt.requests = make(map[uint]RequestFunc)
	}

	rt.requests[command] = fn
}

// OnEvent sets the function that handles events for command.
func (rt *Router) OnEvent(command uint, fn EventFunc) {
	if rt.events == nil {
		rt.events = make(map[uint]EventFunc)
	}

	rt.events[command] = fn
}

// HandleRequest implements Handler.
func (rt *Router) HandleRequest(ctx context.Context, req *Request) error {
	fn, ok := rt.requests[req.Command]

	if !ok {
		return newError(CodeUnknownCommand, fmt.Sprintf("Unknown command %d", req.Command))
	}

	data, err := fn(ctx, req)
	if err != nil {
		return err
	}

	if req.Sent() || ctx.Err() != nil {
		return nil
	}

	return req.Reply(data)
}

// HandleEvent implements Handler.
func (rt *Router) HandleEvent(ctx context.Context, event *Event) error {
	if fn, ok := rt.events[event.Command]; ok {
		return fn(ctx, event)
	}

	return nil
}
