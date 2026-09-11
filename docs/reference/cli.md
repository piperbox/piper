# CLI reference

Every `piper` verb, its flags, and what it does. Each synopsis is the exact usage line the binary prints, and `test/docs` fails when a verb changes and this page does not.

## Invocation

```text
piper [--remote <base-domain>] [--version] <version|login|create|deploy|list|status|stop|start|delete|app|env|domains|github|box|agent> [args]
```

With no verb in an interactive terminal, `piper` opens the [TUI](../guides/tui.md). With no verb and no terminal it prints the usage and exits 2.

Global flags come before the verb:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--remote <base-domain>` | `$PIPER_REMOTE` | Drive a relay-connected box through the relay instead of the local piperd; see [Remote control](../guides/remote-control.md). Rejected with `version`, `login`, and `agent` (exit 2) — the rejection applies only to the explicit flag; a `PIPER_REMOTE` default is silently ignored by those verbs. |
| `--version` | | Print the build version and exit 0. |

Per-verb flags come after the positional arguments: `piper delete blog --yes`, never `piper delete --yes blog`. Go's flag parser stops at the first positional, so a flag placed before the name is taken as the name.

Exit codes are the same everywhere: 0 on success, 1 on error (piperd unreachable, the request rejected, a deploy that did not end `running`), 2 on usage (bad flags or arguments). Declining a confirmation prompt prints `aborted` and exits 0.

Every verb except `version`, `login`, `agent`, `box`, and `github repos` talks to piperd's [control API](api.md) at the address `piper login` saved, `http://127.0.0.1:8088` when nothing is saved. `PIPER_ADDR` and `PIPER_TOKEN` override the saved address and token; see [Environment variables](env.md#piper-cli).

## version

```text
piper version
```

Prints the build version, the same string as `--version`. Exit 0.

## login

```text
piper login [--relay <url>] [--web] [--org <name>] [--no-enroll] [--re-enroll] [--relogin] [--data-dir DIR]
piper login --token <token> [--addr <url>]
piper login --token <token>  (create one with `piperd token create`, prefixed with `sudo` on a systemd install)
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--token <token>` | | API token from `piperd token create`. Present: LAN login. Absent: relay login. |
| `--addr <url>` | the saved address, else `http://127.0.0.1:8088` | piperd address for a LAN login. |
| `--relay <url>` | `https://api.public.getpiper.dev` | Relay control API base URL for a relay login. |
| `--web` | off | Log in through the relay's GitHub App in the browser (one trip) instead of the device flow. |
| `--org <name>` | | Enroll this box for a GitHub org you own. |
| `--no-enroll` | off | Stop after identity; do not claim this box. |
| `--re-enroll` | off | Claim this box fresh even if already enrolled (after `piper box rm`, or when switching accounts or relays). |
| `--relogin` | off | Authenticate again even if the saved credential still works (switching GitHub accounts). |
| `--data-dir DIR` | `~/.piper/piperd` | piperd data directory, used to find the enrollment socket. |

Two commands share the verb. A **LAN login** (`--token`) verifies the token against the box's `GET /v1/apps` and saves the address and token as the current box; see [LAN control](../guides/lan-control.md). A **relay login** (no `--token`) runs the GitHub device flow against the relay (or the browser flow with `--web`), saves the account credential, then claims this box through piperd's enrollment socket and waits for the tunnel to connect; see [Join the public relay](../guides/relay-login.md).

Exit 1 when the token is rejected, piperd is unreachable, or the claim fails outright. Once piperd has persisted the enrollment, a tunnel that has not yet connected is advisory: the command says what is still pending and exits 0.

## agent

```text
piper agent <up|down|status>
```

No flags. Starts, stops, or reports the piperd service on this machine: `brew services` on macOS, the `piperd` systemd unit on Linux. `status` prints whether the daemon is running, the control API address, the build the running daemon reports, and the listen addresses and data directory it loaded. `status` exits 0 whether or not piperd is installed; `up` and `down` exit 1 when it is not installed or when `brew services`/`systemctl` fails; 2 on any other OS.

## create

```text
piper create <name> [--port N]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--port N` | `8080` | Container port the app listens on. |

Registers an app. The name is a DNS label: lowercase letters, digits, and hyphens, 1 to 63 characters, no leading or trailing hyphen, and not `hooks`. Exit 1 if the name is invalid or already exists.

## deploy

```text
piper deploy <name> [--path DIR] [--timeout DUR]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--path DIR` | `.` | Source directory containing a Dockerfile, sent to piperd as a tarball. |
| `--timeout DUR` | `15m` | Give up following the deploy after this long; `0` waits forever. |

Builds and runs one deployment, streaming build logs to stderr until it ends. Without an explicit `--path`, an app linked to a repository with `piper app link` deploys from that repository's tracked branch instead of the local directory. Prints the app URL on success. Exit 1 when the app does not exist, the final status is not `running`, or the timeout passes (the build may still be running on the box).

