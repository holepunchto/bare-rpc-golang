# bare-rpc (Go)

> 🚧 **Work in Progress** - A Go implementation of [bare-rpc](https://github.com/holepunchto/bare-rpc)

Wire-compatible RPC protocol implementation for Go, designed to interoperate with Bare/JavaScript implementations.

## Overview

This library provides a simple RPC protocol built on top of [compact-encoding](https://github.com/holepunchto/compact-encoding-golang). It supports request/response patterns, one-way events, and streaming over any `io.ReadWriter` transport (TCP, Unix sockets, pipes, etc.).

## Installation

```bash
go get github.com/holepunchto/bare-rpc-golang
```

## Features

- ✅ Request/response RPC
- ✅ One-way events
- ✅ Error handling with structured errors
- ✅ Frame-based wire protocol
- ✅ Wire-compatible with JavaScript bare-rpc (tracked against 1.3.8, verified with the [hrpc-test](https://github.com/holepunchto/hrpc-test) vectors)
- ✅ Streaming support (request, response, and bidirectional) with flow control
- ✅ Channel teardown: when the transport ends or fails, pending replies and open streams fail instead of hanging

## Quick Start

### Go Server

```go
package main

import (
    "log"
    "net"
    
    bare_rpc "github.com/holepunchto/bare-rpc-golang"
)

func main() {
    listener, _ := net.Listen("tcp", ":8080")
    conn, _ := listener.Accept()
    
    rpc := bare_rpc.NewRPC(conn)
    
    // Handle incoming requests. A returned error becomes the error reply.
    rpc.Listen(func(req *bare_rpc.Request) error {
        switch req.Command {
        case 0:
            return req.Reply([]byte("Hello from Go!"))
        }
        return &bare_rpc.RPCError{Message: "unknown command", Code: "UNKNOWN"}
    })
}
```

### Go Client

```go
rpc := bare_rpc.NewRPC(conn)

// Make a request
data, err := rpc.Request(0, []byte("ping"))
if err != nil {
    log.Fatal(err)
}

// Send one-way event
rpc.Event(1, []byte("notification"))
```

## Usage Examples

See `main.go` and `server.js` for a complete working example of Go ↔ JavaScript interoperability.

### Go → JavaScript Example

**Go Client** (`main.go`):
```go
// Custom encoding for structured data
type Item struct {
    title string
    desc  string
}

type ItemEncoding struct{}

func (m *ItemEncoding) Preencode(state *c.State, msg *Item) {
    c.NewString().Preencode(state, msg.title)
    c.NewString().Preencode(state, msg.desc)
}

func (m *ItemEncoding) Encode(state *c.State, msg *Item) error {
    if err := c.NewString().Encode(state, msg.title); err != nil {
        return err
    }
    return c.NewString().Encode(state, msg.desc)
}

func (m *ItemEncoding) Decode(state *c.State) (*Item, error) {
    var err error
    msg := &Item{}
    if msg.title, err = c.NewString().Decode(state); err != nil {
        return nil, err
    }
    if msg.desc, err = c.NewString().Decode(state); err != nil {
        return nil, err
    }
    return msg, nil
}

// Make request to JavaScript server
conn, _ := net.Dial("unix", "/tmp/bare-rpc.sock")
rpc := bare_rpc.NewRPC(conn)

buf, err := rpc.Request(0, []byte{})
items, err := c.Decode(c.NewArray(&ItemEncoding{}), buf)
```

**JavaScript Server** (`server.js`):
```javascript
const RPC = require("bare-rpc");
const c = require("compact-encoding");

const Item = {
  preencode(state, m) {
    c.string.preencode(state, m.title);
    c.string.preencode(state, m.desc);
  },
  encode(state, m) {
    c.string.encode(state, m.title);
    c.string.encode(state, m.desc);
  },
  decode(state) {
    return {
      title: c.string.decode(state),
      desc: c.string.decode(state)
    };
  }
};

const Items = c.array(Item);

const rpc = new RPC(socket, (req) => {
  switch (req.command) {
    case 0:
      req.reply(c.encode(Items, [
        { title: "Item 1", desc: "Description 1" },
        { title: "Item 2", desc: "Description 2" }
      ]));
      break;
  }
});
```

## API Reference

### RPC

```go
// Create new RPC instance
rpc := bare_rpc.NewRPC(stream io.ReadWriter)

// Make a request and wait for response
data, err := rpc.Request(command uint, data []byte) ([]byte, error)

// Send one-way event (request with ID 0)
err := rpc.Event(command uint, data []byte) error

// Send response to a request
err := rpc.Reply(id uint, data []byte) error

// Send error response
err := rpc.ReplyError(id uint, err error) error

// Listen for incoming messages until the transport ends or fails. Everything
// still in flight is then failed: a clean end surfaces to waiters as
// bare_rpc.ErrChannelClosed (code CHANNEL_CLOSED), an error as that error.
// The handler runs on its own goroutine per request. Returning an error sends
// it as the error reply (unless a reply was already started); for an event,
// which cannot be replied to, it fails the channel like bare-rpc does.
err := rpc.Listen(onRequest func(req *Request) error) error

// True when nothing is in flight (no pending reply, no open stream)
idle := rpc.Idle() bool

// Fail everything in flight with ErrChannelClosed and close the transport (if it is an io.Closer)
err := rpc.Close() error

// Largest frame body Receive will accept (default 16 MiB); set before Listen
rpc.MaxFrameSize = 64 << 20

// Build a request that can attach streams
req := rpc.NewRequest(command uint) *OutgoingRequest
req.Send(data []byte) error                       // unary payload
data, err := req.Reply() ([]byte, error)          // await unary response
data, err := req.ReplyContext(ctx) ([]byte, error) // same, giving up when ctx is done
ws := req.CreateRequestStream() *OutgoingStream   // io.WriteCloser (-> handler)
rs := req.CreateResponseStream() *IncomingStream  // io.ReadCloser  (<- handler)
```

On the handler side, the `*Request` passed to `Listen` exposes the mirror
methods: `CreateRequestStream() *IncomingStream` (read what the initiator sends)
and `CreateResponseStream() *OutgoingStream` (write the response stream).

Errors cross the wire as `*RPCError{Message, Code, Errno}`. `ReplyError` and
`Destroy` send an `*RPCError` (even when wrapped) with its code and errno intact;
any other error goes over with just its message. On the receiving side the
error comes back as an `*RPCError`, so `errors.As` recovers the code.

Both stream ends can abort with a reason: `OutgoingStream.Destroy(err)` sends
`CLOSE|ERROR` to the reader, `IncomingStream.Destroy(err)` sends `DESTROY|ERROR`
to the writer. `Close()` on either end is the graceful form.

`IncomingStream.Read` is a plain byte reader. When every `Write` on the other
end is one encoded message (as hrpc does), use `ReadChunk()` to get each DATA
frame back whole instead.

### Listen

```go
err := rpc.Listen(func(req *Request) error {
	if req.IsEvent() { // id 0: one-way, nothing awaits a reply
		return nil
	}
	return req.Reply([]byte("Hello javascript!"))
})
```

## Streaming

A request can carry up to two independent streams, identified by the request id
plus a direction (request: initiator → handler, response: handler → initiator).
The readable end implements `io.ReadCloser`; the writable end implements
`io.WriteCloser`. Flow control (PAUSE/RESUME) is handled transparently — a slow
reader signals the remote writer to back off.

```go
// Client: stream the request body up, await a unary reply
req := rpc.NewRequest(2)
ws := req.CreateRequestStream() // io.WriteCloser
go func() {
    io.Copy(ws, src) // stream bytes to the handler
    ws.Close()       // END + CLOSE
}()
reply, err := req.Reply()

// Client: send a request, stream the response back down
req := rpc.NewRequest(1)
req.Send(nil)
rs := req.CreateResponseStream() // io.ReadCloser
io.Copy(dst, rs)                 // read until EOF
```

```go
// Handler
rpc.Listen(func(req *bare_rpc.Request) error {
    switch req.Command {
    case 1: // respond with a stream
        out := req.CreateResponseStream() // io.WriteCloser
        out.Write([]byte("chunk"))
        return out.Close()
    case 2: // read a request stream, then reply
        in := req.CreateRequestStream() // io.ReadCloser
        data, err := io.ReadAll(in)
        if err != nil {
            return err
        }
        return req.Reply(data)
    }
    return nil
})
```

## Compatibility testing

Two suites pin wire compatibility with the JavaScript reference:

- `vectors_test.go` runs the shared [hrpc-test](https://github.com/holepunchto/hrpc-test)
  conformance vectors vendored under `testdata/hrpc-test/` (see `VERSION` there).
  Every fixture frame must decode to its descriptor and re-encode byte for byte;
  malformed and unknown-type frames must be rejected. To pick up a newer
  hrpc-test release, copy its `fixtures/` over `testdata/hrpc-test/` and update
  `VERSION`.
- `interop_test.go` spawns `bare example/stream-server.js` and streams in both
  directions against the real bare-rpc. Run with
  `go test -tags interop ./...` after `npm i` in `example/` (requires `bare`).

## Transport

This library works with any `io.ReadWriter`, including:

- TCP connections (`net.Conn`)
- Unix domain sockets
- Named pipes
- In-memory buffers
- Custom transports

## Running the Examples

Go ↔ JavaScript RPC over a Unix socket path:

```bash
cd example && npm i && go run .
```

The same over an inherited file descriptor (`example/pipe`): Go creates a Unix
socketpair, passes the child's end through `cmd.ExtraFiles` so it lands as fd 3
in Bare, and `pipe-server.js` opens it with `new Pipe(3)`. One catch: the `bare`
command the npm package installs is a Node shim that respawns the runtime with
only stdio 0-2, so fd 3 never reaches it. The example resolves the native binary
behind the shim (or honours `BARE=/path/to/bare`).

```bash
cd example && go run ./pipe
```

The `ipc` package holds the transport helpers: `ipc.Dial(ctx, network, address)`
retries until a server that is still starting up (a Bare sidecar you just
spawned) accepts, `ipc.Socketpair()` is for a Go parent, and `ipc.Inherited(fd)` for a Go child that Bare spawned with
`bare-subprocess` and `stdio: ['inherit', 'inherit', 'inherit', 'pipe']`, which
puts a duplex pipe on `subprocess.stdio[3]` and fd 3 in the child.

The example creates a TUI application that fetches a list of items from the JavaScript server and displays them using [Bubble Tea](https://github.com/charmbracelet/bubbletea).

## Wire Compatibility

This implementation is designed to be wire-compatible with:
- [bare-rpc](https://github.com/holepunchto/bare-rpc) (JavaScript/Bare)
- Uses [compact-encoding](https://github.com/holepunchto/compact-encoding-golang) for serialization

## License

Apache-2.0

## Related

- [bare-rpc](https://github.com/holepunchto/bare-rpc) - Original JavaScript implementation
- [compact-encoding-golang](https://github.com/holepunchto/compact-encoding-golang) - Compact encoding for Go
