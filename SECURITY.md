# AppDeck security guide

How the dashboard is protected, what the risks actually are on Wi-Fi / LAN /
desktop, and step-by-step recipes for remote access (tunnels), the free
built-in TLS, and certificate renewal when something feels compromised.

---

## 1. The one-paragraph model

The dashboard and its API speak **HTTP on port 9090** and, by default,
**HTTPS on port 9443** with a **self-generated certificate** (free, made on
first boot, no CA, no purchase). Anything that leaves your machine — a phone
on the Wi-Fi, another computer on the LAN — should use the `https://` link,
because plain HTTP can be **read** by anyone on the same network (Wireshark)
and **rewritten** by an on-path attacker (Burp, ARP spoofing — together known
as MITM, man-in-the-middle). Loopback traffic (`127.0.0.1`, the native
window) never crosses the network and needs no TLS.

The launcher also enforces, per request, server-side:

- **Loopback-only endpoints.** Launcher self-update, restart, deck
  export/import, storage move, theme/background changes, rebuild, and
  certificate regeneration only answer the machine the launcher runs on —
  checked against the real TCP peer address, not headers. A remote attacker
  (even one controlling the victim's browser traffic) cannot swap your
  launcher binary.
- **Cross-origin guard.** Browsers attach an `Origin` header to cross-site
  POSTs; a foreign one is rejected, so a random webpage can't drive the API.
- **`LAUNCHER_LAN_TOKEN`.** Optional. When set, state-changing requests from
  non-loopback callers must carry `X-AppDeck-Token: <value>`. It's a fence,
  not encryption — set it in addition to TLS, not instead of it.
- **Dashboard loopback mode.** ⚙ Deck tools ▸ 🛜 Dashboard reachability ▸
  *this computer only*. The strongest switch: logs and the app list stop
  being served to the network entirely. Shared *apps* keep their own ports.

## 2. Free TLS — what you get and what it costs

**Cost: $0. No certificate purchase is ever needed.** A paid certificate
proves identity to the public internet, which a private launcher doesn't
need. AppDeck generates its own on first boot (Go's crypto library, no
external tools), like a router or a NAS does.

- **Where it lives:** `<data>/tls/cert.pem` and `<data>/tls/key.pem`.
  On macOS that is `~/Library/Application Support/AppDeck/tls/`.
- **Port:** `9443` by default. Override with `LAUNCHER_TLS_PORT`, disable
  entirely with `LAUNCHER_TLS=0`.
- **Valid for ~13 months** (397 days, the browser-industry ceiling for
  server certificates). Renewing is one click — see §4.
- **Covered names:** loopback, every LAN IP of the machine, and its
  hostname. Connecting by any of them gets a certificate made for it.
- The HTTPS listener carries the **same dashboard and API** as HTTP. The
  dashboard's 🔒 Wi-Fi link automatically points at it. Loopback browsing
  keeps using plain HTTP — that traffic doesn't cross the network.

### First visit: the trust click

Because the certificate is self-generated, each browser asks once:
"This connection is not private / certificate is not trusted." That is the
expected screen, **not** an attack — click through:

| Where | What to do |
|---|---|
| Chrome / Edge (desktop) | Advanced ▸ *Proceed to … (unsafe)* |
| Safari (macOS) | Show details ▸ *visit this website* ▸ Visit Website |
| Firefox | Advanced ▸ *Accept the Risk and Continue* (Firefox keeps its own trust store, so it asks even after OS-level trust) |
| iPhone / iPad (Safari) | Show Details ▸ *visit this website* |
| Android Chrome | Advanced ▸ *Proceed to …* |

**Verifying you got the real cert, not an attacker's:** open the browser's
certificate viewer on the warning page and check the issuer/organization is
**AppDeck** and the fingerprint matches what the launcher printed at boot.
An attacker intercepting you can present their own certificate — if the
details ever change between visits, treat it as §4-level trouble.

**Zero-warning option (optional):** install [mkcert](https://github.com/FiloSottile/mkcert)
once (`brew install mkcert && mkcert -install`), then have the launcher trust
your own CA — or just click through; for a personal launcher the click is
the honest price of a free certificate.

## 3. Remote access: use a tunnel, not a port forward

**Never port-forward 9090/9443 on your router to the internet.** The API
controls process launches, updates and deck data; the internet is not the
place for it. Tunnels give you encryption *and* authentication with nothing
exposed.

### Tailscale (recommended — zero config after setup)

1. Install Tailscale on the machine running AppDeck and on every device
   that should reach it; log both into the same tailnet (free tier is fine).
2. That's it. Each device gets a private 100.x.y.z address that works from
   anywhere (coffee shop, office, vacation) with no router changes.
3. Open `http://<tailscale-ip>:9090` — or the machine's MagicDNS name like
   `http://macbook:9090`. Traffic is WireGuard-encrypted end to end; TLS
   and LAN tokens become optional belt-and-suspenders.
4. Optional hardening: in the Tailscale admin console, restrict which
   devices may reach the AppDeck machine (ACLs), so a lost phone can't.

### SSH tunnel (no extra software on the server side)

From the remote machine:

```bash
ssh -N -L 9090:127.0.0.1:9090 user@home-machine
```

Then open `http://127.0.0.1:9090` locally — everything travels inside the
SSH connection, and the launcher sees a *loopback* caller, so all the
loopback-only endpoints work too (a bonus: the remote device effectively
becomes "this computer" for the launcher). On Windows the same command runs
in PowerShell with the built-in OpenSSH client. Keep the tunnel in a
terminal window; `Ctrl+C` closes it.

Add `-L 9443:127.0.0.1:9443` if you also want the HTTPS twin through the
tunnel (usually pointless — SSH already encrypts).

### When you're only ever on your own Wi-Fi

Skip tunnels. Keep the dashboard reachable on the LAN for phones/tablets,
use the 🔒 `https://` link, set `LAUNCHER_LAN_TOKEN` for peace of mind, and
flip on **loopback mode** whenever guests come over or you're done for the
day.

## 4. "I think I'm compromised" — renewal & response

The certificate and the token are secrets. Regenerate either whenever their
safety is in doubt. Regeneration costs nothing and takes seconds.

### Regenerate the TLS certificate

- **From the API (loopback only):**

  ```bash
  curl -X POST http://127.0.0.1:9090/api/tls/regenerate
  ```

  A brand-new private key and certificate are generated and swapped into
  the running listener immediately — no restart. Browsers will show the
  trust warning once more on the next visit.

- **Manually:** stop the launcher, delete the `tls/` folder
  (`~/Library/Application Support/AppDeck/tls/` on macOS), start it — a
  fresh pair is minted on boot.

Do it when: a device that had the certificate trusted is lost or sold, the
`tls/` folder or a backup containing it may have been read by someone else,
you saw an unfamiliar certificate fingerprint (§2), someone uninvited was
on your Wi-Fi, or roughly once a year as routine hygiene when it expires.

### Rotate the LAN token

`LAUNCHER_LAN_TOKEN` is set in the environment; a leaked one can be replayed
by anyone who captured it. Give the launcher a new value (launch config,
shell profile, or service definition) and restart it. If you never set one,
there's nothing to rotate — consider setting one.

### Full response checklist (paranoia level)

1. Regenerate the TLS certificate (above) — invalidates the old key.
2. Rotate `LAUNCHER_LAN_TOKEN`.
3. Change the **Wi-Fi password** — it is the actual front door; anyone who
   had it can ARP-spoof regardless of TLS.
4. Flip on dashboard loopback mode until you've reviewed things.
5. Skim `<data>/logs/` for requests you don't recognize (the launcher logs
   every request line, with source addresses visible in your HTTP layer).
6. Review installed apps and any import history (`imports.log`) — the API
   can install things; make sure nothing foreign landed.
7. If the machine itself is suspect (malware, not just network): no network
   setting helps — clean the machine first.

### What TLS does *not* fix

- A **malicious device already inside your network** still sees encrypted
  gibberish — but can still attempt to reach the API directly. The
  loopback-only endpoints and (optionally) the token are what stop that.
- A **compromised machine** (malware on the Mac/PC running AppDeck): game
  over regardless — it can read everything the launcher can.
- **Web apps on their own ports** (🛜 Wi-Fi apps): they serve plain HTTP
  from their own servers. The launcher's TLS doesn't cover them; don't put
  secrets through them on shared networks.
- **The dashboard's app logs and settings documents** are served to LAN
  clients (read-only) — under TLS nobody in transit can read them, but a
  client *allowed* to open the dashboard still can. Loopback mode is the
  switch that stops that.

## 5. Risk by location (what an attacker can do)

| Where | Read your traffic | Modify it (MITM) | Reach the API |
|---|---|---|---|
| Native window / `127.0.0.1` on your machine | Malware only | Malware only | Yes — it's you |
| Your own WPA2/3 Wi-Fi (password private) | No (TLS); gibberish otherwise | No (TLS) | LAN endpoints — token optional extra |
| Guest/smart devices on the same Wi-Fi | No (TLS) | No (TLS) | LAN endpoints unless loopback mode is on |
| Café / hotel / office / dorm network | Without TLS: trivially. With TLS: no | Without TLS: trivially. With TLS: no | Loopback-only endpoints blocked; rest per token |
| The internet (port-forwarded) | — | — | **Don't.** Use a tunnel (§3) |
| Through Tailscale/SSH tunnel | Never | Never | Full, and loopback endpoints too |

## 6. Quick reference

```bash
# Env knobs (set before starting the launcher)
LAUNCHER_TLS=0                 # disable the HTTPS listener entirely
LAUNCHER_TLS_PORT=8443         # move the HTTPS port
LAUNCHER_LAN_TOKEN=<secret>    # require X-AppDeck-Token from non-loopback writers

# Regenerate the TLS certificate (loopback only, live, no restart)
curl -X POST http://127.0.0.1:9090/api/tls/regenerate

# Where the certificate lives (delete + restart to re-mint)
#   macOS: ~/Library/Application Support/AppDeck/tls/
#   Windows: <data dir>\AppDeck\tls\
#   Linux: ~/.local/share/AppDeck/tls/ (or your configured data dir)

# SSH tunnel from a remote machine
ssh -N -L 9090:127.0.0.1:9090 user@appdeck-machine
```
