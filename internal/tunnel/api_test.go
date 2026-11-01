package tunnel

import "testing"

func TestTheTunnelMustUseHTTPSExceptOnThisComputer(t *testing.T) {
	for base, allowed := range map[string]bool{
		"https://tunnels.codeflower.app": true,
		"http://localhost:8787":          true,
		"http://127.0.0.1:8787":          true,
		"http://[::1]:8787":              true,
		"http://tunnel.example.org":      false,
		"http://10.0.0.5:8787":           false,
		"ftp://tunnel.example.org":       false,
	} {
		_, err := NewAPI(base, "test")
		if (err == nil) != allowed {
			t.Errorf("%s: allowed %v, error %v", base, allowed, err)
		}
	}
}
