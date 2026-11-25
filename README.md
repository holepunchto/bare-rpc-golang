# bare-rpc-golang

RPC over any byte transport for Go. The Go counterpart of [bare-rpc](https://github.com/holepunchto/bare-rpc), and wire compatible with it. Built on [compact-encoding-golang](https://github.com/holepunchto/compact-encoding-golang).

```
go get github.com/holepunchto/bare-rpc-golang/v2
```

## Usage

This example wires two peers together in-process and makes one request:

```go
package main

import (
	"context"
	"fmt"
	"net"

	barerpc "github.com/holepunchto/bare-rpc-golang/v2"
)

func main() {
	a, b := net.Pipe()

	var router barerpc.Router

	router.OnRequest(1, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
		return []byte("pong"), nil
	})

	server := barerpc.New(b, &barerpc.Options{Handler: &router})
	client := barerpc.New(a, nil)

	reply, err := client.Request(context.Background(), 1, []byte("ping"))
	if err != nil {
		panic(err)
	}

	fmt.Println(string(reply)) // pong

	client.Close()
	server.Close()
}
```

`New()` starts a session over any `io.ReadWriteCloser`, such as a `net.Conn`, a `Sidecar` from [bare-sidecar-golang](https://github.com/holepunchto/bare-sidecar-golang), or a `Worklet` from [bare-kit-golang](https://github.com/holepunchto/bare-kit-golang). The session reads the transport on a goroutine of its own and writes one frame at a time, in order, on another.

## Command routing

A `Router` sends each request and event to the function set for its command. Pass it to `New()` as `Options.Handler`:

```go
var router barerpc.Router

router.OnRequest(1, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
	return []byte("pong"), nil
})

router.OnEvent(2, func(ctx context.Context, event *barerpc.Event) error {
	fmt.Println(event.Command)

	return nil
})

rpc := barerpc.New(transport, &barerpc.Options{Handler: &router})
```

A request function returns the data to reply with, unless it has answered the request itself. The router rejects requests for commands without a function with the code `UNKNOWN_COMMAND`, and drops such events. Without a handler, every request and event is treated so.

`Router` is a `Handler`. You can write your own `Handler` instead, with a `HandleRequest()` method for requests and a `HandleEvent()` method for events.

## Requests and responses

`Request()` sends a request and waits for its reply. `Event()` sends an event, which gets no reply. Each request and event of the peer is handled on a goroutine of its own, so a slow handler does not hold back the others.

A request is answered once, with `Request.Reply()` or `Request.CreateResponseStream()`. Answering again fails with an `*Error` with the code `ALREADY_SENT`, and `Request.Sent()` tells whether the request has been answered.

If a handler returns an error before it answers, the request is rejected with the error. An `*Error` is sent as is. Any other error is sent with its message, an empty code, and errno 0. If the handler had created a response stream, the stream is destroyed with the error instead.

When the peer rejects a request, `Request()` fails with an `*Error` with the `Message`, `Code`, and `Errno` of the peer.

## Streaming requests and responses

`NewRequest()` creates a request without sending it, which can then stream its body, its reply, or both:

- `OutgoingRequest.Send()` sends the request with data, and `OutgoingRequest.CreateRequestStream()` sends it as a stream instead.
- `OutgoingRequest.Reply()` waits for the reply, and `OutgoingRequest.CreateResponseStream()` reads it as a stream instead.

On the answering side, `Request.CreateRequestStream()` reads the body of a request sent as a stream, and `Request.CreateResponseStream()` answers it with a stream. This handler echoes the body back:

```go
router.OnRequest(1, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
	body, err := req.CreateRequestStream()
	if err != nil {
		return nil, err
	}

	reply, err := req.CreateResponseStream()
	if err != nil {
		return nil, err
	}

	if _, err := io.Copy(reply, body); err != nil {
		return nil, err
	}

	return nil, reply.Close()
})

req := client.NewRequest(context.Background(), 1)

incoming, _ := req.CreateResponseStream()
outgoing, _ := req.CreateRequestStream()

go func() {
	outgoing.Write([]byte("ping"))
	outgoing.Close()
}()

data, _ := io.ReadAll(incoming)

fmt.Println(string(data)) // ping
```

An `OutgoingStream` is an `io.WriteCloser` and an `IncomingStream` is an `io.ReadCloser`. A stream only carries data once both ends have opened it, so `Write()` and `Close()` wait until the peer has opened its end, and `Write()` then waits for the data to be written to the transport. If the peer answers before it opens the body of a request, the body is destroyed. Writes after `Close()` or `Destroy()` fail with `io.ErrClosedPipe`, or with the error that the stream was destroyed with.

`Read()` reads the bytes of a stream without regard for how they were written. `IncomingStream.ReadChunk()` instead returns each write of the peer whole, which suits a peer that writes one encoded message per write.

An `IncomingStream` asks the peer to pause once it buffers 16 KiB, and to resume once less than that is buffered. While the peer has paused an `OutgoingStream`, `Write()` waits. If the peer keeps sending anyway, an incoming stream holds at most `Options.MaxBufferedBytes`, 16 MiB by default, before the session fails with the code `STREAM_OVERFLOW`.

`Destroy()` stops a stream early, telling the peer that it failed with the given error, if not nil. `Close()` on an `IncomingStream` is `Destroy(nil)`.

## Cancellation

A request is cancelled when the context passed to `Request()` or `NewRequest()` is done before the request has been answered. The request then fails with an `*Error` with the code `CANCELLED`, or `TIMEOUT` if the deadline of the context passed, which wraps the context error. Its streams are destroyed with that error, and the peer is told to stop handling the request. A context that is already done sends no request at all:

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

reply, err := rpc.Request(ctx, 1, []byte("ping"))

if errors.Is(err, context.DeadlineExceeded) {
	fmt.Println("timed out")
}
```

`OutgoingRequest.Cancel()` cancels a request directly. Closing the response stream of a request before the reply has ended cancels it as well. Once the request has been answered, cancelling does nothing.

On the answering side, a cancel of the peer cancels the context of the handler, with an `*Error` holding the `Message`, `Code`, and `Errno` that the peer cancelled with as its cause. The streams of the request are destroyed with that error, and answering the request sends nothing:

```go
router.OnRequest(1, func(ctx context.Context, req *barerpc.Request) ([]byte, error) {
	return search(ctx, req.Data) // Fails once the peer cancels
})
```

The context of a handler is also cancelled when the session ends, and once the handler returns.

Cancelling a request needs a peer that supports it. A peer without support fails the session with the code `UNKNOWN_MESSAGE` when it receives a cancel.

## Termination

The session ends with `ErrChannelClosed`, an `*Error` with the code `CHANNEL_CLOSED`, when you call `Close()` or the transport ends. It fails with an error instead when:

- The transport ends partway through a frame, with the code `INVALID_MESSAGE`.
- Reading from or writing to the transport fails, with that error.
- An event handler returns an error, with that error.
- The peer sends a bad frame, as described under [Untrusted peers](#untrusted-peers).

Pending requests then fail with the error, and so do later requests and events. Open streams are destroyed with it, and the contexts of running handlers are cancelled with it as their cause.

The session then stops reading and closes the transport. If it ended with `ErrChannelClosed`, it first writes the frames already queued. Otherwise it drops them.

`Close()` waits for the session to end and returns the error of closing the transport. `Wait()` waits for the session to end. It returns nil if the session ended with `ErrChannelClosed`, and the error otherwise:

```go
if err := rpc.Wait(); err != nil {
	fmt.Println("The session failed:", err)
}
```

`Idle()` tells whether no requests or streams are in flight, so that the transport can be closed without failing any.

## Untrusted peers

`RPC` does not trust the peer to send valid frames. If a frame is longer than `Options.MaxFrameSize`, cannot be decoded, or breaks the protocol, the session fails with an `*Error`. The code tells you why:

- `FRAME_TOO_LARGE`: The frame, with its length prefix, is longer than `Options.MaxFrameSize`, 16 MiB by default. This is checked as soon as the length prefix arrives, before the rest of the frame is read.
- `UNKNOWN_MESSAGE`: The frame has a message type that `RPC` does not know.
- `INVALID_MESSAGE`: The frame cannot be decoded, the transport ends partway through a frame, or the frame is one that a valid peer never sends. Examples are a response, stream, or cancel message with an id of 0, a stream message with an invalid set of flags, and a new request that reuses the id of a request that is still in flight.
- `STREAM_OVERFLOW`: An incoming stream holds more than `Options.MaxBufferedBytes` bytes that have not been read. This only happens when the peer keeps sending after it was asked to pause.

A frame that refers to a request or stream that has already finished, or that was never known, is ignored. The same goes for frames that arrive for a stream that has already ended or been destroyed.

If you read the reply as a stream but the peer replies without one, the reply arrives on the stream as a single chunk, unless it is empty, and the stream then ends. An error reply destroys the stream with the error.

If you expect a plain reply but the peer replies with a stream, the request fails with the code `UNEXPECTED_STREAM`. The session destroys the stream toward the peer and stays open.

## License

Apache-2.0
