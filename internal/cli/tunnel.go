package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/mdp/qrterminal/v3"
	"github.com/moby/term"
	"github.com/spf13/cobra"

	"codeflower-app/internal/client"
	"codeflower-app/internal/config"
	"codeflower-app/internal/tunnel"
)

func tunnelCommand() *cobra.Command {
	var configDirectory string
	command := &cobra.Command{
		Use:   "tunnel",
		Short: "Use code flower from anywhere through a code flower tunnel",
		Long: `Reach code flower client on this computer from any browser or phone, through a
code flower tunnel (https://tunnels.codeflower.app, or your own: "tunnel: {url: …}"
in config.yaml).

  codeflower tunnel login    sign this computer in (once)
  codeflower tunnel pair     pair a browser: open the link it prints there
  codeflower client          keeps the tunnel connected while it runs

Everything between a paired browser and this computer is end-to-end encrypted;
the tunnel cannot read it. Signing in to the tunnel alone does not give access:
a browser must also be paired here.

A paired browser can use your sandboxes' chats, which run code in the sandboxes.
"tunnel: {hardened: true}" limits each browser to the sandboxes chosen when
pairing it and confines the connector (Linux). "tunnel: {manage: true}" also lets
paired browsers start sandboxes and browse folders.`,
	}
	command.PersistentFlags().StringVar(&configDirectory, "config", "",
		"config directory with config.yaml (or set $CODEFLOWER_CONFIG; default ~/.codeflower)")
	directory := func() string { return config.Directory(configDirectory) }
	command.AddCommand(
		tunnelLoginCommand(directory),
		tunnelPairCommand(directory),
		tunnelStatusCommand(directory),
		tunnelDevicesCommand(directory),
		tunnelRevokeCommand(directory),
		tunnelRotateCommand(directory),
		tunnelVerifyCommand(directory),
		tunnelSwitchCommand(directory, "off", "Turn remote access off at once (pairings are kept)", true),
		tunnelSwitchCommand(directory, "on", "Turn remote access on again", false),
		tunnelLogoutCommand(directory),
		tunnelConnectCommand(directory),
	)
	return command
}

func tunnelURL(loaded *config.Config, flag string) string {
	if flag != "" {
		return flag
	}
	if loaded.Tunnel.URL != "" {
		return loaded.Tunnel.URL
	}
	return tunnel.DefaultURL
}

// explainProtocol turns the tunnel's version refusals into what to do.
func explainProtocol(err error) error {
	var protocol *tunnel.ProtocolError
	if errors.As(err, &protocol) {
		if protocol.UpgradeRequired() {
			return errors.New(`the tunnel needs a newer code flower: run "codeflower --update"`)
		}
		return errors.New("the tunnel is older than this code flower: its operator must update it")
	}
	return err
}

// askEUOnly asks the EU question until it gets a yes or a no.
func askEUOnly(in io.Reader, out io.Writer) (bool, error) {
	reader := bufio.NewReader(in)
	for {
		fmt.Fprint(out, "Route this tunnel only through EU datacenters? Slower from outside Europe. [y/n]: ")
		line, err := reader.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		if err != nil {
			return false, errors.New("no answer to the EU question")
		}
		fmt.Fprintln(out, "Please answer y or n.")
	}
}

