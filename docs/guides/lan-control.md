# Drive piperd from another machine on the LAN

On the box itself the CLI needs no login. To drive the box from a laptop on the
same network, open the control API off loopback, mint a token on the box, and
log the CLI in once.

**On the box itself, the CLI needs no login**: the control API binds to
loopback (`127.0.0.1:8088`) by default and serves it tokenless — being able to
run `piper` on the box is itself the proof you own it. `piper list`, `piper
deploy`, etc. just work.

Once the API leaves loopback it requires a bearer token, so mint one on the box
and log the CLI in first. Running `piperd token create` on the box needs no
auth either; on a systemd install it needs `sudo` to reach the service's data
dir and will say so if you forget.

## Open the API and mint a token

To reach the control API from another machine on the LAN set
`PIPER_API_ADDR=0.0.0.0:8088` on the box — uncomment it in
`/etc/piper/piperd.env` and restart:

```bash
# on the box:
echo 'PIPER_API_ADDR=0.0.0.0:8088' | sudo tee -a /etc/piper/piperd.env
sudo systemctl restart piperd
sudo piperd token create --name laptop         # prints a token once
# on the client — address the box by its IP; mDNS *.local names often
# don't resolve on home LANs (run `hostname -I` on the box to find it):
piper login --token <token> --addr http://192.168.1.50:8088
piper list                                     # now authenticated
```

`piper login` verifies the token against the box and saves it (with the
address) to `~/.piper/piper/config.json`, mode `0600`; `PIPER_TOKEN` /
`PIPER_ADDR` override the saved values per command. Manage tokens on the box
with `sudo piperd token list` and `sudo piperd token revoke <name>`.
