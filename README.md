# Launcher

`codeflower` starts code flower sandboxes (workers): a container with the agent (Claude Code, Codex or
DeepSeek Harness) and the bridge VS Code connects to. It has a window for people who do not use a
terminal, and a command line for workstations and remote clusters. It is a single static Go binary
with no runtime dependencies.

## Install

On Linux (also on clusters) and macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/lutrarutra/codeflower-launcher/main/install.sh | sh
```

It installs the latest release into `~/.local/bin` after checking its checksum;
`CODEFLOWER_INSTALL_DIR` chooses another folder and `CODEFLOWER_VERSION=vX.Y.Z` another release.
Before downloading or installing, the script displays a safety and usage disclaimer and requires
you to type `y` and press Enter. It needs a terminal for this confirmation and cancels otherwise.

The [releases](https://github.com/lutrarutra/codeflower-launcher/releases) also have each build
for download: `codeflower-<os>-<arch>.tar.gz` (`.zip` on Windows) with the `codeflower` binary, and
`codeflower-macos.dmg` with the macOS app. The builds are not signed yet: on macOS, right-click the
app and choose *Open* the first time; on Windows, choose *More info → Run anyway*.

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
client shows the same guide.

Docker is used through its API (a running daemon, no `docker` command needed); Podman and Apptainer
run as subprocesses. With Docker and Podman the container runs with the user's uid and publishes its
port on `127.0.0.1` only.

## The client

```sh
codeflower client
```

opens code flower in the browser (`codeflower gui` still works); double-clicking the binary does the
same on Windows and macOS. At the top it lists your sandboxes. To use one running elsewhere, for
example in a batch job, paste the `codeflower://…` line it printed: the client keeps it in the user's
config directory (`codeflower/workers.json`, readable only by you), shows whether the sandbox
answers, and connects to it again on the next start. Removing such a sandbox from the list does not
stop it.

Below, it shows whether this computer can run sandboxes (and how to install an engine if not; the
client still connects to sandboxes elsewhere), its processor and memory use (per-core bars as in
htop, refreshed every second), then:

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
then), and whether VS Code is connected. Chats keep working when VS Code disconnects. *Isolate the
network* does what `--isolate-network` does (below), with the project's allowlist file shown on the
sandbox's card; the client says when this computer cannot isolate, as with Docker on macOS.
The client remembers the last choices in the user's config directory (`codeflower/launcher.json`).

`codeflower client --connect-only` (or `client: {connect_only: true}` in `config.yaml`) never starts
sandboxes and does not look for an engine, e.g. on a cluster's login node.

The page is served on `127.0.0.1` with an automatically assigned port by default. Choose a fixed
port with `codeflower client --port 8080`, or use `--port 0` for automatic selection. The advanced
`--address` option sets the full listener address and cannot be combined with `--port`. A one-time
key in the address becomes a cookie, so other users of the machine and other websites cannot use it.
On a remote machine, forward the port (`ssh -N -L 8080:localhost:8080 login-node`; the client prints
the command when you are logged in over SSH) and open the printed address with `localhost:8080`.

Pages talk to a sandbox's agent through the client: `GET /api/workers/{id}/acp` is a WebSocket
that the client passes on to the sandbox's bridge with its token, as browsers cannot send the
bridge's `Authorization` header. This is what a web version of the chat will use.

## The command line

```sh
codeflower interactive --agent claude --input ~/data/study42 --output ~/results/study42
Where the desktop has a system tray, the client also shows an icon there: in the notification area
on Windows, in the menu bar on macOS, and on Linux desktops with a StatusNotifierItem host (KDE,
XFCE, Cinnamon, or GNOME with the AppIndicator extension; stock GNOME, WSL and SSH sessions have
none). A click on it, or *Open code flower*, opens the page again after its tab was closed. Started
by a double-click, the icon replaces the console window, and the client logs to `client.log` in the
config directory. `--no-tray` (or `client: {tray: false}`) turns the icon off. Starting the client
again while it runs (`client.json` in the config directory, readable only by you, holds its address)
opens the running one instead.

When a chat in a sandbox the client is connected to asks you a question, wants a permission or a
plan approved, or ends its turn, the client tells the desktop; a click on the notification opens the
sandbox in VS Code. On Linux this needs a desktop with a notification service (GNOME, KDE and the
others have one), with or without the tray; on macOS it needs the app (`code flower.app`, which asks
once whether to allow them) and on Windows the tray icon. Turns you stop yourself, and the wrap-up
when quitting, are not told. `--no-notifications` (or `client: {notifications: false}`) turns them
off.

Sandboxes started here run while the client runs. Quitting it (*Stop all and quit* on the page,
*Quit* in the tray, or Ctrl+C in its terminal) first says what it will do, then asks the chats
working in those sandboxes to save their results, bring their reports up to date and write down how
to continue, and stops each sandbox when its chats are done, at most after 10 minutes
(`client: {wrap_up_minutes: N}`). *Stop now* (or Ctrl+C again) stops them at once; *Keep running*
calls the quit off, and chats already asked finish their wrap-up. Sandboxes added by their address,
and those started with `codeflower run`, `task` or `interactive`, keep running. A sandbox whose image
is older than wrap-ups on request is stopped with the others without one.

codeflower interactive --agent claude --input ~/data/rna --input ~/data/atac --code ~/src/mypkg
```

