# scooter-mode

Use [Scooter](https://github.com/chadac-me/scooter) from Emacs: browse and start
conversations on one or more Scooter servers, chat through
[agent-shell](https://github.com/xenodium/agent-shell), and sign in behind
oauth2-proxy.

Work in progress.

## Emacs

Needs Emacs 29.1+, `magit-section`, `transient`, `agent-shell` and `curl` 7.76+ (shell-maker checks it), plus
the `scooter` binary from [scooter-cli](https://github.com/chadac-me/scooter-cli)
on `PATH` (`nix profile install github:chadac-me/scooter-mode` installs it too).

```elisp
(add-to-list 'load-path "~/src/scooter-mode/lisp")
(require 'scooter)
(require 'scooter-project)          ; C-x p p and Projectile hooks
(server-start)                      ; lets the bridge refresh logins
(keymap-global-set "C-c s" scooter-command-map)

(setq scooter-servers
      '((home :url "https://scooter.example.org" :auth api-key)
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
  policy and approvals still needed above Approve/Deny.
- `C-c s c`, `C-c s r`, `C-c s p` start a conversation from a prompt, the
  region, or the current project; `C-c s R` sends the region to an existing
  one.
- Auth: `api-key` asks for a key from Scooter's Settings → API keys (or reads
  it from auth-source, host = the server, user = `scooter-api-key`) and saves
  it like a cookie. It needs no auth proxy and lasts until the key expires,
  but reaches only your own conversations, answers questions but not
  approvals, and has no live list updates (`g` refreshes) or Tramp.
  `cookie` asks for the `_oauth2_proxy` cookie (or reads it from
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
and both parts complete. In a Scooter agent-shell buffer, `C-x C-f`
and `C-c s f` start in that conversation's sandbox (`C-u C-c s f` asks). Tramp runs `ssh` with `scooter tunnel` as its
ProxyCommand, which wakes the sandbox, starts its `ssh` web service and pipes
sshd over a WebSocket. The sandbox needs `webServices.ssh.enable`, and `ssh`
must be on your `PATH`. Before each connect Emacs writes the conversation id
and current auth headers to a 0600 file under `scooter-tramp-directory`, next
to the client key.

## The `scooter` binary

`scooter acp` (the ACP agent agent-shell talks to) and `scooter tunnel` (the
Tramp ProxyCommand) live in
[chadac-me/scooter-cli](https://github.com/chadac-me/scooter-cli), with their
docs and releases. When `scooter` isn't on `PATH`, `scooter-install` downloads
the release named by `scooter-install-version` and checks its `SHA256SUMS`.
