package barerpc

import (
	"encoding/binary"
	"fmt"

	c "github.com/holepunchto/compact-encoding-golang/v2"
)

const (
	typeRequest  = 1
	typeResponse = 2
	typeStream   = 3
	typeCancel   = 4
)

const (
	streamOpen     = 0x1
	streamClose    = 0x2
	streamPause    = 0x4
	streamResume   = 0x8
	streamData     = 0x10
	streamEnd      = 0x20
	streamDestroy  = 0x40
	streamError    = 0x80
	streamRequest  = 0x100
	streamResponse = 0x200
)

type message struct {
	typ     uint
	id      uint
	command uint
	stream  uint
	err     *Error
	data    []byte
}

func (m *message) hasData() bool {
	switch m.typ {
	case typeRequest:
		return m.stream == 0
	case typeResponse:
		return m.err == nil && m.stream == 0
	case typeStream:
		return m.stream&streamError == 0 && m.stream&streamData != 0
	default:
		return false
	}
}

type errorCodec struct{}

func (errorCodec) Preencode(state *c.State, e *Error) {
	c.String.Preencode(state, e.Message)
	c.String.Preencode(state, e.Code)
	c.Int.Preencode(state, e.Errno)
}

func (errorCodec) Encode(state *c.State, e *Error) error {
	if err := c.String.Encode(state, e.Message); err != nil {
		return err
	}

	if err := c.String.Encode(state, e.Code); err != nil {
		return err
	}

	return c.Int.Encode(state, e.Errno)
}

func (errorCodec) Decode(state *c.State) (*Error, error) {
	var (
		e   Error
		err error
	)

	if e.Message, err = c.String.Decode(state); err != nil {
		return nil, err
	}

	if e.Code, err = c.String.Decode(state); err != nil {
		return nil, err
	}

	if e.Errno, err = c.Int.Decode(state); err != nil {
		return nil, err
	}

	return &e, nil
}

// The body of a message, after the 4-byte length of its frame.
type bodyCodec struct{}

func (bodyCodec) Preencode(state *c.State, m *message) {
	c.Uint.Preencode(state, m.typ)
	c.Uint.Preencode(state, m.id)

	switch m.typ {
	case typeRequest:
		c.Uint.Preencode(state, m.command)
		c.Uint.Preencode(state, m.stream)
	case typeResponse:
		c.Bool.Preencode(state, m.err != nil)
		c.Uint.Preencode(state, m.stream)

		if m.err != nil {
			errorCodec{}.Preencode(state, m.err)
		}
	case typeStream:
		c.Uint.Preencode(state, m.stream)

		if m.stream&streamError != 0 {
			errorCodec{}.Preencode(state, m.err)
		}
	case typeCancel:
		errorCodec{}.Preencode(state, m.err)
	}

	if m.hasData() {
		c.Buffer.Preencode(state, m.data)
	}
}

func (bodyCodec) Encode(state *c.State, m *message) error {
	if err := c.Uint.Encode(state, m.typ); err != nil {
		return err
	}

	if err := c.Uint.Encode(state, m.id); err != nil {
		return err
	}

	var err error

	switch m.typ {
	case typeRequest:
		if err = c.Uint.Encode(state, m.command); err == nil {
			err = c.Uint.Encode(state, m.stream)
		}
	case typeResponse:
		if err = c.Bool.Encode(state, m.err != nil); err == nil {
			err = c.Uint.Encode(state, m.stream)
		}

		if err == nil && m.err != nil {
			err = errorCodec{}.Encode(state, m.err)
		}
	case typeStream:
		err = c.Uint.Encode(state, m.stream)

		if err == nil && m.stream&streamError != 0 {
			err = errorCodec{}.Encode(state, m.err)
		}
	case typeCancel:
		err = errorCodec{}.Encode(state, m.err)
	}

	if err == nil && m.hasData() {
		err = c.Buffer.Encode(state, m.data)
	}

	return err
}

func (bodyCodec) Decode(state *c.State) (*message, error) {
	var (
		m   message
		err error
	)

	if m.typ, err = c.Uint.Decode(state); err != nil {
		return nil, err
	}

	if m.id, err = c.Uint.Decode(state); err != nil {
		return nil, err
	}

	switch m.typ {
	case typeRequest:
		if m.command, err = c.Uint.Decode(state); err != nil {
			return nil, err
		}

		if m.stream, err = c.Uint.Decode(state); err != nil {
			return nil, err
		}
	case typeResponse:
		failed, err := c.Bool.Decode(state)
		if err != nil {
			return nil, err
		}

		if m.stream, err = c.Uint.Decode(state); err != nil {
			return nil, err
		}

		if failed {
			if m.err, err = (errorCodec{}).Decode(state); err != nil {
				return nil, err
			}
		}
	case typeStream:
		if m.stream, err = c.Uint.Decode(state); err != nil {
			return nil, err
		}

		if m.stream&streamError != 0 {
			if m.err, err = (errorCodec{}).Decode(state); err != nil {
				return nil, err
			}
		}
	case typeCancel:
		if m.err, err = (errorCodec{}).Decode(state); err != nil {
			return nil, err
		}
	default:
		return nil, newError(CodeUnknownMessage, fmt.Sprintf("Unknown message '%d'", m.typ))
	}

	if m.hasData() {
		if m.data, err = c.Buffer.Decode(state); err != nil {
			return nil, err
		}
	}

	return &m, nil
}

func encodeFrame(m *message) []byte {
	state := c.State{End: 4}

	bodyCodec{}.Preencode(&state, m)

	state.Allocate()
	state.Start = 4

	// The buffer is sized by preencoding, so encoding can't run out of it.
	_ = bodyCodec{}.Encode(&state, m)

	binary.LittleEndian.PutUint32(state.Buffer, uint32(state.End-4))

	return state.Buffer
}

func decodeBody(body []byte) (*message, error) {
	m, err := c.Decode(bodyCodec{}, body)
	if err != nil {
		if e, ok := err.(*Error); ok {
			return nil, e
		}

		return nil, newError(CodeInvalidMessage, "Malformed message: "+err.Error())
	}

	if violation := protocolViolation(m); violation != "" {
		return nil, newError(CodeInvalidMessage, violation)
	}

	return m, nil
}

var streamFlags = func() map[uint]bool {
	flags := make(map[uint]bool)

	for _, direction := range []uint{streamRequest, streamResponse} {
		for _, action := range []uint{
			streamOpen,
			streamClose,
			streamClose | streamError,
			streamPause,
			streamResume,
			streamData,
			streamEnd,
			streamDestroy,
			streamDestroy | streamError,
		} {
			flags[direction|action] = true
		}
	}

	return flags
}()

// Returns why a message that decodes cleanly is one that no valid peer sends,
// or "" if it isn't.
func protocolViolation(m *message) string {
	switch m.typ {
	case typeRequest:
		if m.stream == 0 {
			return ""
		}

		if m.stream != streamOpen {
			return fmt.Sprintf("Invalid request stream flags '%d'", m.stream)
		}

		if m.id == 0 {
			return "Stream request without an id"
		}
	case typeResponse:
		if m.id == 0 {
			return "Response without an id"
		}

		if m.stream != 0 && (m.stream != streamOpen || m.err != nil) {
			return fmt.Sprintf("Invalid response stream flags '%d'", m.stream)
		}
	case typeStream:
		if m.id == 0 {
			return "Stream message without an id"
		}

		if !streamFlags[m.stream] {
			return fmt.Sprintf("Invalid stream flags '%d'", m.stream)
		}
	case typeCancel:
		if m.id == 0 {
			return "Cancel without an id"
		}
	}

	return ""
}
