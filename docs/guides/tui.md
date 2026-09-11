# The interactive TUI

Bare `piper` in a terminal opens a full-screen control surface: apps, deploys,
logs, boxes, and the login and GitHub wizards, all interactive. Every
subcommand stays scriptable and unchanged.

```bash
piper            # opens the TUI against the current box
```

- **Apps table** (home) — NAME · STATUS · URL · LAST DEPLOY, refreshed every 2s.
- **Drill down** — `↵` opens an app's detail, deployments, and logs (with live
  follow).
- **Actions** — deploy, new app, stop, delete, right from the TUI.
- **Boxes** — `t` opens a box switcher and config editor to add/edit/remove
  targets.
- **Wizards** — login, GitHub App setup, and repo linking run interactively.

Keys: `↵` open · `esc` back · `t` boxes · `q` quit. Every screen lists its own
keys in a dim legend along the bottom.
Run it on the box and it needs no login ([LAN control](lan-control.md)
explains why); point it at a remote box with `piper --remote <base-domain>`
(see [Remote control](remote-control.md)). Non-TTY invocation (scripts,
pipes) is untouched — bare `piper` with no terminal still prints usage and
exits 2.
