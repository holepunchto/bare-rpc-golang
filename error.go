package barerpc

import (
	"context"
	"errors"
)

// The codes of the errors that the RPC itself raises, which are the same as
// those of bare-rpc.
const (
	// CodeUnknownMessage is the code of a message of an unknown type.
	CodeUnknownMessage = "UNKNOWN_MESSAGE"
	// CodeUnknownCommand is the code of a request for a command that has no
	// handler.
	CodeUnknownCommand = "UNKNOWN_COMMAND"
	// CodeInvalidMessage is the code of a message that is malformed or that no
	// valid peer sends.
	CodeInvalidMessage = "INVALID_MESSAGE"
	// CodeFrameTooLarge is the code of a frame larger than MaxFrameSize.
	CodeFrameTooLarge = "FRAME_TOO_LARGE"
	// CodeAlreadySent is the code of a request or response sent twice.
	CodeAlreadySent = "ALREADY_SENT"
	// CodeAlreadyReceived is the code of a request or response received twice.
	CodeAlreadyReceived = "ALREADY_RECEIVED"
	// CodeChannelClosed is the code of a channel that closed while requests
	// were still in flight.
	CodeChannelClosed = "CHANNEL_CLOSED"
	// CodeUnexpectedStream is the code of a request answered with a stream
	// that it didn't create a response stream for.
	CodeUnexpectedStream = "UNEXPECTED_STREAM"
	// CodeStreamOverflow is the code of a stream that buffered more than
	// MaxBufferedBytes.
	CodeStreamOverflow = "STREAM_OVERFLOW"
	// CodeCancelled is the code of a request that was cancelled.
	CodeCancelled = "CANCELLED"
	// CodeTimeout is the code of a request whose context deadline passed.
	CodeTimeout = "TIMEOUT"
)

// An Error is an error that crosses the wire, such as a request rejected by
// its handler, or one that the RPC itself raises with one of the Code
// constants.
//
// Errors that are not an *Error cross the wire with only their message.
type Error struct {
	// Message is the message of the error.
	Message string
	// Code is the code of the error, if any.
	Code string
	// Errno is the error number of the error, if any.
	Errno int

	err error
}

// ErrChannelClosed is the error of everything in flight when the channel
// closes cleanly, such as when the remote end closes it or Close is called.
var ErrChannelClosed = &Error{Message: "Channel closed", Code: CodeChannelClosed}

func newError(code, message string) *Error {
	return &Error{Message: message, Code: code}
}

func (e *Error) Error() string {
	return e.Message
}

// Unwrap returns the error that caused this one, such as the context error of
// a CANCELLED or TIMEOUT request.
func (e *Error) Unwrap() error {
	return e.err
}

func toError(err error) *Error {
	var e *Error

	if errors.As(err, &e) {
		return e
	}

	return &Error{Message: err.Error()}
}

func cancelError(reason error) *Error {
	e := toError(reason)

	if e.Code != "" {
		return e
	}

	code := CodeCancelled

	if errors.Is(reason, context.DeadlineExceeded) {
		code = CodeTimeout
	}

	return &Error{Message: e.Message, Code: code, Errno: e.Errno, err: reason}
}

// The reason a request is cancelled when its context is done.
func contextError(ctx context.Context) *Error {
	cause := context.Cause(ctx)

	if errors.Is(cause, context.DeadlineExceeded) {
		return &Error{Message: "Request timed out", Code: CodeTimeout, err: cause}
	}

	return &Error{Message: "Request was cancelled", Code: CodeCancelled, err: cause}
}