func tunnelLoginCommand(directory func() string) *cobra.Command {
	var url, name string
	var noBrowser bool
	command := &cobra.Command{
		Use:   "login",
		Short: "Sign this computer in to the tunnel",
		Long: `Sign this computer in to the tunnel: it prints a code to approve in your browser,
signed in to the tunnel. The first time, it asks whether the tunnel should pass
only through EU datacenters, and saves the answer in config.yaml (tunnel.eu_only);
change it there and sign in again to switch.

Signing in again replaces this computer's connector: paired browsers must pair again.`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx := command.Context()
			dir := directory()
			loaded, err := config.Load(dir)
			if err != nil {
				return err
			}
			base := tunnelURL(loaded, url)
			api, err := tunnel.NewAPI(base, Version)
			if err != nil {
				return err
			}
			if _, err := api.VersionInfo(ctx); err != nil {
				return explainProtocol(fmt.Errorf("could not reach the tunnel at %s: %w", base, err))
			}
			euOnly := loaded.Tunnel.EUOnly
			if euOnly == nil {
				if !term.IsTerminal(os.Stdin.Fd()) {
					return fmt.Errorf("set tunnel.eu_only to true or false in %s, then sign in again", loaded.Path)
				}
				answer, err := askEUOnly(os.Stdin, os.Stdout)
				if err != nil {
					return err
				}
				if err := config.SetTunnel(dir, url, answer); err != nil {
					return fmt.Errorf("could not save the answer: %w", err)
				}
				euOnly = &answer
			} else if url != "" {
				if err := config.SetTunnel(dir, url, *euOnly); err != nil {
					return err
				}
			}
			region := "any"
			if *euOnly {
				region = "eu"
			}
			if name == "" {
				name, _ = os.Hostname()
			}
			keys, err := tunnel.GenerateKeypair()
			if err != nil {
				return err
			}
			publicKey := tunnel.EncodeKey(keys.Public)
			code, err := api.RequestDeviceCode(ctx, name, region, publicKey)
			if err != nil {
				return explainProtocol(err)
			}
			fmt.Printf("\nOpen this page, sign in, and approve the code:\n\n  %s\n\n  code: %s\n  name: %s\n  key fingerprint: %s\n\n",
				code.VerificationURL, code.UserCode, name, tunnel.Fingerprint(publicKey))
			if !noBrowser {
				_ = client.OpenBrowser(code.VerificationURL)
			}
			fmt.Println("Waiting for the approval…")
			connectorID, token, err := waitForApproval(ctx, api, code)
			if err != nil {
				return err
			}
			if old, err := tunnel.LoadState(dir); err == nil && old.Token != "" && old.URL == base {
				// The previous connector is replaced: remove it from the account.
				_ = api.Delete(ctx, old.Token)
			}
			state := &tunnel.State{
				URL: base, ConnectorID: connectorID, Token: token, Region: region,
				PrivateKey: tunnel.EncodeKey(keys.Private), PublicKey: publicKey,
			}
			if err := state.Save(dir); err != nil {
				return err
			}
			where := "the nearest datacenter"
			if *euOnly {
				where = "EU datacenters only"
			}
			fmt.Printf("\nSigned in (%s). While \"codeflower client\" runs, this computer is reachable\nthrough the tunnel. Pair a browser with: codeflower tunnel pair\n", where)
			return nil
		},
	}
	command.Flags().StringVar(&url, "url", "", "the tunnel to use, saved in config.yaml (default "+tunnel.DefaultURL+")")
	command.Flags().StringVar(&name, "name", "", "this computer's name in your account (default: its host name)")
	command.Flags().BoolVar(&noBrowser, "no-browser", false, "print the approval page's address instead of opening a browser")
	return command
}

func waitForApproval(ctx context.Context, api *tunnel.API, code tunnel.DeviceCode) (string, string, error) {
	interval := time.Duration(max(code.Interval, 1)) * time.Second
	deadline := time.Now().Add(time.Duration(code.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(interval):
		}
		connectorID, token, err := api.ExchangeDeviceCode(ctx, code.DeviceCode)
		if errors.Is(err, tunnel.ErrPending) {
			continue
		}
		return connectorID, token, explainProtocol(err)
	}
	return "", "", errors.New("the code expired before it was approved; run the command again")
}

