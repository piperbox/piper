# First deploy

Register an app, deploy a directory that holds a Dockerfile, and open it at
`http://<name>.piper.localhost` — all on the box, with no login and no relay.

## Create and deploy

```bash
piper create blog --port 8080   # the port your container listens on (default 8080)
piper deploy blog --path .      # build the Dockerfile in ., run it, health-check, route
```

`deploy` follows the build and prints each stage as it happens:

```
→ building image
→ starting container
→ health-checking
deployed blog: http://blog.piper.localhost (running)
```

The health check is deliberately simple: the container must accept a TCP
connection on its port within 30 seconds. No HTTP path, no status code. A
container that never opens the port ends the deployment as `failed`, and the
previous deployment, if any, keeps serving.

`--path` defaults to the current directory. `--timeout` (default 15 minutes)
bounds how long the CLI follows the deploy; the build itself continues on the
box if the CLI gives up. An app that has been linked to a GitHub repo
([Git deploys](git-deploys.md)) deploys from that repo when `--path` is
omitted.

## Look around

```bash
piper list      # name, port, URL per app
piper status    # the same, plus each app's status and the daemon's version
```

Deployment status is one of `building`, `running`, `failed`, `stopped`.
Bare `piper` opens the [TUI](tui.md), which adds deployment history and live
logs per app.

## Stop, start, delete

```bash
piper stop blog          # stop the container and drop its routes; history stays
piper start blog         # run the last deployment again
piper delete blog --yes  # remove the app, its containers, routes, and images
```

## Environment variables

```bash
piper env blog set DATABASE_URL=postgres://… LOG_LEVEL=debug
piper env blog ls            # names and ages; --show prints the values
piper env blog rm LOG_LEVEL
```

Values are stored on the box and applied on the app's next deploy or restart,
never by bouncing the running container.

## Reaching the app

`*.piper.localhost` resolves to loopback on most systems without any
configuration. Your user must be able to reach a Docker socket: be in the
`docker` group, or set `DOCKER_HOST`. To reach the box from another machine,
see [LAN control](lan-control.md); for a public HTTPS URL, see
[Join the public relay](relay-login.md).
