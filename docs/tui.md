# The agents' own terminal UIs

The user can work with an agent in its own terminal UI (Claude Code, Codex, OpenCode) instead of, or next to, the
chat in the app's page and VS Code. There are two commands:

- **`codeflower attach`** opens a chat of a running sandbox in the agent's terminal UI. The UI runs in the sandbox,
  in a terminal the bridge keeps, and the page and VS Code stay in sync with it.
- **`codeflower tui`** starts a sandbox that runs only the agent's terminal UI, on the user's terminal, and ends with
  it. Nothing else runs in it.

DeepSeek Harness has no terminal UI, and neither command offers it. The user's docs are the site's guide
(`site/src/content/docs/guides/terminal-ui.md`); this file is the design, for whoever changes it.

## `codeflower attach`

```
codeflower attach [SANDBOX] [CHAT] [--new | --last] [--watch] [--links auto|local|tunnel|path] [--config DIR]
codeflower chats [SANDBOX] [--json]
codeflower terminals [SANDBOX] [--json] [--close ID|KEY]
```

`app/internal/attach`, with the commands in `app/internal/cli/attach.go`.

- **SANDBOX** resolves through `ResolveTarget` (`targets.go`) to one of:
  - a `codeflower://` line;
  - the page's id of a local sandbox (`local-1a2b3c4d`, `client.LocalID`);
  - a run id or its start;
  - a project name or an added sandbox's name (case-insensitive);
  - a number from the list.

  The list (`Targets`) holds the sandboxes in the run registry (`sandbox.ListRunning`, newest first, without tasks)
  and then the client's added ones (`client.SavedWorkers`, from `workers.json`). With none given and one running,
  that one is used. With several, `attach` asks and the other commands list them.
- **CHAT** (`ResolveChat`) is a number from `codeflower chats`, a key, a whole title, or the start of exactly one
  title. Without CHAT, `--new` or `--last`, a picker lists the chats.
- **The connection.** It goes straight to the bridge, over TLS pinned to the line's fingerprint
  (`sandbox.Connection.Transport`), with the token as bearer. It sends no `initialize`: the bridge answers
  `_codeflower/*` without one, and starts no chat for it.

### In the terminal

The local terminal goes raw, and every key goes to `_codeflower/terminal/input`, except the escapes. Like ssh's, they
work after Enter or at the start (`escaper` in `session.go`):

| Keys | |
| --- | --- |
| `~.` | detach: `terminal/detach`; the UI keeps running |
| `~s` | switch: detach, show the chat picker (a number, `n` for a new chat, Enter to go back), attach to the chosen chat's UI, started if needed |
| `~q` | quit the UI: `terminal/close` |
| `~l` | links: the chat in the page, the page with its key (local only, printed only here), the files shown so far |
| `~o` | open the last file shown in the browser (`open`, `xdg-open`, `rundll32`; over SSH it prints the link instead) |
| `~?` | help |
| `~~` | a `~` |