func tunnelPairCommand(directory func() string) *cobra.Command {
	command := &cobra.Command{
		Use:   "pair [sandbox…]",
		Short: "Pair a browser: prints a link (and a QR code) to open there",
		Long: `Pair a browser or phone with this computer: open the printed link there (or scan
the QR code). The link works once, for 10 minutes. Compare the key fingerprint
the page shows with the one printed here.

With "tunnel: {hardened: true}" name the sandboxes (by name or ID, as
"codeflower tunnel status" lists them) the browser may use; it sees no others.`,
		RunE: func(command *cobra.Command, args []string) error {
			dir := directory()
			loaded, err := config.Load(dir)
			if err != nil {
				return err
			}
			state, err := tunnel.LoadState(dir)
			if err != nil {
				return err
			}
			var workers []string
			if loaded.Tunnel.Hardened {
				running, err := client.RunningWorkers(dir)
				if err != nil {
					return err
				}
				if len(args) == 0 {
					return fmt.Errorf("name the sandboxes this browser may use (tunnel.hardened is on):\n%s", describeWorkers(running))
				}
				for _, arg := range args {
					id, err := resolveWorker(running, arg)
					if err != nil {
						return err
					}
					workers = append(workers, id)
				}
			} else if len(args) > 0 {
				return errors.New(`pairing a browser with chosen sandboxes needs "tunnel: {hardened: true}" in config.yaml`)
			}
			secret := randomSecret()
			err = tunnel.Update(dir, func(current *tunnel.State) error {
				current.Pairings = append(current.Pairings, tunnel.Pairing{
					SecretHash: tunnel.HashSecret(secret), Workers: workers, ExpiresAt: time.Now().Add(tunnel.PairingLifetime),
				})
				return nil
			})
			if err != nil {
				return err
			}
			link := tunnel.PairingLink(state, secret)
			printQR(os.Stdout, link)
			fmt.Printf("Open this link in the browser to pair (once, within 10 minutes):\n\n  %s\n\n", link)
			fmt.Printf("The page shows this computer's key fingerprint: %s\n", tunnel.Fingerprint(state.PublicKey))
			if _, running := client.Running(dir); !running {
				fmt.Println(`Start "codeflower client" before opening the link: it answers the pairing.`)
			}
			return nil
		},
	}
	return command
}

// printQR prints a QR code of `text` when `out` is a terminal; phones scan it
// to open the link.
func printQR(out io.Writer, text string) {
	file, ok := out.(*os.File)
	if !ok || !term.IsTerminal(file.Fd()) {
		return
	}
	qrterminal.GenerateHalfBlock(text, qrterminal.L, out)
	fmt.Fprintln(out)
}

func randomSecret() string {
	bytes := make([]byte, 32)
	_, _ = rand.Read(bytes)
	return base64.RawURLEncoding.EncodeToString(bytes)
}

func describeWorkers(workers []client.RemoteWorker) string {
	if len(workers) == 0 {
		return "  (no sandboxes in the running client)"
	}
	var lines []string
	for _, worker := range workers {
		lines = append(lines, fmt.Sprintf("  %-24s %s (%s)", worker.ID, worker.Label, worker.Agent))
	}
	return strings.Join(lines, "\n")
}

