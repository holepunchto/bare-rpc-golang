package bare_rpc

import (
	"errors"
	"fmt"

	c "github.com/holepunchto/compact-encoding-golang"
)

// Message types
const (
	TypeRequest  = 1
	TypeResponse = 2
	TypeStream   = 3
)

// Stream flags
const (
	StreamOpen     = 0x001
	StreamClose    = 0x002
	StreamPause    = 0x004
	StreamResume   = 0x008
	StreamData     = 0x010
	StreamEnd      = 0x020
	StreamDestroy  = 0x040
	StreamError    = 0x080
	StreamRequest  = 0x100
	StreamResponse = 0x200
)

// RPCError represents an error sent over RPC
type RPCError struct {
	Message string
	Code    string
	Errno   int
}

func (e *RPCError) Error() string {
	return e.Message
}

// RPCErrorCodec encodes/decodes RPCError
type RPCErrorCodec struct{}

func NewRPCErrorCodec() *RPCErrorCodec {
	return &RPCErrorCodec{}
}

func (ec *RPCErrorCodec) Preencode(state *c.State, e *RPCError) {
	c.NewString().Preencode(state, e.Message)
	c.NewString().Preencode(state, e.Code)
	c.NewInt().Preencode(state, e.Errno)
}

func (ec *RPCErrorCodec) Encode(state *c.State, e *RPCError) error {
	var err error
	if err = c.NewString().Encode(state, e.Message); err != nil {
		return err
	}
	if err = c.NewString().Encode(state, e.Code); err != nil {
		return err
	}
	if err = c.NewInt().Encode(state, e.Errno); err != nil {
		return err
	}
	return nil
}

func (ec *RPCErrorCodec) Decode(state *c.State) (*RPCError, error) {
	var err error
	e := &RPCError{}
	if e.Message, err = c.NewString().Decode(state); err != nil {
		return nil, err
	}
	if e.Code, err = c.NewString().Decode(state); err != nil {
		return nil, err
	}
	if e.Errno, err = c.NewInt().Decode(state); err != nil {
		return nil, err
	}
	return e, nil
}

// Message represents an RPC message
type Message struct {
	Type    uint
	ID      uint
	Command uint
	Stream  uint
	Error   *RPCError
	Data    []byte
}

// MessageCodec encodes/decodes Message with frame header
type MessageCodec struct{}

func NewMessageCodec() *MessageCodec {
	return &MessageCodec{}
}

func (mc *MessageCodec) Preencode(state *c.State, m *Message) {
	// Frame length (fixed 4 bytes LE uint32)
	state.End += 4

	c.NewUint().Preencode(state, m.Type)
	c.NewUint().Preencode(state, m.ID)

	switch m.Type {
	case TypeRequest:
		c.NewUint().Preencode(state, m.Command)
		c.NewUint().Preencode(state, m.Stream)
		if m.Stream == 0 {
			c.NewBuffer().Preencode(state, m.Data)
		}

	case TypeResponse:
		c.NewBool().Preencode(state, true)
		c.NewUint().Preencode(state, m.Stream)
		if m.Error != nil {
			NewRPCErrorCodec().Preencode(state, m.Error)
		} else if m.Stream == 0 {
			c.NewBuffer().Preencode(state, m.Data)
		}

	case TypeStream:
		c.NewUint().Preencode(state, m.Stream)
		if m.Stream&StreamError != 0 {
			NewRPCErrorCodec().Preencode(state, m.Error)
		} else if m.Stream&StreamData != 0 {
			c.NewBuffer().Preencode(state, m.Data)
		}
	}
}

