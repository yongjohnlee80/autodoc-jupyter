// Package kernel runs a Jupyter kernel and speaks its messaging protocol (version 5.3) over ZeroMQ:
// pure Go, on github.com/go-zeromq/zmq4, with nothing beside the kernel the user already has
// (ipykernel, IRkernel, …). It reads kernelspecs where Jupyter keeps them (Specs), starts one with a
// connection file of its own (Start), and runs code on it, handing back each output message
// (Client.Execute).
package kernel

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ProtocolVersion is the messaging protocol version a message's header names.
const ProtocolVersion = "5.3"

// delimiter separates a message's routing identities from its signed parts.
var delimiter = []byte("<IDS|MSG>")

// Header is a message's header, and the parent header of a reply: the request it answers.
type Header struct {
	MsgID    string `json:"msg_id"`
	Session  string `json:"session"`
	Username string `json:"username"`
	Date     string `json:"date"`
	MsgType  string `json:"msg_type"`
	Version  string `json:"version"`
}

// Message is one Jupyter message. Content is the message type's own JSON; Buffers are its binary
// parts, which no message this package sends carries.
type Message struct {
	Identities [][]byte
	Header     Header
	Parent     Header
	Metadata   map[string]any
	Content    json.RawMessage
	Buffers    [][]byte
}

// ErrSignature is a received message whose signature is not its parts' HMAC under the key: it is
// dropped, never read.
var ErrSignature = errors.New("kernel: a message's signature does not match")

// Codec signs and checks messages with a connection's key (HMAC-SHA256); an empty key signs nothing,
// as Jupyter's does. The client speaks through one; so does a kernel written in Go, a test's.
type Codec struct{ Key []byte }

func (s Codec) sign(parts ...[]byte) []byte {
	if len(s.Key) == 0 {
		return nil
	}
	mac := hmac.New(sha256.New, s.Key)
	for _, p := range parts {
		mac.Write(p)
	}
	return []byte(hex.EncodeToString(mac.Sum(nil)))
}

// Encode is m as its wire frames: its identities, the delimiter, the signature, then header,
// parent header, metadata and content as JSON, then the buffers.
func (s Codec) Encode(m Message) ([][]byte, error) {
	header, err := json.Marshal(m.Header)
	if err != nil {
		return nil, err
	}
	parent := []byte("{}")
	if m.Parent.MsgID != "" {
		if parent, err = json.Marshal(m.Parent); err != nil {
			return nil, err
		}
	}
	metadata := []byte("{}")
	if len(m.Metadata) > 0 {
		if metadata, err = json.Marshal(m.Metadata); err != nil {
			return nil, err
		}
	}
	content := []byte(m.Content)
	if len(content) == 0 {
		content = []byte("{}")
	}
	frames := make([][]byte, 0, len(m.Identities)+6+len(m.Buffers))
	frames = append(frames, m.Identities...)
	frames = append(frames, delimiter, s.sign(header, parent, metadata, content), header, parent, metadata, content)
	return append(frames, m.Buffers...), nil
}

// Decode reads a message from its wire frames, refusing one whose signature does not match.
func (s Codec) Decode(frames [][]byte) (Message, error) {
	var m Message
	at := -1
	for i, f := range frames {
		if bytes.Equal(f, delimiter) {
			at = i
			break
		}
	}
	if at < 0 || len(frames) < at+6 {
		return m, fmt.Errorf("kernel: a message of %d frames has no delimiter and five parts after it", len(frames))
	}
	m.Identities = frames[:at]
	sig, header, parent, metadata, content := frames[at+1], frames[at+2], frames[at+3], frames[at+4], frames[at+5]
	if want := s.sign(header, parent, metadata, content); !hmac.Equal(sig, want) {
		return m, ErrSignature
	}
	if err := json.Unmarshal(header, &m.Header); err != nil {
		return m, fmt.Errorf("kernel: a message's header: %w", err)
	}
	if err := json.Unmarshal(parent, &m.Parent); err != nil {
		return m, fmt.Errorf("kernel: a message's parent header: %w", err)
	}
	if err := json.Unmarshal(metadata, &m.Metadata); err != nil {
		return m, fmt.Errorf("kernel: a message's metadata: %w", err)
	}
	m.Content = json.RawMessage(content)
	m.Buffers = frames[at+6:]
	return m, nil
}

// NewID is a random message or session id: 32 hex digits.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on the platforms Go supports
	}
	return hex.EncodeToString(b[:])
}

// NewHeader is a fresh header of msgType for session.
func NewHeader(session, msgType string) Header {
	return Header{
		MsgID:    NewID(),
		Session:  session,
		Username: "autodoc",
		Date:     time.Now().UTC().Format(time.RFC3339Nano),
		MsgType:  msgType,
		Version:  ProtocolVersion,
	}
}