- **One UI per chat.** Switching never changes chats inside the UI. If the UI goes to another session itself
  (Claude's `/resume`, `/clear`), the bridge lets the chat go (see the handoff) and `attach` shows
  `_codeflower/terminal/left`.
- **Leaving.** Whenever `attach` leaves a chat, it resets the terminal: the main screen, the cursor, and no mouse,
  bracketed paste or keyboard modes.
- **Size.** Sizes follow SIGWINCH (`resize_unix.go`), or a poll every 0.5 s on Windows (`resize_windows.go`).
- **Errors.** It refuses without a terminal, and when `status.capabilities.chatTerminals` is false (another agent,
  or an older image).

## The bridge's chat terminals

`sandbox/dsh-plugins/acp-ws/src/chat-terminals.js` and `server.js`.

- **Worker-level.** A chat's terminal belongs to the worker, not a window: it keeps running without viewers. Sign-in
  terminals (`kind: "auth"`) are still per window and end with it.
- **One per chat,** keyed by the session key.
- **Replay.** A window that attaches gets a ring buffer of the last 256 KiB (`replayed: true`) and a resize nudge (one
  row less, then its size), so a full-screen UI draws itself again.
- **Viewers.** The last viewer to type or resize sets the size. Viewers with `watch` can neither type nor resize.
- **The PTY.** The program runs in `bin/codeflower-pty`, sized before exec from `COLUMNS` and `LINES`. JSON lines
  `{"resize":[columns,rows]}` on its file descriptor 3 change the size.

### Methods and notifications

All under `_codeflower/`. They go into `docs/architecture.md`'s contract.

| | |
| --- | --- |
| `terminal/start {kind:"chat", sessionKey \| new:true, columns, rows, watch?, linkBase?}` | `{terminalId, sessionKey, sessionId}`; a chat with a UI already running is attached to |
| `terminal/attach {terminalId, columns, rows, watch?, linkBase?}` | watch again (replay, nudge) |
| `terminal/resize {terminalId, columns, rows}` | |
| `terminal/detach {terminalId}` | |
| `terminal/input {terminalId, data}` / `terminal/close {terminalId}` | for both kinds of terminal |
| `terminal/list` | `{terminals: [{terminalId, sessionKey, sessionId, mode, viewers, startedAt}]}`; `mode` is `shared` or `handoff` |
| notification `terminal/output {terminalId, data, replayed?}`, `terminal/exit {terminalId, exitCode}` | to the terminal's viewers |
| notification `terminal/changed {terminals}` | to every window when one starts, ends, or a viewer comes or goes |
| notification `terminal/show {terminalId, files:[{path,url}], title?, message?}` | to viewers: the rail |
| notification `terminal/left {terminalId, sessionKey}` | the UI went to another session |
| `status.capabilities.chatTerminals` | the agent has `tuiCommand` and the worker a PTY helper |
| `sessions/list` entries' `terminal` | the chat's terminal, as `terminal/list` describes it |

- **Opening a chat.** `startChatTerminal` opens a recorded chat in the agent as a window does (`openSession`): resume
  where offered, else load, refused while another worker holds its lease.
- **New chats.** `new` starts a chat with `session/new`.
- **Windows.** Windows are marked `acp` on `initialize`, and `newestClient` skips connections without it (`attach`),
  so files to show reach a window.
- **Closing.** The bridge now handles `session/close`: the chat leaves `host.sessions`, the windows' sessions and the
  recorder, so the next open loads it again. A handed-over chat is the exception.

## Keeping the windows in sync

Two ways, by agent.

### Shared: OpenCode and Codex

The adapter and the UI use one backend, so both follow the same chat live, and either can prompt, cancel or answer a
permission.

- **OpenCode** (`agents/opencode`):
  - The bridge starts `opencode acp --hostname 127.0.0.1 --port <free>` with a new `OPENCODE_SERVER_PASSWORD` at
    every start. That server is OpenCode's own HTTP API.
  - The UI is `opencode attach http://127.0.0.1:<port> --session <id> --dir <cwd>`, with the password in its
    environment.
  - The agent's tools run in the server's process, so the password is in their environment too; the sandbox is one
    user anyway.
- **Codex** (`agents/codex`):
  - Codex lets one app-server write a chat (its thread writer lock), so there is one:
    `codex app-server --listen unix:///tmp/codeflower-codex-<random>.sock`. It is the agent's `companion`, which the
    bridge starts before the adapter and stops with it.
  - The adapter reaches it through `proxy.js`, set as `CODEX_PATH`. The proxy relays JSON lines on stdio to the
    socket's WebSocket and drops the adapter's `app-server` arguments. `codex app-server proxy` cannot replace it: it
    relays raw WebSocket bytes.
  - The UI is `codex -c check_for_update_on_startup=false --remote unix://<socket> resume <thread>`.
  - The adapter sends the chat's mode (approval policy and reviewer) with every turn, and the thread keeps the last
    one, so the web's mode applies to the UI's turns.

ACP relays the assistant's side of a turn the UI starts, but not the user's message, the turn's start and end, or a
permission answered in the UI. Each agent's `watch` reads them from the backend and emits the same events
(`fromBackend` in `server.js`):

| Event | OpenCode (`GET /global/event`, basic auth) | Codex (an extra app-server client) | The bridge |
| --- | --- | --- | --- |
| `prompt` | `message.updated` with role `user`, then its text `message.part.updated` | `item/started` with a `userMessage` item | records a prompt, sends `_codeflower/prompted` with `_meta['codeflower/from']: 'terminal'` |
| `busy` | `session.status` busy | `turn/started` | starts a turn in `activity` unless a window's prompt did, and holds the windows' prompts until it ends |
| `idle` | `session.status` idle | `turn/completed` | after 500 ms (the backend is ahead of ACP): `turn_end`, ends the turn, sends the next held prompt |
| `permissionReplied` | `permission.replied` (tool call from `permission.asked`) | | `settleElsewhere` |

- **Codex follows chats on request.** Its watcher subscribes to a thread with `thread/resume` once a terminal opens
  that chat (`follow` and `follows`).
- **Counting the windows' own prompts.** A window's own prompt comes back as a user message too. The bridge counts
  the prompts it sent to each watched chat (`ownPrompts`), and those are not shown again.
- **Permissions answered in the UI.** `settleElsewhere` gives the adapter the matching option, which it ignores
  because it has the answer already, so its own queue of requests moves on. The windows get `requestResolved`.
- **The same for Codex.** A tool call whose permission still waits and that ends (`tool_call_update` completed or
  failed) is settled the same way.

### Handoff: Claude Code

Claude Code cannot share a live session with its adapter, so the UI takes the chat over for as long as it runs
(`handOver`, `handBack` and `fromHook` in `server.js`; `agents/claude/mirror.js`).

1. **Taking over.** The bridge waits until the chat's turn ends, then sends `session/close` to the adapter. The
   windows keep the chat open, and the bridge keeps its settings and record.
2. **Starting the UI.** It runs:
   ```
   claude --resume <id> --settings <json> --permission-mode <the chat's mode> [--mcp-config <file>]
   ```
   - `--settings` holds the hooks (`hookSettings`) and the agent's `tuiSettings`, i.e. `plansDirectory`, which the
     adapter's sessions get from `sessionMeta`.
   - `--mcp-config` is the configured servers in Claude's form (`claudeMcpConfig` in `mcp.js`).
   - The environment adds `DISABLE_AUTOUPDATER=1`, `CODEFLOWER_HOOK_SOCKET` and `CODEFLOWER_HOOK_CHAT`.
   - `prepareTui` first answers its first-run screens in `$CLAUDE_CONFIG_DIR/.claude.json`: onboarding, theme, and
     trust for the folder.
3. **Following it.**
   - **Hooks.** Every hook runs `bin/codeflower-hook`, which hands its event, with the chat, to the hook socket and
     decides nothing.
     - `UserPromptSubmit` records the prompt (from the terminal), unless the bridge typed it, and starts the turn.
     - `PermissionRequest` shows the chat as waiting; the UI asks.
     - `PostToolUse` ends the wait.
     - `Stop` ends the turn after 500 ms and a read of the transcript, because Claude writes the last lines just
       after the hook.
     - `SessionStart` with another session id is the UI leaving: the chat goes back, and the viewers get `left`.
   - **Transcript.** It is followed from its end at the handoff, `$CLAUDE_CONFIG_DIR/projects/<cwd with all but
     letters and digits as ->/<id>.jsonl`. `updatesFor` turns its text, thinking, tool uses and results into the
     updates the adapter would send, which are recorded and sent to the windows.
4. **The windows meanwhile.**
   - A prompt (text only) is queued and typed into the UI between turns, as a bracketed paste and then Enter. The
     window's `session/prompt` is answered when that turn's `Stop` comes.
   - A cancel sends Esc.
   - Any other request about the chat (mode, settings, steering) is refused.
   - Opening the chat attaches to what the bridge keeps.
   - At the job's end, the wrap-up is typed in the same way after Esc (`wrapUpInTerminal`).
5. **Handing back.** When the UI exits, the adapter resumes the chat (`session/resume`), which has everything in it.
   Prompts queued meanwhile go to the adapter.

Not handled: a chat whose context the adapter cleared has a new Claude session id behind the same ACP id, and the UI
would resume the old one.

## Links to what the agent shows

The UI draws its own screen, so files the agent shows (`codeflower-show`) reach the user as links. They carry no
secret, because the agent sees them in its tool's output.

- **The link base.** `attach` sends a `linkBase` with `terminal/start` and `terminal/attach`:
  `<page>#/file/<sandbox's id in the page>/`. The bridge adds the encoded path, and `?chat=<key>` when exactly one
  chat is working. `findPage` in `links.go` chooses the page:
  - the page of the client running for the config directory (`client.json`, without its key);
  - through remote access (`GET /api/tunnel`'s `pageUrl`) over SSH, or with `--links tunnel`, unless the sandbox is
    `--no-tunnel`;
  - none with `--links path`, a pasted line, or no client.
- **`codeflower-show`** prints `Link for the user: <url>` for each file. The show answer has `links`. With links but
  no window, it answers `{shown: [], links}` instead of an error.
- **`codeflower-open FILE…`** (a link to `codeflower-show`, `linkOnly`) prints only the links.
- **The rail.** `attach` shows the files on its last row as OSC 8 links for 8 s, then gives the row back with a
  resize nudge.
- **The page's file route.** `#/file/<sandbox>/<path>?chat=<key>` (`ui/src/web/lib/fileLink.ts`, `openLink` in
  `app.svelte.ts`) waits up to 15 s for the sandbox to be listed, opens the chat, and shows the file beside it.

## In the page and VS Code

- **Following the terminals.** The client calls `terminal/list` on connect when the worker has the capability, then
  follows `terminal/changed` (`CodeflowerHandlers.terminals`, `WorkerHandlers.terminals`).
- **The page's notice.** The page shows a notice on a chat open in a terminal (`noticeTerminal` in
  `web/lib/worker.svelte.ts`): shared, or handed over with what that means. It includes the `codeflower attach`
  command, except through the tunnel, whose ids the computer's command does not know.
