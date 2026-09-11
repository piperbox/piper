# Drive a box remotely

Any control command can target one of your relay-connected boxes from
anywhere, by the base domain `piper login` printed. Requests travel relay →
tunnel → box; your relay credential never reaches the box.

```bash
piper --remote ab12-alice.public.getpiper.dev list
piper --remote ab12-alice.public.getpiper.dev status  # box up? what's deployed?
export PIPER_REMOTE=ab12-alice.public.getpiper.dev    # or set it once
piper deploy blog --path .
```

Requests travel relay → tunnel → box: the CLI authenticates to the relay with
the account credential `piper login` saved in `~/.piper/piper/config.json`
(mode `0600`), and the relay swaps it for the box's own token — your relay
credential never reaches the box, and the box still enforces its own auth.
The `--remote` flag overrides `PIPER_REMOTE`; `login` is inherently local and
rejects `--remote`.

Bare `piper --remote <base-domain>` opens the [TUI](tui.md) against that box.
