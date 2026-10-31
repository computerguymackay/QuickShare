# quickshare

Share a file from your machine with one command and one link. The link stops working after a set number of downloads or a set time. Nothing in between: no accounts, no relay, no third-party service. The person downloading needs only a browser or `curl`.

```console
$ quickshare report.pdf
warning: plain HTTP: anyone on the network path can read this file. Encrypt sensitive files first (e.g. age, gpg).
sharing report.pdf (2.1 MiB) · 1 download, expires 2026-11-01 14:02 UTC
http://203.0.113.5:8443/q3V9xG0b1mZ7TzPq4kLwYc2A/report.pdf
```

quickshare needs a machine that the internet can reach, such as a VPS, a server with a public IP, or a home machine with a port forwarded on the router. It does not do NAT traversal.

## Install and first run

```sh
go install github.com/computerguymackay/quickshare@latest
# or, from a checkout:
go build -o quickshare . && sudo mv quickshare /usr/local/bin/
```

It builds as a single static binary (SQLite is pure Go, no cgo).

First run:

```sh
quickshare somefile.zip
```

On first run, quickshare writes a commented config file to `~/.quickshare/config.toml`. If the machine has exactly one public IPv4 address on its network interfaces, quickshare uses it. Otherwise, set `wan` in the config (see below) or pass `--wan`.

## Usage

```text
quickshare <file> [flags]     share a file and print its link
quickshare list [--json]      show active shares
quickshare revoke <token>     cancel a share (a unique prefix of 6+ characters is enough)
quickshare stop               stop the background daemon
```

| Flag | Meaning |
|---|---|
| `--max-downloads N` | Completed downloads allowed. Default 1. `0` = unlimited (requires an expiry). |
| `--expires DURATION` | `30m`, `24h`, `7d`, … Default 24h. `0` = never (requires a download limit). |
| `--allow-forever` | Allow `--max-downloads 0 --expires 0` (a link that never dies). |
| `--wan host:port` | Public address used in the link (overrides config). |
| `--bind host:port` | Address the daemon listens on (overrides config). |
| `--interactive` | Prompt for the options. |
| `--qr` | Show the link as a QR code in the terminal. |
| `--qr-out PATH` | Save the QR code as a PNG. |
| `--json` | Print `{url, token, expires_at, max_downloads}`. |
| `--watch` | Follow the daemon log after sharing (Ctrl-C stops watching; the share keeps going). |
| `--tls cert,key` | Serve over HTTPS. |

Flags can come before or after the file name. To share a file literally named `list`, `stop` or `revoke`, use `quickshare -- list` or `quickshare ./list`.

**Scripting.** stdout carries only the link (or the JSON with `--json`). Warnings, the summary and the QR code all go to stderr, so this works:

```sh
link=$(quickshare build.tar.gz --expires 2h)
echo "Download: $link" | mail -s build someone@example.com
```

## Config

`~/.quickshare/config.toml` (set `QUICKSHARE_HOME` to use a different directory):

```toml
[server]
bind = "0.0.0.0:8443"       # where the daemon listens on THIS machine
wan  = "203.0.113.5:8443"   # what the OUTSIDE world connects to; goes in the link

# optional HTTPS
# tls_cert = "/etc/letsencrypt/live/files.example.com/fullchain.pem"
# tls_key  = "/etc/letsencrypt/live/files.example.com/privkey.pem"

[share]
default_expiry        = "24h"
default_max_downloads = 1
```

**`bind` vs `wan`.** `bind` is the local socket address. `wan` is the address a downloader types. They match on a VPS with a public IP. Behind a home router, they differ: `bind` might be `0.0.0.0:8443` while `wan` is your router's public IP with whatever port you forwarded. quickshare warns when the pair looks wrong. Examples: `wan` is a private or loopback address, the ports differ (check your forward), or `bind` is `127.0.0.1` (remote clients can't connect).

`wan` can be a hostname (`files.example.com:8443`). That is required for TLS, because the certificate must match the name in the link.

## Firewall and NAT

When the daemon first starts, quickshare makes one request to the `wan` address and warns if it gets no answer. Many home routers can't connect to their own public IP from inside ("hairpin NAT"), so the warning can be a false alarm there. To be sure, test from another network, such as a phone on mobile data.

Open the port on the machine:

```sh
# ufw (Ubuntu/Debian)
sudo ufw allow 8443/tcp

# firewalld (Fedora/RHEL)
sudo firewall-cmd --add-port=8443/tcp --permanent && sudo firewall-cmd --reload

# plain iptables
sudo iptables -A INPUT -p tcp --dport 8443 -j ACCEPT
```

If the machine is behind a Linux box doing NAT (a gateway or a VM host), forward the port with DNAT:

```sh
# on the gateway: public 8443 → 192.168.1.20:8443
sudo iptables -t nat -A PREROUTING -i eth0 -p tcp --dport 8443 -j DNAT --to-destination 192.168.1.20:8443
sudo iptables -A FORWARD -p tcp -d 192.168.1.20 --dport 8443 -j ACCEPT
sudo sysctl -w net.ipv4.ip_forward=1
```