- **VS Code.** The handler is optional, so the extension builds against older ui releases. Showing the notice there
  is still to do.

## `codeflower tui`

```
codeflower tui [--agent claude|codex|opencode] [the folder, project, engine and network flags of interactive]
               [--continue | --resume[=ID]]
```

`app/internal/cli/tui.go`; `Worker.TUI` is `new`, `last`, `pick` or an id.

- **What runs.** The sandbox runs the entrypoint as always, and then the bridge's `index.js`, which, on
  `CODEFLOWER_TUI=1`, runs `runTui` (`src/tui.js`) instead of the bridge. That happens after the config's API key,
  MCP servers and plugins are applied and the egress forwarder is started.
- **The command.** `runTui` runs the agent's `soloCommand` with the same settings and environment as a handoff:
  - Claude: `--continue`, `--resume [id]`, and the config's MCP servers.
  - Codex: `resume --last|[id]`.
  - OpenCode: `--continue`, `--session id`; it lists its chats itself.
  - Codex and OpenCode are not given the config's MCP servers yet, and say so.
- **Signals and exit.** The UI runs with inherited stdio. `runTui` ignores SIGINT, which is the program's own key
  (in raw mode the terminal sends none), forwards SIGTERM and SIGHUP, and exits with the UI's code. The container
  ends with it.
