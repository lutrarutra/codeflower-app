package tunnel

import (
	"encoding/json"
	"errors"
)

// Each transport message starts with one byte that says what it carries.
// A stream carries one HTTP exchange or one WebSocket; see the tunnel's
// PROTOCOL.md, "Envelope".
const (
	// KindOpen, browser → connector, first: an OpenRequest.
	KindOpen byte = 1
	// KindHead, connector → browser: the HTTP response's Head.
	KindHead byte = 2
	// KindData: a chunk of an HTTP body.
	KindData byte = 3
	// KindEnd: the end of an HTTP body.
	KindEnd byte = 4
	// KindText and KindBinary: a WebSocket message, or its last part.
	KindText   byte = 5
	KindBinary byte = 6
	// KindClose: a CloseFrame, closing the WebSocket or failing the request.
	KindClose byte = 7
	// KindAccepted, connector → browser: the WebSocket is open.
	KindAccepted byte = 8
	// KindTextPart and KindBinaryPart: a part of a longer message; the
	// last part is KindText or KindBinary.
	KindTextPart   byte = 9
	KindBinaryPart byte = 10
)

// MaxChunk is the most data one message carries, leaving room for the kind.
const MaxChunk = 60000

// MaxMessage bounds a WebSocket message assembled from parts.
const MaxMessage = 32 << 20

type OpenRequest struct {
	// Kind is "http" or "ws".
	Kind    string            `json:"kind"`
	Method  string            `json:"method,omitempty"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
}

type Head struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
}

type CloseFrame struct {
	Code   int    `json:"code"`
	Reason string `json:"reason,omitempty"`
}

func Frame(kind byte, data []byte) []byte {
	message := make([]byte, 1+len(data))
	message[0] = kind
	copy(message[1:], data)
	return message
}

func JSONFrame(kind byte, value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return Frame(kind, data), nil
}

func Unframe(message []byte) (byte, []byte, error) {
	if len(message) == 0 {
		return 0, nil, errors.New("empty message")
	}
	return message[0], message[1:], nil
}

// Parts splits a WebSocket message into frames of at most MaxChunk bytes.
func Parts(binary bool, data []byte) [][]byte {
	part, last := KindTextPart, KindText
	if binary {
		part, last = KindBinaryPart, KindBinary
	}
	var frames [][]byte
	for len(data) > MaxChunk {
		frames = append(frames, Frame(part, data[:MaxChunk]))
		data = data[MaxChunk:]
	}
	return append(frames, Frame(last, data))
}
