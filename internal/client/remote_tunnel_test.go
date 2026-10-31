package client

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codeflower-app/internal/tunnel"
)

// tunnelPage serves the remote page as the connector reaches it, with two
// added workers; it returns the page and the workers' IDs.
func tunnelPage(t *testing.T, manage bool) (*httptest.Server, string, string) {
	t.Helper()
	server, local := testServer(t)
	first, _ := fakeBridge(t, "token-one")
	second, _ := fakeBridge(t, "token-two")
	one := connectTo(t, local, first)["id"].(string)
	two := connectTo(t, local, second)["id"].(string)
	page := httptest.NewServer((&remote{server: server, manage: manage}).routes())
	t.Cleanup(page.Close)
	return page, one, two
}

func remoteRequest(t *testing.T, page *httptest.Server, method, path, workers string) (*http.Response, []byte) {
	t.Helper()
	var body *strings.Reader
	if method == http.MethodGet {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader("{}")
	}
	request, _ := http.NewRequest(method, page.URL+path, body)
	if method != http.MethodGet {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set(tunnel.DeviceHeader, "Phone")
	request.Header.Set(tunnel.WorkersHeader, workers)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var data strings.Builder
	_, _ = bufio.NewReader(response.Body).WriteTo(&data)
	return response, []byte(data.String())
}

func TestRemoteBrowsersSeeOnlyTheirWorkersAndNoBridgeTokens(t *testing.T) {
	page, one, two := tunnelPage(t, false)

	response, body := remoteRequest(t, page, http.MethodGet, "/api/workers", "*")
	var all []map[string]any
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &all) != nil || len(all) != 2 {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	if strings.Contains(string(body), "token-one") || strings.Contains(string(body), "token-two") {
		t.Fatalf("a bridge token reached the remote browser: %s", body)
	}
	for _, worker := range all {
		if _, has := worker["url"]; has {
			t.Fatalf("the worker's address was listed: %v", worker)
		}
	}

	_, body = remoteRequest(t, page, http.MethodGet, "/api/workers", one)
	var scoped []map[string]any
	_ = json.Unmarshal(body, &scoped)
	if len(scoped) != 1 || scoped[0]["id"] != one {
		t.Fatalf("scoped list: %s", body)
	}
	if response, _ := remoteRequest(t, page, http.MethodGet, "/api/workers/"+two+"/sessions", one); response.StatusCode != http.StatusForbidden {
		t.Fatalf("another worker's sessions: %d", response.StatusCode)
	}
	if response, _ := remoteRequest(t, page, http.MethodGet, "/api/workers/"+one+"/sessions", one); response.StatusCode != http.StatusOK {
		t.Fatalf("its own worker's sessions: %d", response.StatusCode)
	}
}

func TestRemoteBrowsersCannotConnectAnywhereQuitOrManageUnlessAllowed(t *testing.T) {
	page, _, _ := tunnelPage(t, false)
	for _, refused := range []struct{ method, path string }{
		{http.MethodPost, "/api/workers/connect"},
		{http.MethodPost, "/api/quit"},
		{http.MethodDelete, "/api/quit"},
		{http.MethodGet, "/api/folders"},
		{http.MethodPost, "/api/folders"},
		{http.MethodPost, "/api/workers"},
	} {
		if response, _ := remoteRequest(t, page, refused.method, refused.path, "*"); response.StatusCode < 400 {
			t.Errorf("%s %s answered %d", refused.method, refused.path, response.StatusCode)
		}
	}
	for _, allowed := range []string{"/", "/api/system", "/api/quit"} {
		if response, _ := remoteRequest(t, page, http.MethodGet, allowed, "*"); response.StatusCode != http.StatusOK {
			t.Errorf("GET %s answered %d", allowed, response.StatusCode)
		}
	}

	managed, _, _ := tunnelPage(t, true)
	if response, _ := remoteRequest(t, managed, http.MethodGet, "/api/folders", "*"); response.StatusCode != http.StatusOK {
		t.Errorf("with manage, the folder browser answered %d", response.StatusCode)
	}
	if response, _ := remoteRequest(t, managed, http.MethodPost, "/api/workers/connect", "*"); response.StatusCode < 400 {
		t.Errorf("even with manage, connect answered %d", response.StatusCode)
	}
}

func TestRemoteChatsReachOnlyAllowedWorkersWithoutAnOrigin(t *testing.T) {
	page, one, two := tunnelPage(t, false)
	open := func(id string) int {
		host := strings.TrimPrefix(page.URL, "http://")
		conn, err := net.Dial("tcp", host)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.Write([]byte("GET /api/workers/" + id + "/acp HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
			tunnel.WorkersHeader + ": " + one + "\r\n\r\n"))
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode
	}
	if status := open(one); status != http.StatusSwitchingProtocols {
		t.Fatalf("allowed worker: %d", status)
	}
	if status := open(two); status != http.StatusForbidden {
		t.Fatalf("other worker: %d", status)
	}
}