func resolveWorker(workers []client.RemoteWorker, name string) (string, error) {
	var matches []string
	for _, worker := range workers {
		if worker.ID == name {
			return worker.ID, nil
		}
		if strings.EqualFold(worker.Label, name) {
			matches = append(matches, worker.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no sandbox %q in the running client:\n%s", name, describeWorkers(workers))
	default:
		return "", fmt.Errorf("%q names several sandboxes; use an ID:\n%s", name, describeWorkers(workers))
	}
}

func tunnelStatusCommand(directory func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the tunnel connection, this computer's key and the tunnel's compatibility",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			dir := directory()
			loaded, err := config.Load(dir)
			if err != nil {
				return err
			}
			state, err := tunnel.LoadState(dir)
			if err != nil {
				return err
			}
			onOff := "on"
			if state.Off {
				onOff = `off ("codeflower tunnel on" turns it on)`
			}
			region := "nearest datacenter"
			if state.Region == "eu" {
				region = "EU datacenters only"
			}
			fmt.Printf("Tunnel:          %s\nConnector:       %s (%s)\nKey fingerprint: %s\nRemote access:   %s\n",
				state.URL, state.ConnectorID, region, tunnel.Fingerprint(state.PublicKey), onOff)
			fmt.Printf("Hardened:        %v\nManage:          %v\nPaired browsers: %d\n", loaded.Tunnel.Hardened, loaded.Tunnel.Manage, len(state.Devices))
			if _, running := client.Running(dir); running {
				fmt.Println("Client:          running (keeps the tunnel connected)")
				if workers, err := client.RunningWorkers(dir); err == nil {
					fmt.Printf("Sandboxes:\n%s\n", describeWorkers(workers))
				}
			} else {
				fmt.Println(`Client:          not running: start "codeflower client" to be reachable`)
			}
			api, err := tunnel.NewAPI(state.URL, Version)
			if err != nil {
				return err
			}
			info, err := api.VersionInfo(command.Context())
			switch {
			case err != nil:
				fmt.Printf("Compatibility:   unknown (%v)\n", err)
			case tunnel.Protocol < info.MinProtocol:
				fmt.Println(`Compatibility:   this code flower is too old for the tunnel: run "codeflower --update"`)
			case tunnel.Protocol > info.Protocol:
				fmt.Println("Compatibility:   the tunnel is older than this code flower: its operator must update it")
			default:
				if after, deprecated := info.DeprecatedFor(tunnel.Protocol); deprecated {
					fmt.Printf("Compatibility:   works until %s: update code flower before then (codeflower --update)\n", after)
				} else {
					fmt.Println("Compatibility:   ok")
				}
			}
			return nil
		},
	}
}

func tunnelDevicesCommand(directory func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "devices",
		Short: "List the paired browsers",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			state, err := tunnel.LoadState(directory())
			if err != nil {
				return err
			}
			if len(state.Devices) == 0 {
				fmt.Println(`No paired browsers. Pair one with "codeflower tunnel pair".`)
				return nil
			}
			for _, device := range state.Devices {
				workers := "all sandboxes"
				if len(device.Workers) > 0 {
					workers = strings.Join(device.Workers, ", ")
				}
				fmt.Printf("%s  %-28s paired %s  %s\n", tunnel.Fingerprint(device.Key), device.Label, device.PairedAt.Local().Format("2006-01-02 15:04"), workers)
			}
			return nil
		},
	}
}

func tunnelRevokeCommand(directory func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke DEVICE",
		Short: "Unpair a browser, by its name or fingerprint (as \"devices\" lists them); its open chats close at once",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			wanted := strings.ToLower(strings.ReplaceAll(args[0], " ", ""))
			var removed []string
			err := tunnel.Update(directory(), func(state *tunnel.State) error {
				var matches []tunnel.Device
				for _, device := range state.Devices {
					fingerprint := strings.ReplaceAll(tunnel.Fingerprint(device.Key), " ", "")
					if strings.EqualFold(device.Label, args[0]) || (len(wanted) >= 4 && strings.HasPrefix(fingerprint, wanted)) {
						matches = append(matches, device)
					}
				}
				if len(matches) == 0 {
					return fmt.Errorf("no paired browser %q (see \"codeflower tunnel devices\")", args[0])
				}
				if len(matches) > 1 {
					return fmt.Errorf("%q matches several browsers: use more of the fingerprint", args[0])
				}
				state.Devices = slices.DeleteFunc(state.Devices, func(device tunnel.Device) bool { return device.Key == matches[0].Key })
				removed = append(removed, matches[0].Label)
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Printf("Unpaired %q.\n", removed[0])
			return nil
		},
	}
}

