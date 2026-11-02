package attach

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"time"

	"codeflower-app/internal/client"
)

// Link modes (--links): auto chooses local on this computer and the tunnel
// over SSH; path prints no links.
const (
	LinksAuto   = "auto"
	LinksLocal  = "local"
	LinksTunnel = "tunnel"
	LinksPath   = "path"
)

// pageLinks is where the user's browser opens the sandbox's files and chats:
// the app's page on this computer, or through the tunnel. The links hold no
// secret, as the agent sees them in its tool's output; the page has its key
// from an earlier visit (the keyed link, shown once), or the tunnel pairs it.
type pageLinks struct {
	// base is the page, e.g. http://127.0.0.1:7720/ or https://…/m/<connector>/.
	base string
	// worker is the sandbox's id in the page's addresses.
	worker string
	// keyed is the local page with its key, for a browser that has not opened it yet.
	keyed string
	// note tells the user why there are no links, or how to reach them.
	note string
}

// fileBase is what the bridge adds a file's path to (`fileLinks` in its
// server.js; the page's `fileLink` reads it).
func (p pageLinks) fileBase() string {
	if p.base == "" {
		return ""
	}
	return p.base + "#/file/" + url.PathEscape(p.worker) + "/"
}

func (p pageLinks) chat(key string) string {
	if p.base == "" {
		return ""
	}
	return p.base + "#/chat/" + url.PathEscape(p.worker) + "/" + url.PathEscape(key)
}

// findPage finds the page of the client running for the config directory,
// and its address through the tunnel.
func findPage(mode, configDirectory string, target Target) pageLinks {
	if mode == LinksPath {
		return pageLinks{}
	}
	if target.Source == "line" {
		return pageLinks{note: "files show as paths: a sandbox reached by its line has no page here; add it in `codeflower client` for links"}
	}
	running, ok := client.Running(configDirectory)
	if !ok {
		return pageLinks{note: "files show as paths: run `codeflower client` for links that open them in your browser"}
	}
	page, err := url.Parse(running)
	if err != nil {
		return pageLinks{}
	}
	key := page.Query().Get("key")
	local := pageLinks{base: page.Scheme + "://" + page.Host + "/", worker: target.ID, keyed: running}
	overSSH := os.Getenv("SSH_CONNECTION") != ""
	if mode == LinksLocal || (mode == LinksAuto && !overSSH) {
		return local
	}
	tunnelPage := ""
	if !target.NoTunnel {
		tunnelPage = tunnelPageURL(local.base, key)
	}
	if tunnelPage != "" {
		return pageLinks{base: tunnelPage, worker: target.ID}
	}
	if mode == LinksTunnel {
		local.note = "remote access is off or not connected (codeflower tunnel pair): links open this computer's page"
	} else {
		local.note = fmt.Sprintf("links open this computer's page: forward it with `ssh -L %s:%s <this host>`, or turn on remote access (codeflower tunnel pair)", page.Port(), page.Host)
	}
	return local
}

// tunnelPageURL asks the client where paired browsers open its page.
func tunnelPageURL(base, key string) string {
	request, err := http.NewRequest(http.MethodGet, base+"api/tunnel", nil)
	if err != nil {
		return ""
	}
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
	if err != nil {
		return ""
	}
	defer response.Body.Close()
	var status struct {
		PageURL string `json:"pageUrl"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&status) != nil {
		return ""
	}
	return status.PageURL
}

// openBrowser opens a link in this computer's browser; over SSH there is
// none to open, so it says so.
func openBrowser(link string) error {
	if os.Getenv("SSH_CONNECTION") != "" {
		return fmt.Errorf("over SSH there is no browser here: open %s", link)
	}
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", link)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
	default:
		command = exec.Command("xdg-open", link)
	}
	return command.Start()
}

// hyperlink is text that opens a link where the terminal supports it (OSC 8).
func hyperlink(link, text string) string {
	return "\x1b]8;;" + link + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}