## list

```text
piper list
```

One line per app: name, port, and URL when it has ever been deployed. Exit 0.

## status

```text
piper status
```

Like `list` with each app's status (`building`, `running`, `failed`, `stopped`, or `-` when never deployed), preceded by the running daemon's version. With `--remote`, first reports whether the box's tunnel is connected and stops there when it is offline. Exit 0.

## stop

```text
piper stop <name>
```

Stops the app's production container, drops its routes, and marks the deployment `stopped`; the app and its history remain, and PR previews are untouched. A no-op when nothing is running. Exit 1 when the app does not exist.

## start

```text
piper start <name>
```

Runs the latest deployment's image again and restores its routes. A no-op unless the latest deployment is `stopped`. Exit 1 when the app does not exist.

## delete

```text
piper delete <name> [--yes]
piper delete <name> [--yes]  (the app name must come before flags)
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--yes` | off | Skip the confirmation prompt. |

Removes the app, its deployments, and its per-app domains after a `y`/`yes` at the prompt. Exit 2 when the first argument starts with `-` (the flag was given before the name).

## app

### app link

```text
piper app link <name> --repo owner/name [--branch main] [--root-dir apps/web]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--repo owner/name` | required | GitHub repository, `owner/name`. |
| `--branch` | `main` | Tracked branch. |
| `--root-dir` | repo root | Build subpath inside the repository, for monorepos. |

Links the app to a repository so pushes to the branch deploy it and `piper deploy <name>` without `--path` builds from the repository; see [Git deploys](../guides/git-deploys.md). Exit 2 when `--repo` is missing.

## env

```text
piper env <app> <set KEY=VALUE [KEY2=VALUE2 ...] | ls [--show] | rm KEY>
```

Per-app environment variables. Changes are saved at once and applied on the app's next deploy or restart, never by restarting the running container.

### env set

```text
piper env <app> set KEY=VALUE [KEY2=VALUE2 ...]
```

No flags. Saves every pair; every argument is parsed before anything is sent, so one malformed argument (exit 2) leaves the app untouched. Keys are `[A-Za-z_][A-Za-z0-9_]*`; `PORT` is reserved and rejected by piperd.

### env ls

```text
piper env <app> ls [--show]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--show` | off | Print values instead of masking them as `******`. |

Lists the app's variables, sorted, with how long ago each was last set.

### env rm

```text
piper env <app> rm KEY
```

No flags. Removes one variable.

## domains

```text
piper domains <add <domain> --app <name> | list [--app <name>] | remove <domain> [--app <name>]>
```

Per-app custom domains; see [Custom domains](../guides/custom-domains.md) and [Direct serve](../guides/direct-serve.md). Every subcommand exits 1 on a LAN-only box, where piperd answers 409.

### domains add

```text
piper domains add <domain> --app <name>
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--app <name>` | required | App to attach the domain to. |

Attaches the domain and prints the DNS record to create. Issuance starts once DNS resolves to the box or relay.

### domains list

```text
piper domains list [--app <name>]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--app <name>` | every app | Only this app's domains. |

One line per domain: app, status (`pending`, `issuing`, `active`, `failed`), certificate expiry, and whether DNS resolves correctly.

### domains remove

```text
piper domains remove <domain> [--app <name>]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--app <name>` | looked up | The app the domain is attached to; skips the lookup. |

Detaches the domain and releases its certificate. Exit 1 when the domain is not attached to any app.

## github

```text
piper github setup [--org <name>] | piper github repos | piper github reset [--yes]
```

The box's own GitHub App, for boxes that do not use a relay-brokered App; see [Git deploys](../guides/git-deploys.md).

### github setup

```text
piper github setup [--org <name>]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--org <name>` | your user account | Create the App under this GitHub organization. |

Opens the browser on GitHub's App-manifest page, catches the redirect, and stores the resulting App credentials on the box. Prints the App's install URL. Exit 1 after five minutes without approval.

### github repos

```text
piper github repos
```

No flags. Lists the account's relay-brokered GitHub App installations and the repositories they cover. Exit 1 without a relay login.

### github reset

```text
piper github reset [--yes]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--yes` | off | Skip the confirmation prompt. |

Deletes the box's own GitHub App credentials (the private key is not recoverable) and reports which webhook provider piperd will use after a restart.

## box

```text
piper box ls | piper box rm <base-domain> [--yes]
```

Boxes claimed on the relay account; both subcommands need a relay login and exit 1 without one. `--remote` does not apply.

### box ls

```text
piper box ls
```

No flags. One line per box: base domain, owner, and `connected` or `offline`.

### box rm

```text
piper box rm <base-domain> [--yes]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--yes` | off | Skip the confirmation prompt. |

Retires a box and frees its agent slot; the box must run `piper login --re-enroll` to come back. Exit 1 when the box is still connected, is not on this account, or belongs to an org you do not own.
