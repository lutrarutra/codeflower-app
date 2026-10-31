package tunnel

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/noise-vectors.json")

func fixedKey(t *testing.T, seed byte) Keypair {
	t.Helper()
	key, err := suite.GenerateKeypair(bytes.NewReader(bytes.Repeat([]byte{seed}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func newKey(t *testing.T) Keypair {
	t.Helper()
	key, err := GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestBrowserAndConnectorAgreeAndAuthenticateEachOther(t *testing.T) {
	connector, browser := newKey(t), newKey(t)
	initiator, first, err := Dial(browser, "eu_abc", connector.Public, Hello{Pair: "secret", Device: "Firefox"})
	if err != nil {
		t.Fatal(err)
	}
	responder, err := Accept(connector, "eu_abc", first)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(responder.Peer, browser.Public) || responder.Hello.Pair != "secret" || responder.Hello.Device != "Firefox" {
		t.Fatalf("responder saw %x %+v", responder.Peer, responder.Hello)
	}
	second, connectorChannel, err := responder.Answer(Welcome{})
	if err != nil {
		t.Fatal(err)
	}
	welcome, browserChannel, err := initiator.Finish(second)
	if err != nil || welcome.V != Protocol || welcome.Error != "" {
		t.Fatalf("welcome %+v, %v", welcome, err)
	}

	sealed, _ := browserChannel.Seal([]byte("to the connector"))
	if opened, err := connectorChannel.Open(sealed); err != nil || string(opened) != "to the connector" {
		t.Fatalf("connector opened %q, %v", opened, err)
	}
	sealed, _ = connectorChannel.Seal([]byte("to the browser"))
	if opened, err := browserChannel.Open(sealed); err != nil || string(opened) != "to the browser" {
		t.Fatalf("browser opened %q, %v", opened, err)
	}
}

func TestAWrongConnectorKeyOrConnectorIDFailsTheHandshake(t *testing.T) {
	connector, impostor, browser := newKey(t), newKey(t), newKey(t)
	_, first, _ := Dial(browser, "eu_abc", connector.Public, Hello{})
	if _, err := Accept(impostor, "eu_abc", first); err == nil {
		t.Fatal("a connector without the browser's expected key read the hello")
	}
	if _, err := Accept(connector, "eu_other", first); err == nil {
		t.Fatal("a handshake meant for another connector was accepted")
	}
}

func TestRepeatedOrReorderedFramesFailToDecrypt(t *testing.T) {
	connector, browser := newKey(t), newKey(t)
	initiator, first, _ := Dial(browser, "c", connector.Public, Hello{})
	responder, _ := Accept(connector, "c", first)
	second, connectorChannel, _ := responder.Answer(Welcome{})
	_, browserChannel, _ := initiator.Finish(second)

	one, _ := browserChannel.Seal([]byte("one"))
	two, _ := browserChannel.Seal([]byte("two"))
	if _, err := connectorChannel.Open(two); err == nil {
		t.Fatal("a frame out of order decrypted")
	}
	// A failed frame ends the stream in practice; a fresh pair shows a replay fails too.
	initiator, first, _ = Dial(browser, "c", connector.Public, Hello{})
	responder, _ = Accept(connector, "c", first)
	second, connectorChannel, _ = responder.Answer(Welcome{})
	_, browserChannel, _ = initiator.Finish(second)
	one, _ = browserChannel.Seal([]byte("one"))
	if _, err := connectorChannel.Open(one); err != nil {
		t.Fatal(err)
	}
	if _, err := connectorChannel.Open(one); err == nil {
		t.Fatal("a replayed frame decrypted")
	}
}

func TestFingerprintMatchesTheTunnelsPages(t *testing.T) {
	// The tunnel's fingerprint(): SHA-256 of the base64url text, first 16 hex digits.
	key := EncodeKey(bytes.Repeat([]byte{7}, 32))
	sum := Fingerprint(key)
	if len(sum) != 19 || sum[4] != ' ' {
		t.Fatalf("fingerprint %q", sum)
	}
}

type vectorKey struct {
	Private string `json:"private"`
	Public  string `json:"public"`
}

type vectorMessage struct {
	From       string `json:"from"`
	Plaintext  string `json:"plaintext"`
	Ciphertext string `json:"ciphertext"`
}

type vectors struct {
	Protocol           string          `json:"protocol"`
	ConnectorID        string          `json:"connectorId"`
	Prologue           string          `json:"prologue"`
	BrowserStatic      vectorKey       `json:"browserStatic"`
	BrowserEphemeral   vectorKey       `json:"browserEphemeral"`
	ConnectorStatic    vectorKey       `json:"connectorStatic"`
	ConnectorEphemeral vectorKey       `json:"connectorEphemeral"`
	Hello              string          `json:"hello"`
	Message1           string          `json:"message1"`
	Welcome            string          `json:"welcome"`
	Message2           string          `json:"message2"`
	Transport          []vectorMessage `json:"transport"`
}

func asVector(key Keypair) vectorKey {
	return vectorKey{Private: hex.EncodeToString(key.Private), Public: hex.EncodeToString(key.Public)}
}

// The vectors pin the wire format: ui's TypeScript implementation reproduces
// them byte for byte (ui/test/fixtures/noise-vectors.json is a copy).
func TestVectors(t *testing.T) {
	browser, browserEphemeral := fixedKey(t, 1), fixedKey(t, 2)
	connector, connectorEphemeral := fixedKey(t, 3), fixedKey(t, 4)
	const id = "eu_vectors"
	seed := func(b byte) *bytes.Reader { return bytes.NewReader(bytes.Repeat([]byte{b}, 32)) }
	initiator, first, err := dial(browser, id, connector.Public, Hello{Pair: "pairing-secret", Device: "Test browser"}, seed(2))
	if err != nil {
		t.Fatal(err)
	}
	responder, err := accept(connector, id, first, seed(4))
	if err != nil {
		t.Fatal(err)
	}
	second, connectorChannel, err := responder.Answer(Welcome{})
	if err != nil {
		t.Fatal(err)
	}
	_, browserChannel, err := initiator.Finish(second)
	if err != nil {
		t.Fatal(err)
	}
	hello, _ := json.Marshal(Hello{V: Protocol, Pair: "pairing-secret", Device: "Test browser"})
	welcome, _ := json.Marshal(Welcome{V: Protocol})
	got := vectors{
		Protocol:           "Noise_IK_25519_ChaChaPoly_SHA256",
		ConnectorID:        id,
		Prologue:           string(Prologue(id)),
		BrowserStatic:      asVector(browser),
		BrowserEphemeral:   asVector(browserEphemeral),
		ConnectorStatic:    asVector(connector),
		ConnectorEphemeral: asVector(connectorEphemeral),
		Hello:              string(hello),
		Message1:           hex.EncodeToString(first),
		Welcome:            string(welcome),
		Message2:           hex.EncodeToString(second),
	}
	open, _ := JSONFrame(KindOpen, OpenRequest{Kind: "http", Method: "GET", Path: "/api/workers"})
	for _, step := range []struct {
		from    string
		message []byte
	}{
		{"browser", open},
		{"connector", Frame(KindData, []byte(`[{"id":"w1"}]`))},
		{"browser", Frame(KindEnd, nil)},
	} {
		channel, peer := browserChannel, connectorChannel
		if step.from == "connector" {
			channel, peer = connectorChannel, browserChannel
		}
		sealed, err := channel.Seal(step.message)
		if err != nil {
			t.Fatal(err)
		}
		if opened, err := peer.Open(sealed); err != nil || !bytes.Equal(opened, step.message) {
			t.Fatalf("round trip failed: %v", err)
		}
		got.Transport = append(got.Transport, vectorMessage{From: step.from, Plaintext: hex.EncodeToString(step.message), Ciphertext: hex.EncodeToString(sealed)})
	}

	path := filepath.Join("testdata", "noise-vectors.json")
	encoded, _ := json.MarshalIndent(got, "", "  ")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -run TestVectors -update)", err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), encoded) {
		t.Fatal("the wire format changed: if on purpose, run go test -run TestVectors -update and copy the file to ui")
	}
}

func TestLongWebSocketMessagesAreSplitIntoParts(t *testing.T) {
	data := bytes.Repeat([]byte("x"), MaxChunk*2+5)
	parts := Parts(false, data)
	if len(parts) != 3 || parts[0][0] != KindTextPart || parts[1][0] != KindTextPart || parts[2][0] != KindText || len(parts[2]) != 6 {
		t.Fatalf("parts: %d", len(parts))
	}
	if single := Parts(true, []byte("y")); len(single) != 1 || single[0][0] != KindBinary {
		t.Fatal("a short message is one frame")
	}
}
