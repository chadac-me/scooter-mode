# scooter-mode

Use [Scooter](https://github.com/chadac-me/scooter) from Emacs: browse and start
conversations on one or more Scooter servers, chat through
[agent-shell](https://github.com/xenodium/agent-shell), and sign in behind
oauth2-proxy.

Work in progress.

## Emacs

Needs Emacs 29.1+, `magit-section`, `transient`, `agent-shell` and `curl` 7.76+ (shell-maker checks it), plus
the `scooter` binary on `PATH` (`nix profile install github:chadac-me/scooter-mode`).

```elisp
(add-to-list 'load-path "~/src/scooter-mode/lisp")
(require 'scooter)
(require 'scooter-project)          ; C-x p p and Projectile hooks
(server-start)                      ; lets the bridge refresh logins
(keymap-global-set "C-c s" scooter-command-map)

(setq scooter-servers
      '((home :url "https://scooter.example.org" :auth cookie)
        (work :url "https://scooter.corp.example"
              :auth (oidc :issuer "https://oauth.id.jumpcloud.com/"
                          :client-id "scooter-emacs"))))
```

- `C-c s l` lists conversations on every server as a table (status icon,
  title, model, links, owner, last activity), starred ones first. Hover an
  icon for what it means; `TAB` on a row shows its details (id, created, linked PRs and threads, URL). `RET`
  opens one in agent-shell; `c` new, `s` star, `t` retitle, `k` stop the run,
  `z` suspend, `f` files viewer, `b` browser, `g` refresh, `?` menu.
  Shells are named `Scooter <slug>@<server>`; a new conversation's shell
  takes its slug after the turn in which the agent titles it.
- Approvals render in the shell. The agent's questions become an
  agent-shell form (`scooter-agent-shell-forms`, on by default, turns on
  agent-shell's experimental forms in Scooter shells only; off, they
  point at the web UI). AWS access requests show the account, risk,
  policy and approvals still needed above Approve/Deny. Commit identity proposals
  show the identity; kinds with no card here show their raw data and the
  web UI link. A request answered in the web UI is withdrawn from the shell.
- `C-c s b` (`scooter-sandbox`) shows the conversation's sandbox (status,
  size, visibility) and resizes, suspends or resumes, restarts, compacts,
  renames, stars or changes its visibility; `C-c s w` opens, copies, starts
  or stops one of its web services; `C-c s k` opens a linked resource (`C-u`
  copies its URL). Each acts on the Scooter shell you are in or the row at
  point in `scooter-list` (`m`, `v`, `l` there), and `C-u` picks another.
  Links added while a conversation is open, and web services the agent
  starts, show up as notes in its shell.
- `C-c s c`, `C-c s r`, `C-c s p` start a conversation from a prompt, the
  region, or the current project; `C-c s R` sends the region to an existing
  one.
- Auth: `cookie` asks for the `_oauth2_proxy` cookie (or reads it from
  auth-source, host = the server, user = `_oauth2_proxy`) and saves it to
  your first `auth-sources` entry, replacing the old one. `oidc` logs in
  through the browser with PKCE; oauth2-proxy must run with
  `--skip-jwt-bearer-tokens` and accept the client as an extra JWT issuer.
  GitHub issues no ID tokens, so GitHub-backed servers use `cookie`.

### Sandbox files over Tramp

`C-c s f` (`scooter-find-file`) opens a file in a conversation's sandbox:

```
/scooter:<conversation>@<server>:/workspace/...
```

`<conversation>` is the title slug shown in `scooter-list` (or an id prefix),
and both parts complete. In a Scooter agent-shell buffer, `C-x C-f`,
`C-c C-f` and `C-c s f` start in that conversation's sandbox (`C-u C-c s f`
asks), and `C-c C-j` opens Dired on its `/workspace` (agent-shell's
`C-c C-d` stays comint's send-EOF). The keys live in
`scooter-agent-shell-files-mode-map`, so `keymap-set` rebinds them. Paths
like `/workspace/foo.mjs:42` (also `:42:5` and `#L42`) in the agent's text
and tool output become links: `RET` or a click opens the file in the sandbox
at that line (`scooter-agent-shell-link-paths` turns this off). Tramp runs `ssh` with `scooter tunnel` as its
ProxyCommand, which wakes the sandbox, starts its `ssh` web service and pipes
sshd over a WebSocket. The sandbox needs `webServices.ssh.enable`, and `ssh`
must be on your `PATH`. Before each connect Emacs writes the conversation id
and current auth headers to a 0600 file under `scooter-tramp-directory`, next
to the client key.

## `scooter tunnel`

`scooter tunnel --server URL --conversation ID --headers-command CMD` pipes
stdin/stdout to a conversation's sshd, for use as an ssh `ProxyCommand`.
`--spec-file FILE` reads the server, conversation and headers from one JSON
file instead: `{"url": …, "conversation": …, "headers": {…}}`.
While the ssh service doesn't answer (the proxy's status poll fails with a
5xx, says it isn't running, or the upgrade gets a 5xx), it resumes the
conversation if it isn't running, starts the sandbox's `ssh` web service and
retries, for up to `--wait` (default 5m). From a shell:

```sh
ssh -o ProxyCommand="scooter tunnel --server https://scooter.example.org --conversation <id> \
      --headers-command 'pass show scooter/headers'" root@sandbox
```

## `scooter acp`

`scooter acp --server https://scooter.example.org` is an
[ACP](https://agentclientprotocol.com) agent on stdin/stdout. A new ACP session
creates a Scooter conversation; `--conversation <id>` attaches the first one to
an existing conversation instead. `session/list` returns your top-level
conversations in the web sidebar's order (starred, then most recently active),
with their status, star, visibility and linked sources under `_meta.scooter`;
`session/resume` attaches without replaying history, and `session/close` stops
following a conversation. Sessions offer a `model` config option from the
server's `/models`; once you pick one, prompts go through `/agui` with
`X-Agent-Model`, as the web UI sends them (the conversation's own model needs
no pick and keeps `/messages`). Image prompts (png, jpeg, gif, webp, up to 5 MB) go
through `/agui` too, and images in history and live runs (yours, the agent's,
and tool results') are fetched from the conversation's assets and inlined. A run
started elsewhere (the web UI, a webhook, a schedule) arrives as a
`session/push`, its prompt included, and ends with `session_push_end`; while
any run is going, `_session/steering` adds your prompt to it, as the web UI's
interrupt does. `--replay-turns N` (`scooter-acp-replay-turns` in Emacs) replays
only the last N turns of a long conversation. Auth headers come from
`--headers-command` (or `--headers-file`), as a JSON object such as
`{"Authorization": "Bearer …"}` or `{"Cookie": "_oauth2_proxy=…"}`. The command
is rerun once when the server's auth proxy rejects a request, with `{retry}`
in it replaced by `t` (otherwise `nil`).

Prebuilt static binaries for Linux and macOS (amd64, arm64) are attached to
each [GitHub release](https://github.com/chadac-me/scooter-mode/releases), with
`SHA256SUMS`; `scooter version` prints the release. Or run it with Nix:

```sh
nix run github:chadac-me/scooter-mode -- acp --server https://scooter.example.org \
  --headers-command 'pass show scooter/headers'
```
