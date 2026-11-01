package tunnel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ErrNotSignedIn: there is no connector yet; `codeflower tunnel login` makes one.
var ErrNotSignedIn = errors.New(`not signed in to a tunnel: run "codeflower tunnel login"`)

// Device is a paired browser.
type Device struct {
	// Key is the browser's static public key, base64url.
	Key   string `json:"key"`
	Label string `json:"label"`
	// Workers are the only workers it may use (hardened mode); empty means all.
	Workers  []string  `json:"workers,omitempty"`
	PairedAt time.Time `json:"pairedAt"`
}

// Pairing is an unused pairing link.
type Pairing struct {
	// SecretHash is the SHA-256 of the link's secret, hex.
	SecretHash string    `json:"secretHash"`
	Workers    []string  `json:"workers,omitempty"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// State is the connector's: its identity at the tunnel, its key, the
// browsers paired with it. The file is the connector's only configuration
// at run time: the CLI changes it (pair, revoke, off) and a running connector
// picks the change up within a second.
//
// Host is the computer that holds the connector, and HeldAt when its
// connector last said so. A config directory shared between computers (a
// cluster's home) connects from one at a time: the tunnel keeps one
// connection per connector, so two would push each other out. Another
// computer takes the connector over once the hold is released or lapses (a
// batch job that ended on one node and starts on another), so browsers stay
// paired and nobody signs in again.
type State struct {
	URL         string    `json:"url"`
	ConnectorID string    `json:"connectorId"`
	Token       string    `json:"token"`
	PrivateKey  string    `json:"privateKey"`
	PublicKey   string    `json:"publicKey"`
	Region      string    `json:"region"`
	Host        string    `json:"host,omitempty"`
	HeldAt      time.Time `json:"heldAt,omitzero"`
	Off         bool      `json:"off,omitempty"`
	Devices     []Device  `json:"devices,omitempty"`
	Pairings    []Pairing `json:"pairings,omitempty"`
}

// PairingLifetime is how long a pairing link works.
const PairingLifetime = 10 * time.Minute

// Directory holds the state file and the socket the connector reaches the app by.
func Directory(configDirectory string) string {
	return filepath.Join(configDirectory, "tunnel")
}

func statePath(configDirectory string) string {
	return filepath.Join(Directory(configDirectory), "state.json")
}

// SocketPath is where the client serves its page to the connector.
func SocketPath(configDirectory string) string {
	return filepath.Join(Directory(configDirectory), "app.sock")
}

func LoadState(configDirectory string) (*State, error) {
	data, err := os.ReadFile(statePath(configDirectory))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotSignedIn
	}
	if err != nil {
		return nil, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("%s: %w", statePath(configDirectory), err)
	}
	return &state, nil
}

// Save writes the state privately and atomically.
func (s *State) Save(configDirectory string) error {
	directory := Directory(configDirectory)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), statePath(configDirectory))
}

// Update changes the state under a lock, so the CLI and a running connector
// do not overwrite each other's changes.
func Update(configDirectory string, change func(*State) error) error {
	unlock, err := lock(configDirectory)
	if err != nil {
		return err
	}
	defer unlock()
	state, err := LoadState(configDirectory)
	if err != nil {
		return err
	}
	if err := change(state); err != nil {
		return err
	}
	return state.Save(configDirectory)
}

func lock(configDirectory string) (func(), error) {
	if err := os.MkdirAll(Directory(configDirectory), 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(Directory(configDirectory), "state.lock")
	deadline := time.Now().Add(5 * time.Second)
	for {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			file.Close()
			return func() { os.Remove(path) }, nil
		}
		// A lock older than this was left by a process that died.
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > 10*time.Second {
			os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the tunnel state is locked (%s)", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// HashSecret is how pairing secrets are stored.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Keypair is the connector's static key pair.
func (s *State) Keypair() (Keypair, error) {
	private, err := DecodeKey(s.PrivateKey)
	if err != nil {
		return Keypair{}, err
	}
	public, err := DecodeKey(s.PublicKey)
	if err != nil {
		return Keypair{}, err
	}
	return Keypair{Private: private, Public: public}, nil
}

func (s *State) Device(key string) (Device, bool) {
	index := slices.IndexFunc(s.Devices, func(device Device) bool { return device.Key == key })
	if index < 0 {
		return Device{}, false
	}
	return s.Devices[index], true
}

// ClaimPairing pairs the browser `key` if `secret` is an unused, unexpired
// pairing link, which is then used up.
func (s *State) ClaimPairing(secret, key, label string, now time.Time) (Device, error) {
	hash := HashSecret(secret)
	s.Pairings = slices.DeleteFunc(s.Pairings, func(pairing Pairing) bool { return !pairing.ExpiresAt.After(now) })
	index := slices.IndexFunc(s.Pairings, func(pairing Pairing) bool { return pairing.SecretHash == hash })
	if index < 0 {
		return Device{}, errors.New("pairing_expired")
	}
	pairing := s.Pairings[index]
	s.Pairings = slices.Delete(s.Pairings, index, index+1)
	if label == "" {
		label = "Browser"
	}
	if len(label) > 80 {
		label = label[:80]
	}
	device := Device{Key: key, Label: label, Workers: pairing.Workers, PairedAt: now}
	s.Devices = append(slices.DeleteFunc(s.Devices, func(other Device) bool { return other.Key == key }), device)
	return device, nil
}

// PairingLink is the link a browser opens to pair: the connector, its key and
// the secret travel in the fragment, which browsers never send to the tunnel.
func PairingLink(state *State, secret string) string {
	return strings.TrimRight(state.URL, "/") + "/p#" + state.ConnectorID + "." + state.PublicKey + "." + secret
}

// RemoveState forgets the connector (signing out).
func RemoveState(configDirectory string) error {
	err := os.Remove(statePath(configDirectory))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// PageURL is where paired browsers open this computer's code flower.
func PageURL(state *State) string {
	return strings.TrimRight(state.URL, "/") + "/m/" + state.ConnectorID + "/"
}

// A running connector renews its hold every HoldRenew; one not renewed for
// HoldLimit lapsed (its computer stopped without releasing it, as a killed
// batch job does).
const (
	HoldRenew = 30 * time.Second
	HoldLimit = 90 * time.Second
)

// ErrHeld: another computer's connector holds the connector.
type ErrHeld struct{ Host string }

func (e *ErrHeld) Error() string {
	return fmt.Sprintf("%s holds the tunnel connection; it is released when its code flower client quits", e.Host)
}

// MayConnect says whether this computer may connect the connector: it holds
// it, or nobody does, or the holder's hold lapsed.
func (s *State) MayConnect(now time.Time) bool {
	host, _ := os.Hostname()
	return s.Host == "" || s.Host == host || now.Sub(s.HeldAt) > HoldLimit
}

// Hold takes or renews this computer's hold on the connector, unless another
// computer holds it.
func Hold(configDirectory string) error {
	host, _ := os.Hostname()
	return Update(configDirectory, func(state *State) error {
		now := time.Now()
		if !state.MayConnect(now) {
			return &ErrHeld{Host: state.Host}
		}
		state.Host, state.HeldAt = host, now
		return nil
	})
}

// Release lets another computer take the connector at once, if this one
// holds it.
func Release(configDirectory string) error {
	host, _ := os.Hostname()
	err := Update(configDirectory, func(state *State) error {
		if state.Host == host {
			state.HeldAt = time.Time{}
		}
		return nil
	})
	if errors.Is(err, ErrNotSignedIn) {
		return nil
	}
	return err
}

// Expected: signed in and on, so the client should come online.
func Expected(configDirectory string) bool {
	state, err := LoadState(configDirectory)
	return err == nil && !state.Off
}
