package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"codeflower-app/internal/config"
	"codeflower-app/internal/tunnel"
)

// The page's remote access card: what `codeflower tunnel login`, `pair`,
// `on`/`off`, `revoke` and `logout` do, on this computer only (remote.go
// refuses these through the tunnel).

// tunnelControl is the page's sign-in in progress and the tunnel's connection.
type tunnelControl struct {
	mu sync.Mutex
	// connected is the page's address through the tunnel once the connector
	// connected.
	connected string
	login     *tunnel.Login
	loginErr  string
	cancel    context.CancelFunc
}

type tunnelDevice struct {
	Key         string    `json:"key"`
	Label       string    `json:"label"`
	Fingerprint string    `json:"fingerprint"`
	PairedAt    time.Time `json:"pairedAt"`
	Workers     []string  `json:"workers,omitempty"`
}

type tunnelLogin struct {
	VerificationURL string    `json:"verificationUrl"`
	UserCode        string    `json:"userCode"`
	Name            string    `json:"name"`
	Fingerprint     string    `json:"fingerprint"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

type tunnelStatus struct {
	// Available: this client keeps a tunnel connected once signed in.
	Available bool   `json:"available"`
	URL       string `json:"url"`
	SignedIn  bool   `json:"signedIn"`
	Off       bool   `json:"off"`
	Region    string `json:"region,omitempty"`
	// EUOnly is config.yaml's answer, null until the first sign-in.
	EUOnly      *bool          `json:"euOnly"`
	Hardened    bool           `json:"hardened"`
	Manage      bool           `json:"manage"`
	Fingerprint string         `json:"fingerprint,omitempty"`
	Devices     []tunnelDevice `json:"devices"`
	// PageURL is where paired browsers open code flower, once connected.
	PageURL    string       `json:"pageUrl,omitempty"`
	Login      *tunnelLogin `json:"login,omitempty"`
	LoginError string       `json:"loginError,omitempty"`
}

func (s *Server) tunnelStatus() (tunnelStatus, error) {
	directory := s.options.ConfigDirectory
	status := tunnelStatus{Available: s.options.Tunnel != nil && directory != "", Devices: []tunnelDevice{}}
	loaded, err := config.Load(directory)
	if err != nil {
		return status, err
	}
	status.URL = loaded.Tunnel.URL
	if status.URL == "" {
		status.URL = tunnel.DefaultURL
	}
	status.EUOnly, status.Hardened, status.Manage = loaded.Tunnel.EUOnly, loaded.Tunnel.Hardened, loaded.Tunnel.Manage
	state, err := tunnel.LoadState(directory)
	if err != nil && !errors.Is(err, tunnel.ErrNotSignedIn) {
		return status, err
	}
	s.tunnel.mu.Lock()
	defer s.tunnel.mu.Unlock()
	if state != nil {
		status.SignedIn, status.URL, status.Off, status.Region = true, state.URL, state.Off, state.Region
		status.Fingerprint = tunnel.Fingerprint(state.PublicKey)
		for _, device := range state.Devices {
			status.Devices = append(status.Devices, tunnelDevice{
				Key: device.Key, Label: device.Label, Fingerprint: tunnel.Fingerprint(device.Key),
				PairedAt: device.PairedAt, Workers: device.Workers,
			})
		}
		if !state.Off && s.tunnel.connected == tunnel.PageURL(state) {
			status.PageURL = s.tunnel.connected
		}
	}
	if login := s.tunnel.login; login != nil {
		status.Login = &tunnelLogin{
			VerificationURL: login.Code.VerificationURL, UserCode: login.Code.UserCode, Name: login.Name,
			Fingerprint: tunnel.Fingerprint(login.PublicKey), ExpiresAt: login.Expires,
		}
	}
	status.LoginError = s.tunnel.loginErr
	return status, nil
}

func (s *Server) writeTunnelStatus(w http.ResponseWriter) {
	status, err := s.tunnelStatus()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) getTunnel(w http.ResponseWriter, _ *http.Request) { s.writeTunnelStatus(w) }

// startTunnelLogin asks the tunnel for a code, opens its approval page and
// waits for the approval in the background; the connector connects once the
// new state is saved.
func (s *Server) startTunnelLogin(w http.ResponseWriter, r *http.Request) {
	var request struct {
		EUOnly *bool `json:"euOnly"`
	}
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	directory := s.options.ConfigDirectory
	if s.options.Tunnel == nil || directory == "" {
		writeError(w, http.StatusConflict, errors.New("this client does not use a tunnel"))
		return
	}
	loaded, err := config.Load(directory)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	euOnly := loaded.Tunnel.EUOnly
	if request.EUOnly != nil && (euOnly == nil || *euOnly != *request.EUOnly) {
		if err := config.SetTunnel(directory, "", *request.EUOnly); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("could not save the answer: %w", err))
			return
		}
		euOnly = request.EUOnly
	}
	if euOnly == nil {
		writeError(w, http.StatusBadRequest, errors.New("choose whether the tunnel passes only through EU datacenters"))
		return
	}
	base := loaded.Tunnel.URL
	if base == "" {
		base = tunnel.DefaultURL
	}
	region := "any"
	if *euOnly {
		region = "eu"
	}
	name, _ := os.Hostname()
	s.cancelTunnelLogin()
	ctx, cancel := context.WithCancel(context.Background())
	login, err := tunnel.StartLogin(r.Context(), base, s.options.Version, name, region)
	if err != nil {
		cancel()
		writeError(w, http.StatusBadGateway, fmt.Errorf("could not reach the tunnel at %s: %w", base, err))
		return
	}
	s.tunnel.mu.Lock()
	s.tunnel.login, s.tunnel.loginErr, s.tunnel.cancel = login, "", cancel
	s.tunnel.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
		case <-s.quit:
			cancel()
		}
	}()
	go func() {
		err := login.Wait(ctx, directory)
		s.tunnel.mu.Lock()
		defer s.tunnel.mu.Unlock()
		if s.tunnel.login != login {
			return
		}
		s.tunnel.login, s.tunnel.cancel = nil, nil
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			s.tunnel.loginErr = err.Error()
			fmt.Fprintf(s.options.Log, "tunnel: signing in failed: %v\n", err)
		}
	}()
	_ = openBrowser(login.Code.VerificationURL)
	s.writeTunnelStatus(w)
}

func (s *Server) cancelTunnelLogin() {
	s.tunnel.mu.Lock()
	defer s.tunnel.mu.Unlock()
	if s.tunnel.cancel != nil {
		s.tunnel.cancel()
	}
	s.tunnel.login, s.tunnel.cancel, s.tunnel.loginErr = nil, nil, ""
}

func (s *Server) stopTunnelLogin(w http.ResponseWriter, _ *http.Request) {
	s.cancelTunnelLogin()
	s.writeTunnelStatus(w)
}

// pairTunnel makes a one-time pairing link; with tunnel.hardened only for
// the named sandboxes.
func (s *Server) pairTunnel(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Workers []string `json:"workers"`
	}
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	directory := s.options.ConfigDirectory
	loaded, err := config.Load(directory)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if loaded.Tunnel.Hardened {
		if len(request.Workers) == 0 {
			writeError(w, http.StatusBadRequest, errors.New("choose the sandboxes this browser may use (tunnel.hardened is on)"))
			return
		}
		s.mu.Lock()
		for _, id := range request.Workers {
			if s.workers[id] == nil {
				s.mu.Unlock()
				writeError(w, http.StatusBadRequest, fmt.Errorf("no sandbox %q in this client", id))
				return
			}
		}
		s.mu.Unlock()
	} else if len(request.Workers) > 0 {
		writeError(w, http.StatusBadRequest, errors.New(`pairing a browser with chosen sandboxes needs "tunnel: {hardened: true}" in config.yaml`))
		return
	}
	link, state, err := tunnel.Pair(directory, request.Workers)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"link": link, "fingerprint": tunnel.Fingerprint(state.PublicKey), "expiresAt": time.Now().Add(tunnel.PairingLifetime),
	})
}

// updateTunnel turns remote access on or off; pairings are kept.
func (s *Server) updateTunnel(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Off bool `json:"off"`
	}
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := tunnel.Update(s.options.ConfigDirectory, func(state *tunnel.State) error { state.Off = request.Off; return nil }); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	s.writeTunnelStatus(w)
}

// revokeTunnelDevice unpairs a browser by its key; its open chats close.
func (s *Server) revokeTunnelDevice(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Key string `json:"key"`
	}
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	err := tunnel.Update(s.options.ConfigDirectory, func(state *tunnel.State) error {
		if _, ok := state.Device(request.Key); !ok {
			return errors.New("no such paired browser")
		}
		state.Devices = slices.DeleteFunc(state.Devices, func(device tunnel.Device) bool { return device.Key == request.Key })
		return nil
	})
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	s.writeTunnelStatus(w)
}

// signOutTunnel removes this computer from the tunnel account and forgets its
// key and paired browsers.
func (s *Server) signOutTunnel(w http.ResponseWriter, r *http.Request) {
	directory := s.options.ConfigDirectory
	state, err := tunnel.LoadState(directory)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	var warning string
	if api, err := tunnel.NewAPI(state.URL, s.options.Version); err == nil {
		if err := api.Delete(r.Context(), state.Token); err != nil {
			warning = fmt.Sprintf("Could not remove this computer from your tunnel account (%v); revoke it on %s.", err, strings.TrimRight(state.URL, "/"))
		}
	}
	if err := tunnel.RemoveState(directory); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	status, err := s.tunnelStatus()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		tunnelStatus
		Warning string `json:"warning,omitempty"`
	}{status, warning})
}