- **The terminal.**
  - Docker: the container gets a TTY (`DockerRun.TTY`); the user's terminal goes raw and follows its size
    (`ContainerResize`, `tty.go`).
  - Podman: `-t`.
  - Apptainer: simply the user's terminal.
- **Opening the chat again.** The agent prints its own resume command, which the user's terminal does not have.
  The app passes the command line that ran, without `--continue`/`--resume`, and ending in `--resume=`
  (`CODEFLOWER_TUI_AGAIN`, `tuiAgain`). When the UI ends, `runTui` asks the agent's `lastSession` for the chat written
  since the UI started, and prints that line with its id:
  - Claude: the newest transcript in `$CLAUDE_CONFIG_DIR/projects/<cwd slug>/`, named by session id.
  - Codex: the newest `sessions/**/rollout-*-<id>.jsonl` in `CODEX_HOME`.
  - OpenCode: the latest top-level session of the folder in `opencode.db`.
- **Nothing to connect to.** No port, connection file, page or tunnel, and no entry in the run registry.
- **Chats.** They are the agent's own, kept in the project's agent homes, and code flower does not record them: the
  page and VS Code do not list them.
- **No previews.** `codeflower-show` has no bridge to reach.
- **Older images** start a worker instead, whose connection file makes the app stop with `ErrImageWithoutTUI`.

## In the image

- **The PATH in login shells.** `profile.d/codeflower.sh` (in `/etc/profile.d`) restores the sandbox's `PATH` from
  `CODEFLOWER_SANDBOX_PATH`, which the entrypoint sets. Login shells, such as OpenCode's `!` commands, otherwise get
  Debian's `/etc/profile` one, without `/opt/codeflower/bin`, conda or the project's environment.
- **Scripts.** `bin/codeflower-hook`, `bin/codeflower-open`, and `agents/codex/proxy.js` (executable).

## Tests

- **Bridge** (`sandbox/dsh-plugins/acp-ws/test`):
  - `chat-terminals.test.js`: the registry with a fake UI in the real PTY helper, attach and replay, watchers,
    OpenCode's event parser, a turn typed in the terminal, held prompts, permissions settled, links.
  - `handoff.test.js`: `fixtures/fake-claude-tui.js` runs the real hook script and writes a transcript in Claude's
    format.
  - `codex.test.js`, `opencode.test.js`, `tui.test.js`.
- **App:** `internal/attach/attach_test.go` (escapes, resolution, links).
- **ui:** `test/fileLink.test.ts`, and the terminal list in `test/client.test.ts`.

End to end it was checked in the image with OpenCode and Codex on mock models and with Claude Code signed in. The
page's file route and notice were checked in headless Chrome against `codeflower client --ui ui/dist-web`.
