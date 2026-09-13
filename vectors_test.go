package bare_rpc

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	c "github.com/holepunchto/compact-encoding-golang"
)

// Wire conformance against hrpc-test's shared vectors (see testdata/hrpc-test/VERSION).
//
// The JavaScript bare-rpc is the reference the C, Swift and Python ports are held
// to; these fixtures are generated from it. Every frame must decode to the
// committed descriptor and every descriptor must re-encode to the exact frame
// bytes, so any change that moves a byte fails here rather than in interop.

const vectorsDir = "testdata/hrpc-test"

type vectorError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
	Errno   int    `json:"errno"`
}

// vectorDescriptor is the shape messages.json stores. Data is a hex string (""
// for a zero-length payload) or null when the frame carries no dataLen field.
type vectorDescriptor struct {
	Type    uint         `json:"type"`
	ID      uint         `json:"id"`
	Command uint         `json:"command"`
	Stream  uint         `json:"stream"`
	Error   *vectorError `json:"error"`
	Data    *string      `json:"data"`
}

type vectorMessage struct {
	Note       string           `json:"note"`
	Descriptor vectorDescriptor `json:"descriptor"`
}

func readVectorJSON(t *testing.T, v any, elem ...string) {
	t.Helper()
	path := filepath.Join(append([]string{vectorsDir}, elem...)...)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func decodeFrameHex(t *testing.T, frame string) (*Message, error) {
	t.Helper()
	buf, err := hex.DecodeString(frame)
	if err != nil {
		t.Fatal(err)
	}
	state := c.NewState()
	state.Buffer = buf
	state.End = uint(len(buf))
	return NewMessageCodec().Decode(state)
}

func (d vectorDescriptor) message(t *testing.T) *Message {
	t.Helper()
	m := &Message{Type: d.Type, ID: d.ID, Command: d.Command, Stream: d.Stream}
	if d.Error != nil {
		m.Error = &RPCError{Message: d.Error.Message, Code: d.Error.Code, Errno: d.Error.Errno}
	}
	if d.Data != nil {
		data, err := hex.DecodeString(*d.Data)
		if err != nil {
			t.Fatal(err)
		}
		m.Data = data
	}
	return m
}

func assertMessageMatches(t *testing.T, note string, got *Message, want vectorDescriptor) {
	t.Helper()
	if got.Type != want.Type || got.ID != want.ID || got.Stream != want.Stream {
		t.Errorf("%s: header = (type %d, id %d, stream %d), want (type %d, id %d, stream %d)",
			note, got.Type, got.ID, got.Stream, want.Type, want.ID, want.Stream)
	}
	if want.Type == TypeRequest && got.Command != want.Command {
		t.Errorf("%s: command = %d, want %d", note, got.Command, want.Command)
	}

	switch {
	case want.Error == nil && got.Error != nil:
		t.Errorf("%s: unexpected error %+v", note, got.Error)
	case want.Error != nil && got.Error == nil:
		t.Errorf("%s: missing error, want %+v", note, want.Error)
	case want.Error != nil:
		if got.Error.Message != want.Error.Message || got.Error.Code != want.Error.Code || got.Error.Errno != want.Error.Errno {
			t.Errorf("%s: error = %+v, want %+v", note, got.Error, want.Error)
		}
	}

	if want.Data == nil {
		if len(got.Data) != 0 {
			t.Errorf("%s: data = %x, want none", note, got.Data)
		}
	} else if hex.EncodeToString(got.Data) != *want.Data {
		t.Errorf("%s: data = %x, want %s", note, got.Data, *want.Data)
	}
}

func TestVectorsFamilies(t *testing.T) {
	var families []string
	readVectorJSON(t, &families, "families.json")

	for _, family := range families {
		t.Run(family, func(t *testing.T) {
			var frames []string
			var messages []vectorMessage
			readVectorJSON(t, &frames, family, "frames.json")
			readVectorJSON(t, &messages, family, "messages.json")

			if len(frames) != len(messages) {
				t.Fatalf("%d frames but %d messages", len(frames), len(messages))
			}

			for i := range frames {
				note := messages[i].Note
				want := messages[i].Descriptor

				got, err := decodeFrameHex(t, frames[i])
				if err != nil {
					t.Errorf("%s: decode: %v", note, err)
					continue
				}
				assertMessageMatches(t, note, got, want)

				encoded, err := c.Encode(NewMessageCodec(), want.message(t))
				if err != nil {
					t.Errorf("%s: encode: %v", note, err)
					continue
				}
				if hex.EncodeToString(encoded) != frames[i] {
					t.Errorf("%s: encoded\n  %x\nwant\n  %s", note, encoded, frames[i])
				}
			}
		})
	}
}

func TestVectorsNegative(t *testing.T) {
	var negative []struct {
		Hex    string `json:"hex"`
		Reason string `json:"reason"`
	}
	readVectorJSON(t, &negative, "negative", "frames.json")

	for _, v := range negative {
		if _, err := decodeFrameHex(t, v.Hex); err == nil {
			t.Errorf("%s: decoded %s without error", v.Reason, v.Hex)
		}
	}
}

func TestVectorsSequence(t *testing.T) {
	var seq struct {
		Concatenated string `json:"concatenated"`
		Count        int    `json:"count"`
	}
	readVectorJSON(t, &seq, "sequence", "frames.json")

	buf, err := hex.DecodeString(seq.Concatenated)
	if err != nil {
		t.Fatal(err)
	}

	// Codec level: one state, re-split by length prefix.
	state := c.NewState()
	state.Buffer = buf
	state.End = uint(len(buf))
	seen := 0
	for state.Start < state.End {
		if _, err := NewMessageCodec().Decode(state); err != nil {
			t.Fatalf("frame %d: %v", seen, err)
		}
		seen++
	}
	if seen != seq.Count {
		t.Fatalf("codec re-split %d frames, want %d", seen, seq.Count)
	}

	// Transport level: the same bytes through RPC.Receive on an io.Reader.
	rpc := NewRPC(struct {
		io.Reader
		io.Writer
	}{bytes.NewReader(buf), io.Discard})
	seen = 0
	for {
		_, err := rpc.Receive()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("frame %d: %v", seen, err)
		}
		seen++
	}
	if seen != seq.Count {
		t.Fatalf("Receive re-split %d frames, want %d", seen, seq.Count)
	}
}
