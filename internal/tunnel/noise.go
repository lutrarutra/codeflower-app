// Package tunnel is the connector: it keeps code flower client reachable
// through a code flower tunnel (codeflower-tunnel), end-to-end encrypted, so
// a paired browser anywhere can use the client's page. The tunnel only pairs
// sockets; see its PROTOCOL.md.
package tunnel

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/flynn/noise"
)

// Protocol is the tunnel protocol version this connector speaks, sent with
// every request (Codeflower-Tunnel-Protocol).
const Protocol = 1

var suite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)

// Keypair is an X25519 key pair.
type Keypair = noise.DHKey

// GenerateKeypair makes a new static key pair.
func GenerateKeypair() (Keypair, error) {
	return suite.GenerateKeypair(rand.Reader)
}

// EncodeKey and DecodeKey: keys travel as unpadded base64url.
func EncodeKey(key []byte) string { return base64.RawURLEncoding.EncodeToString(key) }

func DecodeKey(text string) ([]byte, error) {
	key, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil || len(key) != 32 {
		return nil, errors.New("not a base64url X25519 key")
	}
	return key, nil
}

// Fingerprint is what the CLI, the tunnel's pages and the pairing page show
// for a public key: SHA-256 of its base64url text, 16 hex digits in groups.
func Fingerprint(publicKey string) string {
	sum := sha256.Sum256([]byte(publicKey))
	digits := hex.EncodeToString(sum[:])[:16]
	return strings.Join([]string{digits[0:4], digits[4:8], digits[8:12], digits[12:16]}, " ")
}

// Prologue binds a handshake to one connector.
func Prologue(connectorID string) []byte {
	return []byte("codeflower-tunnel/1 " + connectorID)
}

// Hello is the payload of the browser's first handshake message.
type Hello struct {
	V int `json:"v"`
	// Pair is a pairing secret, when the browser pairs.
	Pair string `json:"pair,omitempty"`
	// Device names the browser, e.g. "Firefox on Android".
	Device string `json:"device,omitempty"`
}

// Welcome is the payload of the connector's answer. Error refuses the
// browser: not_paired, pairing_expired, or off.
type Welcome struct {
	V     int    `json:"v"`
	Error string `json:"error,omitempty"`
}

// Channel encrypts and decrypts transport messages once the handshake is done.
// Noise nonces count messages, so a dropped, repeated or reordered frame
// fails to decrypt.
type Channel struct {
	send, receive *noise.CipherState
}

// MaxPlaintext is the most one transport message carries (Noise's 65535,
// less the tag).
const MaxPlaintext = 65535 - 16

func (c *Channel) Seal(plaintext []byte) ([]byte, error) {
	if len(plaintext) > MaxPlaintext {
		return nil, fmt.Errorf("message of %d bytes is too long", len(plaintext))
	}
	return c.send.Encrypt(nil, nil, plaintext)
}

func (c *Channel) Open(ciphertext []byte) ([]byte, error) {
	return c.receive.Decrypt(nil, nil, ciphertext)
}

func handshake(static Keypair, connectorID string, initiator bool, peer []byte, random io.Reader) (*noise.HandshakeState, error) {
	config := noise.Config{
		CipherSuite:   suite,
		Random:        random,
		Pattern:       noise.HandshakeIK,
		Initiator:     initiator,
		Prologue:      Prologue(connectorID),
		StaticKeypair: static,
		PeerStatic:    peer,
	}
	return noise.NewHandshakeState(config)
}

// Responder is the connector's side of a handshake in progress.
type Responder struct {
	state *noise.HandshakeState
	// Peer is the browser's static public key.
	Peer []byte
	// Hello is what the browser sent with it.
	Hello Hello
}

// Accept reads the browser's first message.
func Accept(static Keypair, connectorID string, message []byte) (*Responder, error) {
	return accept(static, connectorID, message, rand.Reader)
}

// accept and dial take the source of the ephemeral key: fixed in test vectors.
func accept(static Keypair, connectorID string, message []byte, random io.Reader) (*Responder, error) {
	state, err := handshake(static, connectorID, false, nil, random)
	if err != nil {
		return nil, err
	}
	payload, _, _, err := state.ReadMessage(nil, message)
	if err != nil {
		return nil, fmt.Errorf("handshake: %w", err)
	}
	responder := &Responder{state: state, Peer: state.PeerStatic()}
	if err := json.Unmarshal(payload, &responder.Hello); err != nil {
		return nil, fmt.Errorf("handshake: unreadable hello: %w", err)
	}
	return responder, nil
}

// Answer writes the connector's message. With an error in the welcome the
// channel is useless and the stream should close after sending it.
func (r *Responder) Answer(welcome Welcome) ([]byte, *Channel, error) {
	welcome.V = Protocol
	payload, err := json.Marshal(welcome)
	if err != nil {
		return nil, nil, err
	}
	message, toResponder, toInitiator, err := r.state.WriteMessage(nil, payload)
	if err != nil {
		return nil, nil, err
	}
	return message, &Channel{send: toInitiator, receive: toResponder}, nil
}

// Initiator is a browser's side of a handshake, for tests and test vectors;
// browsers run ui's TypeScript implementation.
type Initiator struct {
	state *noise.HandshakeState
}

func Dial(static Keypair, connectorID string, connector []byte, hello Hello) (*Initiator, []byte, error) {
	return dial(static, connectorID, connector, hello, rand.Reader)
}

func dial(static Keypair, connectorID string, connector []byte, hello Hello, random io.Reader) (*Initiator, []byte, error) {
	state, err := handshake(static, connectorID, true, connector, random)
	if err != nil {
		return nil, nil, err
	}
	hello.V = Protocol
	payload, err := json.Marshal(hello)
	if err != nil {
		return nil, nil, err
	}
	message, _, _, err := state.WriteMessage(nil, payload)
	if err != nil {
		return nil, nil, err
	}
	return &Initiator{state: state}, message, nil
}

func (i *Initiator) Finish(message []byte) (Welcome, *Channel, error) {
	var welcome Welcome
	payload, toResponder, toInitiator, err := i.state.ReadMessage(nil, message)
	if err != nil {
		return welcome, nil, fmt.Errorf("handshake: %w", err)
	}
	if err := json.Unmarshal(payload, &welcome); err != nil {
		return welcome, nil, err
	}
	return welcome, &Channel{send: toResponder, receive: toInitiator}, nil
}
