package client

import (
	"net/http"
	"strings"
	"testing"

	"codeflower-app/internal/tunnel"
)

func TestThePageSignsInPairsAndTurnsTheTunnelOff(t *testing.T) {
	server, page := testServer(t)
	server.options.ConfigDirectory = t.TempDir()
	server.options.Tunnel = &TunnelOptions{}

	response, status := authorized(t, page, http.MethodGet, "/api/tunnel", "")
	if response.StatusCode != http.StatusOK || status["available"] != true || status["signedIn"] != false || status["euOnly"] != nil {
		t.Fatalf("before signing in: %d %v", response.StatusCode, status)
	}
	if response, data := authorized(t, page, http.MethodPost, "/api/tunnel/login", "{}"); response.StatusCode != http.StatusBadRequest || !strings.Contains(data["error"].(string), "EU") {
		t.Fatalf("signing in without the EU answer: %d %v", response.StatusCode, data)
	}
	if response, _ := authorized(t, page, http.MethodPost, "/api/tunnel/pair", "{}"); response.StatusCode != http.StatusConflict {
		t.Fatalf("pairing before signing in: %d", response.StatusCode)
	}

	keys, _ := tunnel.GenerateKeypair()
	state := &tunnel.State{URL: "https://tunnel.example", ConnectorID: "c1", Token: "t", Region: "any",
		PrivateKey: tunnel.EncodeKey(keys.Private), PublicKey: tunnel.EncodeKey(keys.Public)}
	if err := state.Save(server.options.ConfigDirectory); err != nil {
		t.Fatal(err)
	}
	response, pairing := authorized(t, page, http.MethodPost, "/api/tunnel/pair", "{}")
	link, _ := pairing["link"].(string)
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(link, "https://tunnel.example/p#c1.") || pairing["fingerprint"] != tunnel.Fingerprint(state.PublicKey) {
		t.Fatalf("pairing: %d %v", response.StatusCode, pairing)
	}
	if saved, _ := tunnel.LoadState(server.options.ConfigDirectory); len(saved.Pairings) != 1 {
		t.Fatalf("pairings: %+v", saved.Pairings)
	}
	if response, data := authorized(t, page, http.MethodPost, "/api/tunnel/pair", `{"workers":["w1"]}`); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("choosing sandboxes without hardened: %d %v", response.StatusCode, data)
	}

	if response, status := authorized(t, page, http.MethodPatch, "/api/tunnel", `{"off":true}`); response.StatusCode != http.StatusOK || status["off"] != true || status["signedIn"] != true {
		t.Fatalf("turning off: %d %v", response.StatusCode, status)
	}
	server.tunnel.connected = tunnel.PageURL(state)
	if _, status := authorized(t, page, http.MethodGet, "/api/tunnel", ""); status["pageUrl"] != nil {
		t.Fatalf("an off tunnel shows its address: %v", status)
	}
	if _, status := authorized(t, page, http.MethodPatch, "/api/tunnel", `{"off":false}`); status["pageUrl"] != "https://tunnel.example/m/c1/" {
		t.Fatalf("connected: %v", status)
	}
}