- `--agent`: `claude` (Claude Code), `codex` (Codex) or `dsh` (DeepSeek Harness). Asked for when
  left out on a terminal.
- `--input DIR`: your data, read-only at `/data/input/NAME`. `--output DIR`: created if needed,
  read-write at `/data/output/NAME`; the agent copies its finished report, figures and tables there.
  `--code DIR`: a repository or package the agent changes and runs, read-write at `/data/code/NAME`.
  `--skills DIR`: a skill (a folder with a `SKILL.md`) or a folder of skills, read-only at
  `/data/skills/NAME`; each skill is installed for the agent (Claude Code, Codex and dsh).
  All four can be repeated; NAME is the folder's own name.
- `--isolate-network`: the sandbox gets no network of its own. The launcher is its only way out and lets
  it reach only its agent's service and the hosts in the allowlist file, refusing (and printing) the rest.
  `--allowlist FILE` names the file (default: `PROJECT.allowed-hosts.txt` next to the project
  directory, created with an explanation). One host per line, `*.example.org` for subdomains,
  `example.org:8443` for another port; edits apply immediately. A line `@NAME` allows every host of
  one service: `@pypi`, `@conda`, `@cran`, `@bioconductor`, `@npm`, `@huggingface`, `@github`,
  `@ncbi`, `@ensembl`, `@uniprot`, `@ebi` (the new file lists them with their hosts). The agent is told
  how this works and asks you for what it needs, e.g. `@pypi` to install packages.
  It needs no administrator rights: Docker and Podman on Linux, or an Apptainer that lets users run
  `apptainer exec --net --network none` (the launcher checks and says so). Docker Desktop on macOS
  and Windows cannot share the launcher's sockets with the sandbox, so it does not work there.
- `--bind SOURCE[:TARGET[:ro|rw]]`: more mounts (repeatable). Without a target it maps to
  `/data/input/NAME`; binds are read-only unless `:rw` is given.
- `--project NAME` (default `default`): the project lives in `~/codeflower/projects/NAME`. Or give the
  directory with `--state DIR`. The project holds the sessions, the agent environment, the workspace,
  the agents' logins, caches, and connection files, created with owner-only permissions.