func tunnelRotateCommand(directory func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "rotate",
		Short: "Give this computer a new key; every browser must pair again",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			dir := directory()
			state, err := tunnel.LoadState(dir)
			if err != nil {
				return err
			}
			api, err := tunnel.NewAPI(state.URL, Version)
			if err != nil {
				return err
			}
			keys, err := tunnel.GenerateKeypair()
			if err != nil {
				return err
			}
			publicKey := tunnel.EncodeKey(keys.Public)
			if err := api.SetKey(command.Context(), state.Token, publicKey); err != nil {
				return explainProtocol(err)
			}
			err = tunnel.Update(dir, func(current *tunnel.State) error {
				current.PrivateKey, current.PublicKey = tunnel.EncodeKey(keys.Private), publicKey
				current.Devices, current.Pairings = nil, nil
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Printf("New key fingerprint: %s. All browsers were unpaired; pair them again.\n", tunnel.Fingerprint(publicKey))
			return nil
		},
	}
}

func tunnelVerifyCommand(directory func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "verify",
		Short: "Check that the tunnel serves the browser code this code flower expects",
		Long: `The tunnel serves the small bootstrap code that browsers run before they reach
this computer. A compromised tunnel could serve other code there. This command
downloads it and compares it with the hashes built into this code flower.`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			dir := directory()
			loaded, err := config.Load(dir)
			if err != nil {
				return err
			}
			base := loaded.Tunnel.URL
			if state, err := tunnel.LoadState(dir); err == nil {
				base = state.URL
			}
			if base == "" {
				base = tunnel.DefaultURL
			}
			results, err := tunnel.VerifyBootstrap(command.Context(), base)
			for _, result := range results {
				fmt.Println(result)
			}
			return err
		},
	}
}

func tunnelSwitchCommand(directory func() string, use, short string, off bool) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := tunnel.Update(directory(), func(state *tunnel.State) error { state.Off = off; return nil }); err != nil {
				return err
			}
			if off {
				fmt.Println(`Remote access is off; open chats were closed. "codeflower tunnel on" turns it on again.`)
			} else {
				fmt.Println("Remote access is on while \"codeflower client\" runs.")
			}
			return nil
		},
	}
}

func tunnelLogoutCommand(directory func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Sign this computer out of the tunnel and forget its key and paired browsers",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			dir := directory()
			state, err := tunnel.LoadState(dir)
			if err != nil {
				return err
			}
			if api, err := tunnel.NewAPI(state.URL, Version); err == nil {
				if err := api.Delete(command.Context(), state.Token); err != nil {
					fmt.Fprintf(os.Stderr, "Could not remove this computer from your tunnel account (%v); revoke it on %s.\n", err, state.URL)
				}
			}
			if err := tunnel.RemoveState(dir); err != nil {
				return err
			}
			fmt.Println("Signed out.")
			return nil
		},
	}
}

// tunnelConnectCommand is the confined connector `codeflower client` starts
// with tunnel.hardened; not for users.
func tunnelConnectCommand(directory func() string) *cobra.Command {
	var version string
	command := &cobra.Command{
		Use:    "connect",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			dir := directory()
			notify := func(notice tunnel.Notice) {
				data, _ := json.Marshal(notice)
				fmt.Println(client.NoticePrefix + string(data))
			}
			base := tunnel.DefaultURL
			if state, err := tunnel.LoadState(dir); err == nil {
				base = state.URL
			} else if loaded, err := config.Load(dir); err == nil && loaded.Tunnel.URL != "" {
				base = loaded.Tunnel.URL
			}
			if err := tunnel.Confine(dir, base); err != nil {
				fmt.Fprintf(os.Stderr, "tunnel: could not confine the connector (%v); it runs unconfined\n", err)
				notify(tunnel.Notice{Title: "The tunnel connector is not confined", Body: "tunnel.hardened is on, but this system cannot confine the connector (Landlock on Linux 6.7 or later)."})
			}
			socket := tunnel.SocketPath(dir)
			return tunnel.Run(command.Context(), tunnel.Options{
				ConfigDirectory: dir,
				Version:         version,
				Connected:       func(pageURL string) { fmt.Println(client.ConnectedPrefix + pageURL) },
				DialApp:         func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "unix", socket) },
				Notify:          notify,
				Log:             os.Stderr,
			})
		},
	}
	command.Flags().StringVar(&version, "version", Version, "")
	return command
}
