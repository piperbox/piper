# Git deploys

Once a box has joined the relay, a `git push` builds and publishes an app.
The hosted relay holds one shared GitHub App on everyone's behalf, so there is
nothing to create: `piper login` installs it on the repos you choose.

```bash
piper login                                          # ... then: install the App (link printed); login waits for it
piper create myapp --port 8080                       # register the app (needed before it can be linked)
piper app link myapp --repo owner/name --branch main # bind the repo to an app
git push origin main                                 # → live at the app's routed URL
```

Pull requests get their own preview URL, `pr-<N>-<app>.<base>`, torn down when
the PR closes.

Every push to the tracked branch builds the Dockerfile at the repo root,
health-checks the container, and serves it. The live URL shows up on GitHub as
a Deployment status. `piper github repos` lists what the installation can reach
at any point; re-run `piper login` to install the App on more repos later.

A box that ever ran `piper github setup` keeps its own App, and that always
wins over the relay's — so brokered deliveries fail their signature check until
you give it up:

```bash
piper github reset                                   # drop this box's own App
sudo systemctl restart piperd                        # the provider is picked at start
```

## Self-hosted relay or your own GitHub App

Running your own `piper-relay` without a configured App, or serving on your own
domain outside the public relay? Each box then creates and holds its **own**
GitHub App instead — the private key and webhook secret never leave it:

```bash
piper create myapp --port 8080                       # register the app (needed before it can be linked)
piper github setup [--org name]                      # create the GitHub App (one-time; use --org for org-owned apps)
# install the App on your repo in GitHub, then:
piper app link myapp --repo owner/name --branch main # bind the repo to an app
```

After that, every push to the tracked branch builds the Dockerfile at the repo
root, health-checks the container, and serves it at `https://myapp.<your-domain>`.
The live URL shows up on GitHub as a Deployment status. Webhooks ride the same
tunnel as your traffic (delivered to `hooks.<your-domain>`); nothing else on the
box is exposed.

Standing either path up against a real relay, domain, and GitHub App end to end
is covered by the repo's [self-host docs](../self-host/relay.md).