- `--config DIR`: the config directory (default `~/.codeflower`, or `$CODEFLOWER_CONFIG`); see
  [Settings](#settings).
- `--engine`, `--image` and `--image-tag`: see above and [The sandbox image](#the-sandbox-image);
  `CODEFLOWER_ENGINE`, `CODEFLOWER_IMAGE`, `CODEFLOWER_IMAGE_TAG` and `CODEFLOWER_AGENT` set defaults.

Start a new chat session with an initial task and keep it available for follow-up in VS Code:

```sh
codeflower run --agent claude --input ~/data/study42 --output ~/results/study42 \
  "Analyse the data and generate a report"
```

`run` takes the same folder, project, network and engine options as `interactive`. It sends the
task (the argument, `-` to read it from standard input, or `--prompt FILE` to read it from a Markdown
file) as the first prompt in a newly created session, then keeps the worker running
so VS Code can attach while the agent works and continue the chat afterwards. The task is an ordinary
interactive turn: the extension handles questions and permissions. Sign in with `interactive` first
if this project has not been signed in yet. `run` replaces its former use as an alias of
`interactive`; use `interactive` for a worker without an initial prompt.

On a cluster, download the image once on the login node (compute nodes often have no internet), then
start the worker in a job:

```sh
codeflower pull --engine apptainer      # ~/codeflower/images/codeflower-sandbox_<version>.sif
codeflower interactive --agent claude \
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
before walltime. The launcher tells the agent what the job may use: it reads the allocation of a
Slurm, PBS Pro/OpenPBS/Torque, LSF, Grid Engine or HTCondor job (CPUs, memory, GPUs and when the job
ends, as far as the scheduler gives them), and `--cpus`, `--memory 32G` and `--time-limit 8h` replace
those figures or give them outside a job. Docker and Podman also enforce `--cpus` and `--memory`;
under Apptainer the scheduler does. Ctrl+C or SIGTERM stops the worker gracefully; Docker and Podman containers are
named `codeflower-<id>` and removed when they exit. `make dev-run AGENT=claude|codex|dsh` in
[codeflower-workspace](https://github.com/lutrarutra/codeflower-workspace) starts a Docker worker for
development with the state in `.dev-state/`.

## Settings

`~/.codeflower/config.yaml` keeps your defaults between runs. Everything in it is optional; a
mistyped key is an error. A flag or an environment variable beats the file, and the file beats the
built-in default.

```yaml
engine: auto               # as --engine
image: ""                  # as --image
isolate_network: false     # as --isolate-network
skills: [~/more-skills]    # more skills folders to always mount (relative paths: to this directory)
agents:                    # claude, codex or dsh
  claude:
    model: opus            # for `codeflower task`, unless --model is given
    effort: high           # likewise --effort
    options: { fast: "on" }   # likewise --option
  codex:
    api_key_file: ~/.codeflower/keys/openai   # a file holding the key (chmod 600)
  dsh:
    api_key_env: DEEPSEEK_API_KEY             # or the name of a host environment variable
```

- The folder `~/.codeflower/skills` is mounted read-only in every sandbox when it exists, like
  `--skills ~/.codeflower/skills`, as are the folders in `skills:`. The client lists them.
- `model`, `effort` and `options` apply to `codeflower task`. In `codeflower interactive` you choose them
  in the chat.
- An API key is given in one of three ways per agent: `api_key_env`, `api_key_file`, or `api_key`
  itself. A file that holds a key (`config.yaml` with `api_key`, or the `api_key_file`) must not be
  readable by other users (`chmod 600`). The agent can read its key in the sandbox, as it can the key
  of a sign-in; `--isolate-network` limits where it can go. If Claude Code's settings file or Codex's
  credential file cannot be read or contains invalid JSON, the worker stops without replacing it;
  correct or restore that file before starting again.

## Tasks: the agent works by itself

`codeflower task` starts the same sandbox without VS Code and lets the agent work on one task until it
is done, for example as a batch job:

```sh
codeflower task --agent claude --project study42 --input /data/study42 --output ~/results/study42 \
  "Cluster the cells and annotate the cell types"
sbatch --time=8:00:00 --signal=B:TERM@120 --wrap "codeflower task --agent claude --project study42 \
  --input /data/study42 --output ~/results/study42 --timeout 7h50m --prompt task.md"
```

- The task is the one argument, `-` to read it from standard input, or `--prompt FILE` to read it from
  a Markdown file (one or the other, not both). It takes the folder and network
  options of `interactive`; `--output` is required, and the first one gets the results.
- The agent gets the task as its goal (`/goal` for Claude Code and Codex; DeepSeek Harness gets it as a
  plain prompt) and instructions that nobody is there to answer: it decides open questions itself and
  records them as assumptions. Permission requests are allowed and questions declined automatically.
- It builds a storyteller report as it works and delivers it as `report.html` in the output folder.
  The task is not done until the finalized report is there. `--skip-report` asks for a `README.md`
  instead.
- `--model` and `--effort` choose among what the agent offers, for example `--model opus --effort high`
  for Claude Code or `--model gpt-6.1-sol --effort xhigh` for Codex (Codex's own default effort is
  `low`). A value the agent does not offer stops the task before it starts, with the agent's choices.
  `--option ID=VALUE` sets any other setting of the agent by its id, e.g. `--option fast=on`. Without
  them the agent's defaults apply, including what its settings file in the project says.
- After each round the agent is asked whether the task is done. If not, it is sent back to work, at
  most `--max-rounds` times (default 3). `--timeout 90m` stops it after that long.
- Before it stops, the agent gets one more turn to wrap up: when the rounds run out, and shortly before
  `--timeout` (a tenth of it, between 2 and 15 minutes; the hidden `--wrap-up DURATION` sets another
  lead). It stops starting new work and brings the report up to date with what it did, what it found,
  its assumptions, and what is left and why. A task that wraps up without finishing ends "not done".
- If the agent offers terminal sign-in, `codeflower task TASK` starts it when needed when run from an
  interactive terminal, then continues the task. Batch jobs and tasks read from standard input still
  need sign-in beforehand; use `codeflower interactive` once and sign in there. The run is a session
  of the project, so VS Code lists it afterwards and you can continue it there. Its progress is
  printed.
- `--output-mode auto` uses the attached progress view when stdout is a terminal and raw output when
  redirected. Use `--output-mode raw` to pass progress through unchanged, or `--output-mode attached`
  to request the formatted view. The attached footer shows elapsed time, the selected model and effort,
  and usage/quota values when the agent reports them; Claude cost is an estimate at API prices.
- Exit status: `0` done, `2` not done (judged impossible, out of rounds or time, or the report failed its check), `3` not signed in,
  `130` interrupted or timed out, `1` other errors.

### Continuing a task

The task's folder, `<project>/tasks/<id>/`, is printed when the task starts, and the command to continue
it when it ends:

```sh
codeflower task --continue ~/codeflower/projects/study42/tasks/1a2b3c --timeout 7h50m
codeflower task --continue ~/codeflower/projects/study42/tasks/1a2b3c "Also compare the two donors"
```

- The agent continues in the task's session and is told why: the run before was interrupted (Ctrl+C,
  `--timeout`, a killed job), stopped on an error (e.g. a usage limit), or ran out of rounds or time.
  After an interruption it first checks what a cut-off command or edit left behind. A finished task
  can be continued too: the agent checks and improves its work.
- A follow-up (the argument, `-` to read it from standard input, or `--prompt FILE`) asks for more work in the same task:
  the agent builds on its results and updates the same report.
- A continued task keeps its project, agent, folders and network, which must still be where they were,
  and whether it writes a report; `--input`, `--output`, `--agent`, `--project` and the other folder
  and network options are refused. Each run gets a fresh `--max-rounds` (default: the task's) and its own
  `--timeout`; `--model`, `--effort`, `--option`, `--engine` and `--image` may change. API keys are
  read from the config again, so a new key applies.
- If the run before never started a session (e.g. it was not signed in), the task starts over, and says
  so. A session that cannot be continued, or that is open in VS Code or another run, stops the attempt
  instead of starting a second session.
- The folder keeps `task.json`, `session.json` (the task's session) and `attempts/<n>/` with each run's
  `attempt.json` and `result.json`.

## The sandbox image

The sandbox is published by [codeflower-sandbox](https://github.com/lutrarutra/codeflower-sandbox) as
`ghcr.io/lutrarutra/codeflower-sandbox`. The launcher uses its `latest` tag, so sandbox releases reach it
without a new launcher.

- `--image-tag TAG` (or `CODEFLOWER_IMAGE_TAG`, or "Sandbox version" in the window) uses another build:
  a release (`0.2.1`, `v0.2.1`, or `0.2` for the newest patch of 0.2) or a commit (`sha-abc1234`, or the
  commit's hash).
- `codeflower interactive`, `codeflower run`, `codeflower task` and the window download the image when it is missing.
  `codeflower pull [--image-tag TAG]` downloads it, or updates it to the newest build of that tag; a
  downloaded `latest` stays as it is until then.
- With Apptainer it is kept as `~/codeflower/images/codeflower-sandbox_<tag>.sif`, or in
  `CODEFLOWER_IMAGE_DIR`. Jobs that start at the same time wait for one download.
- `--image` (or `CODEFLOWER_IMAGE`) uses another image, which is never downloaded automatically: for
  example `codeflower-sandbox` built locally with `make sandbox-build`, or a `.sif` file.

## Sign-in

There is no login command. After connecting, the VS Code extension offers the agent's own sign-in, which
runs inside the sandbox and stores the login in the project:

- Claude Code: a terminal into the sandbox runs `claude auth login` (subscription or Anthropic Console).
- Codex: a ChatGPT device code shown as a link, or an OpenAI API key.
- DeepSeek Harness: a DeepSeek API key and/or an OpenAI-compatible endpoint.

The sandbox needs internet access for sign-in and for model calls (with `--isolate-network`, the
launcher's own access, through the node's `HTTPS_PROXY` if it has one). Anthropic does not permit third-party
products to offer Claude.ai login, but does not prevent users from signing in to the unmodified Claude
Code binary with their own subscription; whether using it through code flower is covered is not settled,
so check that your use is permitted, or use an API key.

## Tests

```sh
make launcher-test   # go vet and go test; `make test` in codeflower-workspace runs every component
```

The local test environment does not include Apptainer or a cluster, so run the image under Apptainer and
verify login-node-to-compute-node connectivity before using it on a cluster.
