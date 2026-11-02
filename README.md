<p align="center">
  <img src="assets/icon.png" width="112" height="112" alt="code flower app icon">
</p>

<h1 align="center">code flower app</h1>

<p align="center">Start code flower sandboxes from a window or the command line.</p>

<p align="center">
  <b><img src="assets/icon.png" width="20" height="20" alt=""> app</b>
  &nbsp;·&nbsp;
  <a href="https://github.com/lutrarutra/codeflower-sandbox"><img src="https://raw.githubusercontent.com/lutrarutra/codeflower-sandbox/main/assets/icon.png" width="20" height="20" alt=""> sandbox</a>
  &nbsp;·&nbsp;
  <a href="https://github.com/lutrarutra/codeflower-vsc-extension"><img src="https://raw.githubusercontent.com/lutrarutra/codeflower-vsc-extension/main/media/icon.png" width="20" height="20" alt=""> VS Code extension</a>
  &nbsp;·&nbsp;
  <a href="https://github.com/lutrarutra/storyteller"><img src="https://raw.githubusercontent.com/lutrarutra/storyteller/main/assets/icon.png" width="20" height="20" alt=""> storyteller</a>
  &nbsp;·&nbsp;
  <a href="https://github.com/lutrarutra/codeflower-tunnel"><img src="https://raw.githubusercontent.com/lutrarutra/codeflower-tunnel/main/assets/icon.png" width="20" height="20" alt=""> tunnel</a>
  &nbsp;·&nbsp;
  <a href="https://docs.codeflower.app">docs.codeflower.app</a>
</p>

`codeflower` starts code flower sandboxes (workers): a container with an agent (Claude Code, Codex,
DeepSeek Harness or OpenCode) that sees only the folders you give it, and that you chat with from VS Code or the
browser. It is a single static Go binary with a window for people who do not use a terminal and a
command line for workstations and clusters.

## Install

Linux (also on clusters) and macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/lutrarutra/codeflower-app/main/install.sh | sh
```

Windows, in PowerShell:

```powershell
irm https://raw.githubusercontent.com/lutrarutra/codeflower-app/main/install.ps1 | iex
```

The scripts check the release's checksum, show a usage disclaimer you confirm with `y`, and install
into `~/.local/bin` (Windows: `%LOCALAPPDATA%\Programs\codeflower`, added to PATH). They add
*code flower* to the applications or Start menu. `CODEFLOWER_INSTALL_DIR` picks another folder and
`CODEFLOWER_VERSION=vX.Y.Z` another release. To uninstall, delete the binary (on Windows, the folder
and the Start menu entry).

The [releases](https://github.com/lutrarutra/codeflower-app/releases) also have the binaries
(`codeflower-<version>-<os>-<arch>.tar.gz`, `.zip` on Windows) and a macOS app
(`codeflower-<version>-macos.dmg`). They are not signed yet: on macOS right-click the app and choose
*Open* the first time; on Windows choose *More info → Run anyway*.

Then:

```sh
codeflower check     # can this machine run sandboxes? how to set it up if not
codeflower client    # the window: start sandboxes, connect to them, chat
codeflower interactive --agent claude --input ~/data/study42 --output ~/results/study42
```

The sandbox needs Docker, Podman or Apptainer. `interactive` prints a `codeflower://…` line: paste it
into VS Code ("code flower: Connect to worker"), or open the printed link while `codeflower client`
runs. After connecting, VS Code or the page offers the agent's [sign-in](#sign-in).

## Commands