**Home router.** Add a port-forward rule (often called "Virtual server" or "NAT forwarding") from external TCP port 8443 to the machine's LAN IP, port 8443. Set `wan` to the router's public IP, which you can find on its status page. If your ISP uses carrier-grade NAT (your router's "WAN IP" starts with `100.64.`–`100.127.` or is a private address), inbound connections can't reach you, and quickshare can't fix that.

## Security

Read this before sharing anything sensitive.

- **Plain HTTP by default.** The file and the link travel unencrypted. Anyone on the network path (public Wi-Fi, an ISP, a compromised router) can read or copy both. quickshare prints this warning every time it shares over HTTP.
- **The token is the only lock.** Tokens are 144 random bits, so guessing is not a realistic attack. But anyone who *sees* the link can use it, so share it over a channel you trust.
- **Short windows help.** The default (1 download, 24 h) means a leaked link is usually already dead. Once your recipient has the file, any later attempt gets `410 Gone`.
- **Logs never contain full tokens**, only the first 8 characters.
- **Downloads aren't authenticated.** If someone else uses the link first, your recipient gets `410`. That is your signal that the link leaked.

For anything sensitive, encrypt the file first:

```sh
# age (https://age-encryption.org) with a passphrase
age -p -o secrets.tar.age secrets.tar
quickshare secrets.tar.age
# recipient: age -d -o secrets.tar secrets.tar.age

# or gpg, symmetric
gpg -c secrets.tar          # writes secrets.tar.gpg
quickshare secrets.tar.gpg
# recipient: gpg -d secrets.tar.gpg > secrets.tar
```

Send the passphrase through a different channel than the link.

### HTTPS with your own certificate

If you have a certificate for a hostname that points at this machine (for example from Let's Encrypt via `certbot certonly --standalone -d files.example.com`):

```sh
quickshare report.pdf --wan files.example.com:8443 \
  --tls /etc/letsencrypt/live/files.example.com/fullchain.pem,/etc/letsencrypt/live/files.example.com/privkey.pem
```

Or set `tls_cert` and `tls_key` in the config so every run uses them. TLS is a daemon-wide setting, applied when the daemon starts. If a plain-HTTP daemon is already running, `quickshare stop` it first. Your user needs read access to the key file.

## QR codes

Use them to get a file onto a phone:

```sh
quickshare photo.jpg --qr                 # QR code in the terminal; scan it with the phone camera
quickshare photo.jpg --qr-out link.png    # save it as an image to paste into a chat or slide
quickshare photo.jpg --qr --qr-out link.png
```

The terminal QR code is drawn white-on-black, so it scans on both light and dark terminal themes. It goes to stderr, so `link=$(quickshare --qr f.jpg)` still captures just the link. If the code is too big for your window, zoom the terminal out briefly.

## Download counting

quickshare counts **completed downloads**:

- A normal `GET` that delivers the whole file counts once, even if the same person downloads twice.
- A transfer that breaks halfway doesn't count. If the downloader starts over and finishes, that counts once.
- Requests with a `Range` header (resumed or partial downloads) never count, and neither do `HEAD` requests. So `curl -C -` and download managers can resume freely while the share is still live.
- The count never goes above `--max-downloads`. If two downloads of a one-download share start at the same moment, both may finish.

## When the daemon runs and when it stops

- `quickshare <file>` starts a background daemon if none is running, hands it the share, and exits. One daemon serves all your shares.
- A share ends when it reaches its download limit, expires, is revoked, or its file is deleted. Ended links return `410 Gone`.
- When no shares are active, the daemon waits 45 seconds (so a new share can arrive), then exits. Expiry is checked every 10 seconds, and downloads in progress are never cut off by an automatic shutdown.
- Shares are saved in `~/.quickshare/state.db` (SQLite). If the machine reboots or the daemon crashes, the next `quickshare <file>` starts a new daemon, which brings back every share that is still valid and drops expired ones and ones whose file is gone. `quickshare list` and `quickshare revoke` work while the daemon is down.
- `quickshare stop` stops the daemon immediately but **keeps** unfinished shares; they come back the next time the daemon starts. To kill a link for good, use `quickshare revoke`.
- Running several `quickshare` commands at once is safe: a lock file ensures only one daemon starts.

Files in `~/.quickshare/`:

| File | Purpose |
|---|---|
| `config.toml` | Your settings |
| `state.db` | Shares (survives restarts) |
| `quickshare.log` | Daemon log (rotated at 5 MB) |
| `control.sock` | CLI ↔ daemon channel (`control.addr` on Windows, which uses loopback TCP with a key) |
| `daemon.pid`, `*.lock` | Daemon bookkeeping |

## Development

```sh
go test ./...          # unit + end-to-end tests (builds the binary)
go test -race ./...
```

Two environment variables shorten timers for testing: `QUICKSHARE_GRACE` (default `45s`) and `QUICKSHARE_TICK` (default `10s`).
