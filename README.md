# Launcher

`codeflower` starts code flower sandboxes (workers): a container with the agent (Claude Code, Codex or
DeepSeek Harness) and the bridge VS Code connects to. It has a window for people who do not use a
terminal, and a command line for workstations and remote clusters. It is a single static Go binary
with no runtime dependencies.

## Install

Download it from the [releases](https://github.com/lutrarutra/codeflower-launcher/releases):
`codeflower-<os>-<arch>.tar.gz` (or `.zip` on Windows) holds the `codeflower` binary, and
`codeflower-macos.dmg` holds the macOS app. The builds are not signed yet: on macOS, right-click the app
and choose *Open* the first time; on Windows, choose *More info → Run anyway*. On a cluster:

```sh
curl -fsSL https://github.com/lutrarutra/codeflower-launcher/releases/latest/download/codeflower-linux-amd64.tar.gz | tar xz
```

Or build it with Go 1.26 or newer:

```sh
make launcher-build          # build/codeflower for this machine
make launcher-install        # or install it into $(go env GOPATH)/bin
make launcher-build-linux    # build/codeflower-linux-amd64 for an x86_64 cluster
make launcher-build-all      # Linux, macOS and Windows binaries, e.g. to hand out
```

## Container engines

The sandbox runs with Docker, Podman or Apptainer (formerly Singularity). `--engine auto`, the
default, uses the first that works: Docker, then Podman, then Apptainer. An image ending in `.sif`
always selects Apptainer.

```sh
codeflower check
```

lists the engines on this machine and checks that the sandbox image is there. When none is
installed or running, it prints instructions for this system: Docker Desktop on macOS, Windows and
WSL, Docker Engine or Podman for the Linux distribution, and Apptainer modules on clusters. The
window shows the same guide.

Docker is used through its API (a running daemon, no `docker` command needed); Podman and Apptainer
run as subprocesses. With Docker and Podman the container runs with the user's uid and publishes its
port on `127.0.0.1` only.

## The window

```sh
codeflower gui
```

opens the launcher in the browser; double-clicking the binary does the same on Windows and macOS.
It shows whether this computer can run sandboxes (and how to install an engine if not), its
processor and memory use (per-core bars as in htop, refreshed every second), then:

1. the agent: Claude Code, Codex or DeepSeek Harness;
2. the folders, added one by one with a folder browser: input folders with your data, mounted
   read-only in `/data/input`; output folders for results, read-write in `/data/output`; and code
   folders (repositories or packages the agent develops and runs), read-write in `/data/code`. Each
   folder appears under its own name, e.g. `/data/input/study42` (`study42-2` if two share a name);
3. under *Advanced*, the project name, the engine and the image.

*Start sandbox* starts it and shows the line to connect with, a *Copy* button and *Open in VS Code*,
which hands the line to the code flower extension (`vscode://codeflower.codeflower/connect`; VS Code
asks before connecting). Each running sandbox shows what its agent does: how many chats are
working, how many wait for you (a plan to approve, a permission or a question; the card is outlined
then), and whether VS Code is connected. Chats keep working when VS Code disconnects. Sandboxes run
while the launcher runs: *Stop all and quit*, or Ctrl+C in its
terminal, stops them. The window remembers the last choices in the user's config directory
(`codeflower/launcher.json`).

The page is served on `127.0.0.1` with a random port and a one-time key in the address that becomes
a cookie, so other users of the machine and other websites cannot use it.

## The command line

```sh
codeflower run --agent claude --input ~/data/study42 --output ~/results/study42
codeflower run --agent claude --input ~/data/rna --input ~/data/atac --code ~/src/mypkg
```

- `--agent`: `claude` (Claude Code), `codex` (Codex) or `dsh` (DeepSeek Harness). Asked for when
  left out on a terminal.
- `--input DIR`: your data, read-only at `/data/input/NAME`. `--output DIR`: created if needed,
  read-write at `/data/output/NAME`; the agent copies its finished report, figures and tables there.
  `--code DIR`: a repository or package the agent changes and runs, read-write at `/data/code/NAME`.
  All three can be repeated; NAME is the folder's own name.
- `--bind SOURCE[:TARGET[:ro|rw]]`: more mounts (repeatable). Without a target it maps to
  `/data/input/NAME`; binds are read-only unless `:rw` is given.
- `--project NAME` (default `default`): the project lives in `~/codeflower/projects/NAME`. Or give the
  directory with `--state DIR`. The project holds the sessions, the agent environment, the workspace,
  the agents' logins, caches, and connection files, created with owner-only permissions.
- `--engine` and `--image`: see above; `CODEFLOWER_ENGINE`, `CODEFLOWER_IMAGE` and `CODEFLOWER_AGENT`
  set defaults.

On a cluster, download the image once on the login node (compute nodes often have no internet), then
start the worker in a job:

```sh
codeflower pull --engine apptainer      # ~/codeflower/images/codeflower-sandbox_<version>.sif
codeflower run --agent claude \
  --project study42 \
  --input /data/study42 --output ~/results/study42
```

On start the worker prints the line to paste into VS Code ("code flower: Connect to worker"):

```
code flower worker ready (Claude Code). In VS Code, run "code flower: Connect to worker" and paste:

  codeflower://10.20.3.17:41234?token=…
```

The address defaults to the node's IPv4 address with Apptainer, and `127.0.0.1` with Docker and
Podman; `--advertise-host` overrides it. Anyone with this line can control the agent, so keep job logs
private, for example `sbatch --output=$HOME/.codeflower-logs/%j.out` with that directory `chmod 700`.
The same information is also written to a mode-`0600` file in `<project>/instances/` for scripts.

Under Slurm, wrap it in `sbatch` and add `--signal=B:TERM@120` so the worker can shut down cleanly
before walltime. Ctrl+C or SIGTERM stops the worker gracefully; Docker and Podman containers are
named `codeflower-<id>` and removed when they exit. `make dev-run AGENT=claude|codex|dsh` in
[codeflower-workspace](https://github.com/lutrarutra/codeflower-workspace) starts a Docker worker for
development with the state in `.dev-state/`.

## The sandbox image

The sandbox is published by [codeflower-sandbox](https://github.com/lutrarutra/codeflower-sandbox) as
`ghcr.io/lutrarutra/codeflower-sandbox`. Each launcher release uses the `major.minor` tag of the sandbox
it was tested with (`sandbox.ImageVersion`), so sandbox patch releases reach it without a new launcher.

- `codeflower run` and the window download the image when it is missing. `codeflower pull` downloads
  it, or updates it to the newest patch release of that tag.
- With Apptainer it is kept as `~/codeflower/images/codeflower-sandbox_<version>.sif`, or in
  `CODEFLOWER_IMAGE_DIR`. Jobs that start at the same time wait for one download.
- `--image` (or `CODEFLOWER_IMAGE`) uses another image, which is never downloaded automatically: for
  example `codeflower-sandbox` built locally with `make sandbox-build`, or a `.sif` file.

## Sign-in

There is no login command. After connecting, the VS Code extension offers the agent's own sign-in, which
runs inside the sandbox and stores the login in the project:

- Claude Code: a terminal into the sandbox runs `claude auth login` (subscription or Anthropic Console).
- Codex: a ChatGPT device code shown as a link, or an OpenAI API key.
- DeepSeek Harness: a DeepSeek API key and/or an OpenAI-compatible endpoint.

The sandbox needs internet access for sign-in and for model calls. Anthropic does not permit third-party
products to offer Claude.ai login, but does not prevent users from signing in to the unmodified Claude
Code binary with their own subscription; whether using it through code flower is covered is not settled,
so check that your use is permitted, or use an API key.

## Tests

```sh
make launcher-test   # go vet and go test; `make test` in codeflower-workspace runs every component
```

The local test environment does not include Apptainer or a cluster, so run the image under Apptainer and
verify login-node-to-compute-node connectivity before using it on a cluster.