| Command | |
| --- | --- |
| [`client`](#codeflower-client) | the window: start sandboxes here, connect to sandboxes elsewhere, chat |
| [`interactive`](#codeflower-interactive) | start a sandbox to work with from VS Code or the browser |
| [`run`](#codeflower-run) | the same, with a first task already sent to a new chat |
| [`task`](#codeflower-task) | let the agent work on a task by itself, then stop (batch jobs) |
| [`pull`](#codeflower-pull) | download or update the sandbox image |
| [`check`](#codeflower-check) | check the container engines and the image |
| [`update`](#codeflower-update) | replace this program with the latest release |
| [`tunnel`](#codeflower-tunnel) | reach this computer's sandboxes from any browser or phone |

`codeflower COMMAND --help` lists every flag.

### `codeflower client`

Opens code flower in the browser (also `codeflower gui`, or double-click the binary on Windows and
macOS). It lists your sandboxes, shows whether this computer can run them and its CPU and memory use,
and starts new ones: pick the agent, add input, output and code folders, and *Start sandbox*. *Open in
VS Code* hands the sandbox to the extension; or chat in the page itself. Each sandbox shows how many
chats are working and how many wait for you.

To use a sandbox running elsewhere (e.g. in a batch job), paste the `codeflower://…` line it printed.
The client keeps it in `codeflower/workers.json` in the user's config directory; the gear on a
sandbox renames it, changes its address (e.g. when the job restarted on another node) or removes it
from the list, which does not stop it.

| Flag | |
| --- | --- |
| `--port N` | port to serve on (default 7720, or any free port when taken; `0` any free port) |
| `--address ADDR` | full listener address, instead of `--port` |
| `--connect-only` | never start sandboxes here, e.g. on a cluster's login node |
| `--no-browser` | print the address instead of opening a browser |
| `--no-tray` | no icon in the system tray or menu bar |
| `--no-notifications` | no desktop notifications when a chat waits for you or ends its turn |

- The page is served on `127.0.0.1` only; a one-time key in its address becomes a cookie. On a remote
  machine, forward the port (`ssh -N -L 7720:localhost:7720 login-node`; the client prints the
  command when you are logged in over SSH).
- Starting the client while it runs opens the running one.
- Quitting (*Stop all and quit*, *Quit* in the tray, Ctrl+C) asks the working chats in the sandboxes
  it started to save their results and update their reports, then stops each one, at most after 10
  minutes. *Stop now* (or Ctrl+C again) stops them at once; *Keep running* calls the quit off.
  Sandboxes added by their line or started from the command line keep running.
- Notifications need a notification service on Linux, the macOS app on macOS, and the tray icon on
  Windows. A click opens the sandbox in VS Code. The tray icon needs a system tray (Windows, macOS,
  KDE, XFCE, Cinnamon, or GNOME with the AppIndicator extension).

### `codeflower interactive`

Starts a sandbox for a project and keeps it running until Ctrl+C or SIGTERM.

```sh
codeflower interactive --agent claude --input ~/data/study42 --output ~/results/study42
codeflower interactive --agent codex --project study42 --input ~/data/rna --code ~/src/mypkg --isolate-network
```

| Flag | |
| --- | --- |
| `--agent` | `claude` (Claude Code), `codex` (Codex), `dsh` (DeepSeek Harness) or `opencode` (OpenCode); asked for on a terminal |
| `-i, --input DIR` | your data, read-only at `/data/input/NAME` |
| `-o, --output DIR` | results, created if needed, read-write at `/data/output/NAME` |
| `-c, --code DIR` | a repository or package the agent changes and runs, read-write at `/data/code/NAME` |
| `-s, --skills DIR` | a skill (a folder with `SKILL.md`) or a folder of skills, read-only at `/data/skills/NAME` |
| `--bind SRC[:DST[:ro\|rw]]` | another mount; read-only at `/data/input/NAME` by default |
| `-p, --project NAME` | the project in `~/codeflower/projects/NAME` (default `default`) |
| `--state DIR` | the project directory, instead of `--project` |
| `--isolate-network` | no network except the agent's service and the [allowlist](#network-isolation) |
| `--allowlist FILE` | the allowlist (default `PROJECT.allowed-hosts.txt` next to the project) |
| `--cpus`, `--memory 32G`, `--time-limit 8h` | resources the agent is told it has (default: the batch job's); Docker and Podman enforce CPUs and memory |
| `--port`, `--host`, `--advertise-host` | where the worker listens and the address it prints |
| `--server` | also serve the chat page and print a link to it, as Jupyter does |
| `--no-tunnel` | keep this sandbox from browsers paired through the [tunnel](#codeflower-tunnel) |
| `--engine`, `--image`, `--image-tag` | see [Container engines and the image](#container-engines-and-the-image) |
| `--config DIR` | the config directory (default `~/.codeflower`) |

`--input`, `--output`, `--code`, `--skills` and `--bind` can be repeated; NAME is the folder's own
name (`study42-2` if two share one). The project holds the sessions, the agent's environment and
logins, the workspace and caches, with owner-only permissions; several sandboxes can share it.

### `codeflower run`

```sh
codeflower run --agent claude --input ~/data/study42 --output ~/results/study42 \
  "Analyse the data and generate a report"
```

Like `interactive`, with the same flags, but sends the task (the argument, `-` for standard input, or
`--prompt FILE`) as the first prompt of a new chat. Connect from VS Code or the page to follow it,
answer its questions and continue the chat. If the project is not signed in yet, the task waits until
you connect and sign in.

### `codeflower task`

Lets the agent work on a task by itself, without VS Code, until it is done, e.g. as a batch job:

```sh
codeflower task --agent claude --project study42 --input /data/study42 --output ~/results/study42 \
  "Cluster the cells and annotate the cell types"
sbatch --time=8:00:00 --signal=B:TERM@120 --wrap "codeflower task --agent claude --project study42 \
  --input /data/study42 --output ~/results/study42 --timeout 7h50m --prompt task.md"
```

It takes the flags of `interactive` (without `--server` and the listening ones); `--output` is
required and the first one gets the results. The agent gets the task as its goal and is told nobody
will answer: it decides open questions itself and records them as assumptions. It builds a storyteller
report as it works, delivered as `report.html` in the output folder.

| Flag | |
| --- | --- |
| `--prompt FILE` | read the task from a Markdown file (or pass `-` to read standard input) |
| `--model`, `--effort` | e.g. `--model opus --effort high` (Claude Code), `--model gpt-6.1-sol --effort xhigh` (Codex); default: the agent's |
| `--option ID=VALUE` | any other setting of the agent, e.g. `fast=on` |
| `--max-rounds N` | how often the agent is sent back to work when it has not finished (default 3) |
| `--timeout 90m` | stop after this long |
| `--skip-report` | ask for a `README.md` instead of a report |
| `--output-mode` | `auto` (a progress view in a terminal, raw output when redirected), `raw` or `attached` |
| `--continue TASK_DIR` | continue a task (below) |

Before it stops, when the rounds run out or shortly before `--timeout`, the agent gets one more turn
to bring the report up to date with what it did, found and left open. Exit status: `0` done, `2` not
done, `3` not signed in, `130` interrupted or timed out, `1` other errors.

Sign in once with `interactive` first. From an interactive terminal, `task` can start the agent's
terminal sign-in itself. The run is a session of the project, so VS Code lists it afterwards.

**Continuing.** The task's folder (`<project>/tasks/<id>/`) is printed when it starts, and the command
to continue it when it ends:

```sh
codeflower task --continue ~/codeflower/projects/study42/tasks/1a2b3c --timeout 7h50m
codeflower task --continue ~/codeflower/projects/study42/tasks/1a2b3c "Also compare the two donors"
```

The agent continues in the same session and is told why the last run stopped (interrupted, an error
such as a usage limit, out of rounds or time); a finished task is checked and improved. A follow-up
asks for more work on the same report. The project, agent, folders and network stay as they were;
`--max-rounds`, `--timeout`, `--model`, `--effort`, `--option`, `--engine` and `--image` may change.

### `codeflower pull`

Downloads the sandbox image, or updates a moving tag (`latest`, `0.2`) to its newest build. The
other commands download it on first use; on a cluster, run `codeflower pull --engine apptainer` once
on the login node, as compute nodes often have no internet.

### `codeflower check`

Lists the container engines on this machine and checks that the image is there. When none works, it
prints how to install one here: Docker Desktop on macOS, Windows and WSL, Docker Engine or Podman on
Linux, Apptainer modules on clusters.

### `codeflower update`

Replaces the installed binary with the latest release after checking its checksum; `--check` only says
whether one is out. Running clients and sandboxes keep the old version until restarted. The macOS app
is updated from a new `.dmg`.

### `codeflower tunnel`

Reaches the sandboxes on this computer from any browser or phone through a
[code flower tunnel](https://github.com/lutrarutra/codeflower-tunnel), end-to-end encrypted.

```sh
codeflower tunnel pair     # open the printed link (or QR code) in the browser to pair
```

Pairing signs this computer in when needed and starts `codeflower client` in the background, which
keeps the tunnel connected and lists every sandbox on this computer.

| Subcommand | |
| --- | --- |
| `pair` | pair a browser |
| `devices` | list the paired browsers |
| `revoke NAME` | unpair a browser; its open chats close at once |
| `off`, `on` | turn remote access off at once (pairings are kept), or on again |
| `status` | the connection, this computer's key and the tunnel's compatibility |
| `login`, `logout` | sign this computer in to the tunnel, or out (forgetting its key and pairings) |
| `rotate` | give this computer a new key; every browser must pair again |
| `verify` | check that the tunnel serves the browser code this version expects |

## Settings

`~/.codeflower/config.yaml` (or `--config DIR`, `$CODEFLOWER_CONFIG`) keeps your defaults. Every key
is optional and a mistyped key is an error. Flags beat environment variables, which beat the file.

```yaml
engine: auto               # --engine
image: ""                  # --image
isolate_network: false     # --isolate-network
skills: [~/more-skills]    # more skills folders to mount in every sandbox
agents:                    # claude, codex, dsh or opencode
  claude:
    model: opus            # for `codeflower task`, unless --model is given
    effort: high           # likewise --effort
    options: { fast: "on" }   # likewise --option
  codex:
    api_key_file: ~/.codeflower/keys/openai   # a file holding the key (chmod 600)
  dsh:
    api_key_env: DEEPSEEK_API_KEY             # or a host environment variable
icons:
  folder: ~/.codeflower/icons   # the icon gallery (the default); "off" mounts none
images:                    # lets the agent generate icons it cannot find
  provider: openai         # or google
  model: gpt-image-1       # optional
  api_key_env: OPENAI_API_KEY
mcp_servers:               # MCP servers every agent may use, by name
  docs:
    url: https://mcp.example.org/mcp
    transport: http        # the default; or sse (Claude Code and OpenCode only)
    headers: { Authorization: "Bearer …" }
  files:
    command: files-mcp     # runs inside the sandbox; found in its PATH
    args: [--root, /workspace]
    env: { TOKEN: "…" }
plugins:                   # each agent's own plugins, in every project (Claude Code for now)
  claude:
    - source: marketplace:commit-commands@claude-plugins-official
    - source: github:my-org/my-plugin#v1.2.0
client:
  connect_only: false      # --connect-only
  tray: true               # false: --no-tray
  notifications: true      # false: --no-notifications
  wrap_up_minutes: 10      # how long quitting waits for chats to wrap up
tunnel:
  url: ""                  # your own tunnel; the hosted one when empty
  eu_only: true            # route only through EU datacenters (asked at first login)
  hardened: false          # limit each browser to the sandboxes chosen when pairing it (Linux)
  manage: false            # let paired browsers start sandboxes and browse folders
```

| Environment variable | |
| --- | --- |
| `CODEFLOWER_AGENT`, `CODEFLOWER_ENGINE`, `CODEFLOWER_IMAGE`, `CODEFLOWER_IMAGE_TAG` | defaults for `--agent`, `--engine`, `--image`, `--image-tag` |
| `CODEFLOWER_CONFIG` | the config directory |
| `CODEFLOWER_IMAGE_DIR` | where Apptainer images are kept (default `~/codeflower/images`) |

- **Skills.** `~/.codeflower/skills`, when it exists, and the folders in `skills:` are mounted
  read-only in every sandbox, like `--skills`.
- **Icons.** The icon gallery is mounted read-write at `/data/icons` in every sandbox, so projects
  share the icons you imported for schematic figures (e.g. Servier Medical Art, SciDraw), with their
  licenses. code flower ships and downloads no icons. With `images:` the agent can also generate one
  after asking you.
- **API keys.** Per agent and for `images:`, give `api_key_env`, `api_key_file` or `api_key`. A file
  holding a key must not be readable by others (`chmod 600`). The agent can read its key inside the
  sandbox; `--isolate-network` limits where it can send it.
- **MCP servers.** Each chat's agent gets the servers in `mcp_servers:`, through ACP (all four
  agents take them that way). A `command` runs inside the sandbox, so it must be installed there,
  for example in the project's environment. A server the agent cannot use (sse for Codex and dsh, a
  command not found, one that does not start) is left out, and the chat says so. With `--isolate-network`, the hosts
  of `url` servers are reachable like the agent's service, unless they are on a private network:
  add those to the allowlist file. A file whose servers have `env` or
  `headers` must not be readable by others (`chmod 600`), as these usually hold tokens; the agent
  can read them inside the sandbox.
- **Plugins.** Each agent's own plugins, by agent: a `marketplace:NAME@CATALOG`, `github:OWNER/REPO[#REF]`,
  `git:URL[#REF]` or `path:/…` source for Claude Code, with `enabled: false` to list one switched off. A
  project adds its own and switches these off in the chat's *Plugins…* or with `codeflower plugins`. They
  are fetched with the agent's own installer before a sandbox starts, also for `--isolate-network`, whose
  fetch reaches only the hosts the installer needs; `codeflower plugins fetch` does it ahead, e.g. on a
  login node. Details in [plugins.md](https://github.com/lutrarutra/codeflower-workspace/blob/main/docs/plugins.md).
- **Model settings** (`model`, `effort`, `options`) apply to `codeflower task`; in a chat you choose
  them there.
- The client also keeps its last choices (`codeflower/app.json`) and added sandboxes
  (`codeflower/workers.json`) in the user's config directory.

## On a cluster

Download the image on the login node, then start the worker in a job:

```sh
codeflower pull --engine apptainer
sbatch --cpus-per-task=8 --mem=32G --time=8:00:00 --signal=B:TERM@120 \
  --output=$HOME/.codeflower-logs/%j.out \
  --wrap "codeflower interactive --agent claude --project study42 --input /data/study42 --output ~/results/study42"
```

The worker prints a link for `codeflower client` on your computer and the line for VS Code:

```
code flower worker ready (Claude Code). With the code flower app running on your computer, open:

  http://127.0.0.1:7720/#add=codeflower%3A%2F%2F10.20.3.17%3A41234%3Ftoken%3D…

Or in VS Code on this computer, or connected to it over Remote-SSH, open:

  vscode://codeflower.codeflower/connect?address=codeflower%3A%2F%2F127.0.0.1%3A41234%3Ftoken%3D…

From VS Code elsewhere, run "code flower: Connect to worker" and paste:

  codeflower://10.20.3.17:41234?token=…
```

- Anyone with the line or the link controls the agent: keep job logs private (`chmod 700` the log
  folder). The same is written to a `0600` file in `<project>/instances/` for scripts.
- `--signal=B:TERM@120` lets the worker shut down cleanly before walltime.
- The app tells the agent what the job may use: CPUs, memory, GPUs and when it ends, read from Slurm,
  PBS Pro/OpenPBS/Torque, LSF, Grid Engine or HTCondor. `--cpus`, `--memory` and `--time-limit`
  override them.
- The printed address is the node's IPv4 address with Apptainer and `127.0.0.1` with Docker and
  Podman; `--advertise-host` overrides it.
- With `--server`, the sandbox serves the chat page itself and prints
  `http://node042.cluster.example:8123/?key=…` to open in any browser that reaches the node. It is
  plain HTTP; on a shared network use `--host 127.0.0.1` and `ssh -L 8123:127.0.0.1:8123 node042`.

## Container engines and the image

`--engine auto` (the default) uses the first engine that works: Docker (through its API, no `docker`
command needed), then Podman, then Apptainer (formerly Singularity). An image ending in `.sif` always
selects Apptainer. Docker and Podman run the container with your uid, publish its port on `127.0.0.1`
only, name it `codeflower-<id>` and remove it when it exits.

The image is `ghcr.io/lutrarutra/codeflower-sandbox`, from
[codeflower-sandbox](https://github.com/lutrarutra/codeflower-sandbox). Each app release runs the
sandbox release it was tested with (`codeflower --help` shows it under `--image-tag`).

- `--image-tag TAG` uses another build: `latest`, a release (`0.2.1`, `v0.2.1`, or `0.2` for its
  newest patch) or a commit (`sha-abc1234`).
- `--image` uses another image, which is never downloaded: e.g. one built with `make sandbox-build`,
  or a `.sif` file.
- Apptainer images are kept as `~/codeflower/images/codeflower-sandbox_v<version>-sha-<commit>.sif`.
  Jobs that start at the same time wait for one download.
- `codeflower client` alone, connecting to sandboxes elsewhere, downloads nothing.

## Network isolation

With `--isolate-network` the sandbox has no network of its own. The app is its only way out and lets
it reach only its agent's service and the hosts in the allowlist file, printing what it refuses.

- One host per line, `*.example.org` for subdomains, `example.org:8443` for another port. Edits apply
  at once.
- `@NAME` allows every host of a service: `@pypi`, `@conda`, `@cran`, `@bioconductor`, `@npm`,
  `@huggingface`, `@github`, `@ncbi`, `@ensembl`, `@uniprot`, `@ebi`. A new file lists them.
- The agent is told how this works and asks you for what it needs, e.g. `@pypi`.
- It needs no administrator rights: Docker and Podman on Linux, or an Apptainer that lets users run
  `apptainer exec --net --network none` (`codeflower check` says). Docker Desktop on macOS and Windows
  cannot do it.

## Sign-in

There is no login command. After connecting, VS Code or the page offers the agent's own sign-in, which
runs inside the sandbox and stores the login in the project:

- Claude Code: a terminal into the sandbox runs `claude auth login` (subscription or Anthropic Console).
- Codex: a ChatGPT device code shown as a link, or an OpenAI API key.
- DeepSeek Harness: a DeepSeek API key and/or an OpenAI-compatible endpoint.
- OpenCode: a terminal into the sandbox runs `opencode auth login` (a ChatGPT subscription, or a key for
  OpenAI, Anthropic, OpenRouter, OpenCode Zen and others). A model on the cluster goes into
  `harness/opencode/opencode.jsonc` in the project.

Or set an API key in [Settings](#settings) (not for OpenCode, which needs to know the key's provider). The sandbox needs internet access for sign-in and model
calls (with `--isolate-network`, through the app, and the node's `HTTPS_PROXY` if it has one).
Anthropic does not permit third-party products to offer Claude.ai login, but does not prevent users
from signing in to the unmodified Claude Code binary with their own subscription; whether using it
through code flower is covered is not settled, so check that your use is permitted, or use an API key.

## Development

Build with Go 1.26 or newer:

```sh
make app-build          # build/codeflower for this machine
make app-install        # into $(go env GOPATH)/bin
make app-build-linux    # build/codeflower-linux-amd64 for an x86_64 cluster
make app-build-all      # Linux, macOS and Windows binaries
make app-test           # go vet and go test
```

To work on it with the other components, clone
[codeflower-workspace](https://github.com/lutrarutra/codeflower-workspace), which has this repo as
the `app/` submodule; `make dev-run AGENT=claude|codex|dsh|opencode` there starts a development worker. How the
parts fit together is in its [architecture document](https://github.com/lutrarutra/codeflower-workspace/blob/main/docs/architecture.md#how-code-flower-fits-together).
The tests do not cover Apptainer or a cluster: try the image under Apptainer, and the connection from
the login node to a compute node, before relying on it there.
