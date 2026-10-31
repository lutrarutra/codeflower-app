package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultURL is the hosted tunnel.
const DefaultURL = "https://tunnels.codeflower.app"

// ProtocolHeader carries Protocol on every request to the tunnel.
const ProtocolHeader = "Codeflower-Tunnel-Protocol"

// ProtocolError is the tunnel refusing this connector's protocol version:
// Code is upgrade_required (update code flower) or tunnel_too_old.
type ProtocolError struct {
	Code    string
	Message string
}

func (e *ProtocolError) Error() string { return e.Message }

// UpgradeRequired: this code flower is too old for the tunnel.
func (e *ProtocolError) UpgradeRequired() bool { return e.Code == "upgrade_required" }

// Deprecation is a version the tunnel will stop accepting after a date.
type Deprecation struct {
	Below int    `json:"below"`
	After string `json:"after"`
}

type VersionInfo struct {
	Protocol    int          `json:"protocol"`
	MinProtocol int          `json:"minProtocol"`
	Deprecated  *Deprecation `json:"deprecated"`
}

// DeprecatedFor is the date after which the tunnel refuses this connector, if set.
func (v VersionInfo) DeprecatedFor(protocol int) (string, bool) {
	if v.Deprecated != nil && protocol < v.Deprecated.Below {
		return v.Deprecated.After, true
	}
	return "", false
}

// API is the tunnel's HTTP interface for connectors (PROTOCOL.md).
type API struct {
	Base    *url.URL
	Version string
	HTTP    *http.Client
}

func NewAPI(base, version string) (*API, error) {
	parsed, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return nil, fmt.Errorf("not a tunnel URL: %q", base)
	}
	return &API{Base: parsed, Version: version, HTTP: &http.Client{Timeout: 30 * time.Second}}, nil
}

// Header is what every request and socket sends.
func (a *API) Header() http.Header {
	header := http.Header{}
	header.Set(ProtocolHeader, strconv.Itoa(Protocol))
	header.Set("User-Agent", "codeflower/"+a.Version)
	return header
}

func (a *API) endpoint(path string) string {
	return a.Base.String() + path
}

// WebSocketURL is the address of a socket path on the tunnel.
func (a *API) WebSocketURL(path string) string {
	scheme := "wss"
	if a.Base.Scheme == "http" {
		scheme = "ws"
	}
	return scheme + "://" + a.Base.Host + a.Base.Path + path
}

type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// CheckResponse turns the tunnel's refusals into errors; ProtocolError for
// version problems, so callers can tell the user to update.
func CheckResponse(response *http.Response) error {
	if response.StatusCode < 400 {
		return nil
	}
	var body apiError
	_ = json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&body)
	if body.Error == "upgrade_required" || body.Error == "tunnel_too_old" {
		return &ProtocolError{Code: body.Error, Message: body.Message}
	}
	if body.Error == "" {
		body.Error = response.Status
	}
	return &StatusError{Status: response.StatusCode, Code: body.Error}
}

// StatusError is any other refusal; Code is the tunnel's error code.
type StatusError struct {
	Status int
	Code   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("the tunnel answered %d (%s)", e.Status, e.Code)
}

func (a *API) do(ctx context.Context, method, path string, body any, token string, result any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, a.endpoint(path), reader)
	if err != nil {
		return err
	}
	request.Header = a.Header()
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := a.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := CheckResponse(response); err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result)
}

func (a *API) VersionInfo(ctx context.Context) (VersionInfo, error) {
	var info VersionInfo
	err := a.do(ctx, http.MethodGet, "/api/version", nil, "", &info)
	return info, err
}

type DeviceCode struct {
	DeviceCode      string `json:"deviceCode"`
	UserCode        string `json:"userCode"`
	VerificationURL string `json:"verificationUrl"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expiresIn"`
}

func (a *API) RequestDeviceCode(ctx context.Context, name, region, publicKey string) (DeviceCode, error) {
	var code DeviceCode
	err := a.do(ctx, http.MethodPost, "/api/device/code", map[string]string{"name": name, "region": region, "publicKey": publicKey}, "", &code)
	return code, err
}

// ErrPending: the user has not approved the code yet.
var ErrPending = errors.New("authorization_pending")

// ExchangeDeviceCode returns the new connector's ID and token once approved.
func (a *API) ExchangeDeviceCode(ctx context.Context, deviceCode string) (string, string, error) {
	var result struct {
		ConnectorID string `json:"connectorId"`
		Token       string `json:"token"`
	}
	err := a.do(ctx, http.MethodPost, "/api/device/token", map[string]string{"deviceCode": deviceCode}, "", &result)
	var status *StatusError
	if errors.As(err, &status) {
		switch status.Code {
		case "authorization_pending":
			return "", "", ErrPending
		case "access_denied":
			return "", "", errors.New("the sign-in was denied in the browser")
		case "expired_token":
			return "", "", errors.New("the code expired before it was approved; run the command again")
		}
	}
	return result.ConnectorID, result.Token, err
}

// SetKey tells the tunnel the connector's new public key (after rotating).
func (a *API) SetKey(ctx context.Context, token, publicKey string) error {
	return a.do(ctx, http.MethodPut, "/api/connector/key", map[string]string{"publicKey": publicKey}, token, nil)
}

// Delete removes this connector from the account (signing out).
func (a *API) Delete(ctx context.Context, token string) error {
	return a.do(ctx, http.MethodDelete, "/api/connector", nil, token, nil)
}