func (mc *MessageCodec) Encode(state *c.State, m *Message) error {
	var err error

	// Remember where frame length goes
	frameStart := state.Start
	state.Start += 4

	// Remember where payload starts
	payloadStart := state.Start

	if err = c.NewUint().Encode(state, m.Type); err != nil {
		return err
	}
	if err = c.NewUint().Encode(state, m.ID); err != nil {
		return err
	}

	switch m.Type {
	case TypeRequest:
		if err = c.NewUint().Encode(state, m.Command); err != nil {
			return err
		}
		if err = c.NewUint().Encode(state, m.Stream); err != nil {
			return err
		}
		if m.Stream == 0 {
			if err = c.NewBuffer().Encode(state, m.Data); err != nil {
				return err
			}
		}

	case TypeResponse:
		if err = c.NewBool().Encode(state, m.Error != nil); err != nil {
			return err
		}
		if err = c.NewUint().Encode(state, m.Stream); err != nil {
			return err
		}
		if m.Error != nil {
			if err = NewRPCErrorCodec().Encode(state, m.Error); err != nil {
				return err
			}
		} else if m.Stream == 0 {
			if err = c.NewBuffer().Encode(state, m.Data); err != nil {
				return err
			}
		}

	case TypeStream:
		if err = c.NewUint().Encode(state, m.Stream); err != nil {
			return err
		}
		if m.Stream&StreamError != 0 {
			if err = NewRPCErrorCodec().Encode(state, m.Error); err != nil {
				return err
			}
		} else if m.Stream&StreamData != 0 {
			if err = c.NewBuffer().Encode(state, m.Data); err != nil {
				return err
			}
		}
	}

	// Write frame length at the start (LE uint32)
	frameLen := state.Start - payloadStart
	state.Buffer[frameStart] = byte(frameLen)
	state.Buffer[frameStart+1] = byte(frameLen >> 8)
	state.Buffer[frameStart+2] = byte(frameLen >> 16)
	state.Buffer[frameStart+3] = byte(frameLen >> 24)

	return nil
}

func (mc *MessageCodec) Decode(state *c.State) (*Message, error) {
	var err error

	// Read frame length (LE uint32)
	if state.Start+4 > state.End {
		return nil, errors.Join(errors.New("failed to decode message"), c.NewEncodingErrorOutOfBounds())
	}
	frameLen := uint(state.Buffer[state.Start]) |
		uint(state.Buffer[state.Start+1])<<8 |
		uint(state.Buffer[state.Start+2])<<16 |
		uint(state.Buffer[state.Start+3])<<24
	state.Start += 4

	if state.Start+frameLen > state.End {
		return nil, c.NewEncodingErrorOutOfBounds()
	}

	m := &Message{}

	if m.Type, err = c.NewUint().Decode(state); err != nil {
		return nil, err
	}
	if m.ID, err = c.NewUint().Decode(state); err != nil {
		return nil, err
	}

	switch m.Type {
	case TypeRequest:
		if m.Command, err = c.NewUint().Decode(state); err != nil {
			return nil, err
		}
		if m.Stream, err = c.NewUint().Decode(state); err != nil {
			return nil, err
		}
		if m.Stream == 0 {
			if m.Data, err = c.NewBuffer().Decode(state); err != nil {
				return nil, err
			}
		}

	case TypeResponse:

		var hasError bool
		if hasError, err = c.NewBool().Decode(state); err != nil {
			return nil, err
		}
		if m.Stream, err = c.NewUint().Decode(state); err != nil {
			return nil, err
		}
		if hasError {
			if m.Error, err = NewRPCErrorCodec().Decode(state); err != nil {
				return nil, err
			}
		} else if m.Stream == 0 {
			state.Start += 1 //? WHY?
			if m.Data, err = c.NewBuffer().Decode(state); err != nil {
				return nil, err
			}
		}

	case TypeStream:
		if m.Stream, err = c.NewUint().Decode(state); err != nil {
			return nil, err
		}
		if m.Stream&StreamError != 0 {
			if m.Error, err = NewRPCErrorCodec().Decode(state); err != nil {
				return nil, err
			}
		} else if m.Stream&StreamData != 0 {
			state.Start += 1 //? WHY?
			if m.Data, err = c.NewBuffer().Decode(state); err != nil {
				return nil, err
			}
		}

	default:
		return nil, fmt.Errorf("unknown message type: %d", m.Type)
	}

	return m, nil
}
