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
- ✅ Wire-compatible with JavaScript bare-rpc
- ✅ Streaming support (request, response, and bidirectional) with flow control

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
    
    // Handle incoming requests
    rpc.Listen(func(req *bare_rpc.Request) {
        switch req.Command {
        case 0:
            req.Reply([]byte("Hello from Go!"))
        }
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

// Listen for incoming messages
err := rpc.Listen(onRequest func(req *Request)) error

// Build a request that can attach streams
req := rpc.NewRequest(command uint) *OutgoingRequest
req.Send(data []byte) error                       // unary payload
data, err := req.Reply() ([]byte, error)          // await unary response
ws := req.CreateRequestStream() *OutgoingStream   // io.WriteCloser (-> handler)
rs := req.CreateResponseStream() *IncomingStream  // io.ReadCloser  (<- handler)
```

On the handler side, the `*Request` passed to `Listen` exposes the mirror
methods: `CreateRequestStream() *IncomingStream` (read what the initiator sends)
and `CreateResponseStream() *OutgoingStream` (write the response stream).

### Listen

```go
err := rpc.Listen(func(req *Request) {
	// Reply to this request
	err := req.Reply([]byte("Hello javascript!"))
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
rpc.Listen(func(req *bare_rpc.Request) {
    switch req.Command {
    case 1: // respond with a stream
        out := req.CreateResponseStream() // io.WriteCloser
        out.Write([]byte("chunk"))
        out.Close()
    case 2: // read a request stream, then reply
        in := req.CreateRequestStream() // io.ReadCloser
        data, _ := io.ReadAll(in)
        req.Reply(data)
    }
})
```

Cross-runtime streaming is exercised against the JavaScript reference in
`interop_test.go` (run with `go test -tags interop ./...`, requires `bare`).

## Transport

This library works with any `io.ReadWriter`, including:

- TCP connections (`net.Conn`)
- Unix domain sockets
- Named pipes
- In-memory buffers
- Custom transports

## Running the Example

The example demonstrates Go ↔ JavaScript RPC over Unix sockets:

```bash
cd example && npm i && go run .
```

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
