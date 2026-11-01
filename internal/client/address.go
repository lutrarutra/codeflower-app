package client

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"codeflower-app/internal/sandbox"
)

// The line a worker prints on start, e.g. codeflower://10.20.3.17:41234?token=Qm9…
// Users may paste it with surrounding text, so the first address is used. It
// matches parseWorkerAddress in the extension's src/address.ts.
var addressPattern = regexp.MustCompile(`(?i)(?:codeflower|wss?)://[^\s"'<>]+`)

var schemePattern = regexp.MustCompile(`(?i)^(?:codeflower|wss?):`)

// parseAddress reads a pasted worker address into a connection to it.
func parseAddress(text string) (sandbox.Connection, error) {
	match := addressPattern.FindString(strings.TrimSpace(text))
	if match == "" {
		return sandbox.Connection{}, errors.New("paste the codeflower://… line the worker printed when it started")
	}
	parsed, err := url.Parse(schemePattern.ReplaceAllString(match, "http:"))
	if err != nil {
		return sandbox.Connection{}, errors.New("the worker address is not valid")
	}
	token := parsed.Query().Get("token")
	if token == "" {
		return sandbox.Connection{}, errors.New("the worker address has no token; copy the whole line")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port <= 0 || port > 65535 {
		return sandbox.Connection{}, errors.New("the worker address has no port")
	}
	// The bridge's certificate's fingerprint: with it, the client speaks TLS
	// to the bridge and accepts only that certificate.
	fingerprint := parsed.Query().Get("fp")
	if fingerprint != "" && !sandbox.ValidFingerprint(fingerprint) {
		return sandbox.Connection{}, errors.New("the worker address's fingerprint (fp=) is damaged; copy the whole line")
	}
	host := parsed.Hostname()
	line := "codeflower://" + net.JoinHostPort(host, strconv.Itoa(port)) + "?token=" + url.QueryEscape(token)
	if fingerprint != "" {
		line += "&fp=" + fingerprint
	}
	return sandbox.Connection{URL: line, Host: host, Port: port, Token: token, Fingerprint: fingerprint}, nil
}
