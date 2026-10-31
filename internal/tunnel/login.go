package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"time"
)

// Explain turns the tunnel's version refusals into what to do.
func Explain(err error) error {
	var protocol *ProtocolError
	if errors.As(err, &protocol) {
		if protocol.UpgradeRequired() {
			return errors.New(`the tunnel needs a newer code flower: run "codeflower --update"`)
		}
		return errors.New("the tunnel is older than this code flower: its operator must update it")
	}
	return err
}

// Login is a sign-in waiting for the user to approve its code on the tunnel.
type Login struct {
	Code      DeviceCode
	Name      string
	Region    string
	PublicKey string
	// Expires is when the code can no longer be approved.
	Expires time.Time
	base    string
	api     *API
	keys    Keypair
}

// StartLogin asks the tunnel at base for a code to approve; region is "eu" or
// "any".
func StartLogin(ctx context.Context, base, version, name, region string) (*Login, error) {
	api, err := NewAPI(base, version)
	if err != nil {
		return nil, err
	}
	keys, err := GenerateKeypair()
	if err != nil {
		return nil, err
	}
	publicKey := EncodeKey(keys.Public)
	code, err := api.RequestDeviceCode(ctx, name, region, publicKey)
	if err != nil {
		return nil, Explain(err)
	}
	return &Login{
		Code: code, Name: name, Region: region, PublicKey: publicKey,
		Expires: time.Now().Add(time.Duration(code.ExpiresIn) * time.Second), base: base, api: api, keys: keys,
	}, nil
}

// Wait waits for the approval, then saves the new connector, replacing (and
// removing from the account) this computer's previous one.
func (l *Login) Wait(ctx context.Context, configDirectory string) error {
	connectorID, token, err := l.waitForApproval(ctx)
	if err != nil {
		return err
	}
	if old, err := LoadState(configDirectory); err == nil && old.Token != "" && old.URL == l.base {
		_ = l.api.Delete(ctx, old.Token)
	}
	host, _ := os.Hostname()
	state := &State{
		URL: l.base, ConnectorID: connectorID, Token: token, Region: l.Region, Host: host,
		PrivateKey: EncodeKey(l.keys.Private), PublicKey: l.PublicKey,
	}
	return state.Save(configDirectory)
}

func (l *Login) waitForApproval(ctx context.Context) (string, string, error) {
	interval := time.Duration(max(l.Code.Interval, 1)) * time.Second
	for time.Now().Before(l.Expires) {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(interval):
		}
		connectorID, token, err := l.api.ExchangeDeviceCode(ctx, l.Code.DeviceCode)
		if errors.Is(err, ErrPending) {
			continue
		}
		return connectorID, token, Explain(err)
	}
	return "", "", errors.New("the code expired before it was approved; sign in again")
}

// Pair adds a one-time pairing for the given sandboxes (all when empty) and
// returns the link a browser opens to use it.
func Pair(configDirectory string, workers []string) (string, *State, error) {
	state, err := LoadState(configDirectory)
	if err != nil {
		return "", nil, err
	}
	bytes := make([]byte, 32)
	_, _ = rand.Read(bytes)
	secret := base64.RawURLEncoding.EncodeToString(bytes)
	err = Update(configDirectory, func(current *State) error {
		current.Pairings = append(current.Pairings, Pairing{
			SecretHash: HashSecret(secret), Workers: workers, ExpiresAt: time.Now().Add(PairingLifetime),
		})
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return PairingLink(state, secret), state, nil
}
