# OpenWrt AC Controller

[简体中文](README.md) · **English**

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![Docker pulls](https://img.shields.io/docker/pulls/naoki66/openwrt_ac-controller.svg)](https://hub.docker.com/r/naoki66/openwrt_ac-controller)
[![Docker Image Version](https://img.shields.io/docker/v/naoki66/openwrt_ac-controller?sort=semver&label=image)](https://hub.docker.com/r/naoki66/openwrt_ac-controller/tags)
[![Docker Last Updated](https://img.shields.io/docker/last-updated/naoki66/openwrt_ac-controller)](https://hub.docker.com/r/naoki66/openwrt_ac-controller)

> A lightweight OpenWrt wireless controller: a single Go binary on the AC side (Web UI, REST API and SQLite embedded) plus `ac-agent` running under procd on each AP. One compose file runs it on a NAS, x86 soft-router or ARM board; the controller can also be built as an OpenWrt package and run directly on a router.

**Fastest path**: start the controller → provision its signing public key and each AP's own key → adopt a template on the Devices page.

> This is the English README. The Chinese version is [README.md](README.md). Every command and config below is copy-paste ready.

## 1. What it is, and what it is not

It **does not forward user traffic** and does not implement CAPWAP. It does exactly one thing: compiles "what this AP should look like" into an Ed25519-signed, versioned config bundle. The AP pulls it, verifies the signature, merges it segment-by-segment into local UCI, applies it, self-checks, and rolls back on failure. The AC only holds desired state, so a dead controller never takes down the Wi-Fi.

| Good fit | Not a fit |
|---|---|
| Managing SSIDs / VLANs / radio settings across several OpenWrt APs | Commercial AC features: CAPWAP, central forwarding, tunnelling |
| Home / small office that wants Wi-Fi to survive a controller outage | AAA, captive portal, billing gateways |
| Tinkerers willing to build packages or flash companion firmware | A zero-command-line, out-of-the-box product |

**Before you try it:**

- This is an **early v0.5.x** project. Features, API and config formats may still change; do not put it on a critical production network.
- **Verified AP hardware** is the Brightspeed/Gemtek **XR1710G** only (airoha/an7581, arm64, DSA), using the companion [ImmortalWrt firmware](https://github.com/naoki66/ImmortalWrt-for-Gemtek-brightspeed/releases). Other OpenWrt-family devices are unverified and need a DSA network model plus `procd`/`ubus`/`uci`/`hostapd`; 11k/11v additionally requires hostapd to expose `rrm_nr_get_own`, `rrm_nr_set` and `bss_transition_request`. The capability badges on the devices page are **measured at runtime**: 11k/11v probe the ubus method surface, while 11r (FT) is read from the netifd-generated hostapd configs (`/var/run/hostapd-*.conf`) together with the mobility domain (MDID); a badge only lights when every BSS satisfies it.
- Auto-discovery requires the AP and AC to share a **broadcast domain**; across L3, fill in "Controller address" on the AP's LuCI page (see section 8).
- The image is a `linux/amd64` + `linux/arm64` multi-arch manifest; Docker picks the right platform automatically.

## 2. Key features

- **Segment-level merge, not full-file overwrite**: backup before apply and automatic rollback on failure, so you never lock yourself out of an AP.
- **Three anti-lockout guards** (added after v0.5.1): a `preflight` check for management-address moves before a push; a post-apply BSS/address watchdog; and automatic repair of the `lan` firewall zone and dropbear binding for newly added management interfaces.
- **The AC never connects to APs**: config is always pulled. APs exchange 11k neighbour and client state over UDP 29821, so local 11v roaming advice survives an AC outage (`peer_steer=1`); the agent steps aside when usteer/dawn is detected.
- **AC trust over HTTP**: APs verify heartbeat replies and bundles with a preprovisioned Ed25519 public key. Bundles also carry per-file SHA-256 hashes and target constraints.
- **One secret, two purposes**: the AC and every AP share only `AC_SHARED_SECRET`; HKDF-SHA256 derives two distinct keys — the AP→AC auth token and the AP↔AP message HMAC key. The raw secret is never sent on the wire.
- **Templates + per-device overrides + groups**: templates define wireless/L2, groups decide which template their members run, and per-device `network_params` / radio overrides supply ports, VLANs, management addresses, channels and power.
- **Operations loop**: firmware repository and upgrades, scheduled reboot/LED, client naming and kick, telemetry charts, event log, config import/export and diagnostics.
- **Two runtime profiles**: `full` (Docker default, everything on) and `openwrt` (trims telemetry history and firmware storage, caps config revisions and events, and limits Go memory).

The embedded UI has 8 pages: Devices, Clients, Telemetry, Wireless, RF, Ports, Firmware and System (login password, shared secret, NTP, audit log and backups).

## 3. Architecture & ports

```
Browser ──HTTP──▶ AC (single binary: Web UI + REST + SQLite; TCP 8090 / UDP 29820)
                   ▲ AP-initiated: discovery → hello (20s) → signed bundle pull
                   │
AP: ac-agent ──────┘  verify → UCI segment merge → reload → self-check → rollback
                   └── UDP 29821 (optional) ──▶ other APs: 11k neighbours, roaming hints
```

| Port | Protocol | Purpose |
|---|---|---|
| TCP 8090 | TCP | Web UI and REST API |
| UDP 29820 | UDP | AP broadcast discovery (works with host networking) |
| UDP 29821 | UDP | AP-to-AP coordination, optional (`peer_port 0` disables it) |

> `network_mode: host` is **mandatory** for Docker: discovery uses UDP broadcast, which an isolated bridge subnet cannot receive — the AP stays in `discovering`. With host mode no `ports:` mapping is needed. `init: true` makes SIGTERM reach the Go process for a graceful shutdown.
>
> Host networking depends on a Linux host. On Docker Desktop (Windows/macOS), fill in "Controller address" on the AP in LuCI instead (see section 8).

## 4. Quick start with Docker

Image: [Docker Hub · naoki66/openwrt_ac-controller](https://hub.docker.com/r/naoki66/openwrt_ac-controller).

**1. Prepare the data directory; it must be owned by 1000:1000** (the image runs as uid 1000)

```sh
mkdir -p data && chown 1000:1000 data
```

**2. Write `docker-compose.yml`**: `AC_SHARED_SECRET` is optional — set your own secret, or leave it empty and configure it later from the Web System page. The compose file keeps only what a deployment actually needs; every other variable has a built-in default, and settings such as NTP servers and the login password can be changed and persisted from the System page — there is no need to put them in compose. Section 6 lists every variable. This is the same file as [`controller/deploy/docker-compose.yml`](controller/deploy/docker-compose.yml) in the repo (only the placeholder text is translated).

```yaml
services:
  ac:
    image: naoki66/openwrt_ac-controller:latest
    container_name: ac-controller
    restart: unless-stopped
    init: true
    network_mode: host
    environment:
      TZ: Asia/Shanghai
      # Shared secret (optional; leave empty to set it in the Web UI)
      AC_SHARED_SECRET: ""
      AC_ADMIN_USER: "admin"
      AC_ADMIN_PASSWORD: "${AC_ADMIN_PASSWORD:-}"
    volumes:
      - ./data:/var/lib/ac
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
```

If 8090 / 29820 are taken, add `AC_HTTP_ADDR` / `AC_DISCOVER_ADDR` to override the defaults (see section 6).

**3. Start and verify**

```sh
docker compose up -d
curl -s http://<HOST_IP>:8090/api/v1/health
# expect {"status":"ok","version":...,"commit":...,"devices":0,...}
```

Set `AC_ADMIN_PASSWORD` to at least 12 characters in your local Compose environment before first start. Open `http://<HOST_IP>:8090/` and log in as `admin` with that password. Then install the AP agent (section 8).

## 5. Other deployment options

| Option | Use when | See |
|---|---|---|
| Docker Hub image (recommended) | NAS, x86 soft-router, ARM board with internet | Section 4 |
| OpenWrt package (experimental) | Run the controller on a router without Docker | Below |

### Run the AC on an OpenWrt router (experimental)

> **Experimental — not yet released.** This form packages the same binary as `ac-controller`, managed by procd, with the Web UI still served by the binary itself. Estimated footprint **XXX MB RAM / XXX CPU** (to be measured; the Go memory cap is currently 64MiB, which is not a measured footprint). To fit ARM routers, telemetry history, firmware storage, config-revision caps and the event-log cap have been trimmed (the `AC_PROFILE=openwrt` preset), along with a cap on Go runtime memory. It will be released after optimisation; do not use it on a production network yet.

## 6. Configuration

The compose file lists only four common deployment settings. The table below is the **complete** list of variables and their built-in defaults; anything not in compose uses its default. The shared secret, NTP list and login password can be changed and persisted from the System page (a saved UI value wins over the environment). Add a variable to compose only when you need to change a port, switch profiles or tune behaviour.

<details>
<summary>All 19 environment variables and defaults</summary>

| Variable | Default | Description |
|---|---|---|
| `AC_SHARED_SECRET` | empty (**optional**) | Shared by the AC and all APs; leave it empty and set or rotate it from the System page (≥12 characters when set). A saved UI value wins over the environment |
| `AC_ADMIN_USER` | `admin` | Initial login user, **seeded once on first boot** |
| `AC_ADMIN_PASSWORD` | empty | Required for first startup or migration from the old default password, at least 12 characters. On OpenWrt use `ac-controller.controller.admin_password` |
| `AC_ADMIN_TOKEN` | empty | Bearer credential for automation; must differ from the shared secret |
| `AC_HTTP_ADDR` | `:8090` | Web UI / API listen address; override only on a port conflict |
| `AC_HTTP_PORT` | derived from `AC_HTTP_ADDR` (`8090`) | Port advertised to APs |
| `AC_DISCOVER_ADDR` | `:29820` | UDP discovery listen address |
| `AC_ADVERTISE_IP` | empty | Force the AC address announced in AC_OFFER |
| `AC_DATA_DIR` | `./data` (`/var/lib/ac` in the image) | SQLite, AC identity and signing key; already set inside the image |
| `AC_PROFILE` | `full` | `full` (everything) or `openwrt` (flash-friendly) |
| `AC_NTP_SERVERS` | 4 public NTP sources | Time sources handed to APs; set them directly in the System page |
| `AC_OFFLINE_AFTER` | `90s` | Heartbeat age at which an AP is reported offline |
| `AC_SAMPLE_INTERVAL` | `60s` (`120s` on `openwrt`) | Telemetry sampling cadence |
| `AC_SAMPLE_RETENTION` | `168h` (`24h` on `openwrt`) | Telemetry retention |
| `AC_TELEMETRY_HISTORY` | `true` (`false` on `openwrt`) | Store history for the charts |
| `AC_FIRMWARE` | `true` (`false` on `openwrt`) | Enable the firmware repository |
| `AC_PACKAGE_HISTORY` | `0` unlimited (`1` on `openwrt`) | Signed revisions kept per AP |
| `AC_EVENTS_MAX` | `0` unlimited (`500` on `openwrt`) | Audit-log cap |
| `TZ` | `Asia/Shanghai` | Timezone for scheduled reboot / LED HH:MM values |

</details>

> After first boot the credentials live in SQLite and changing the env vars has **no** effect. The shared secret and NTP list can be rotated from the System page and persist there; a saved UI value wins over the environment. To change listen ports, add `AC_HTTP_ADDR` / `AC_DISCOVER_ADDR` and keep `AC_HTTP_PORT` in sync.

## 7. Upgrade

Data lives in the host `data/` directory and survives container recreation.

```sh
docker compose pull && docker compose up -d
```

`latest` is a rolling tag. Pin a release tag such as `naoki66/openwrt_ac-controller:v0.5.1`, or use a `sha-<short-commit>` tag to trace back to a specific build, when you need reproducibility.

## 8. Install the AP agent

The image contains only the **AC side**; every AP also needs `ac-agent`.

> Two packages are needed on the AP: `ac-agent` (the agent) and `luci-app-ac-agent` (the LuCI page). Their versions are listed under option C. Pick one of the three installation options.

### Option A: Flash the companion firmware (easiest)

For the Brightspeed/Gemtek XR1710G: the [ImmortalWrt firmware releases](https://github.com/naoki66/ImmortalWrt-for-Gemtek-brightspeed/releases) include the agent and LuCI page. Update the agent to a version matching the controller and provision it as described below.

### Option B: Build ipk/apk from the OpenWrt feed

```sh
src-git --root=agent/openwrt ac_agent https://github.com/naoki66/OpenWrt_AC-Controller.git
./scripts/feeds update ac_agent
./scripts/feeds install ac-agent luci-app-ac-agent
# select the packages in menuconfig, build, then install on the AP
```

Both packages set `PKG_SOURCE_VERSION` to the default branch `main` instead of a pinned commit, so agent changes need no Makefile edit. Three things to keep in mind:

- OpenWrt names the source tarball `<name>-<PKG_VERSION>.tar.zst` (`include/download.mk`), so an existing `dl/` cache is reused verbatim. Bump `PKG_VERSION` / `PKG_RELEASE` for a release, or delete `dl/ac-agent-*.tar.zst` and `dl/ac-controller-*.tar.zst` to force a fresh clone.
- `PKG_MIRROR_HASH:=skip` must stay (the branch tip moves, so the tarball is not reproducible), and neither package injects a concrete commit into the binary any more. Each Release ships `build-info.txt` with the exact source commit.
- To reproduce an older build locally, override it: `make ... PKG_SOURCE_VERSION=<full commit SHA>`.

### Option C: Download ipk/apk packages from Releases

`ac-agent`, `luci-app-ac-agent` and the generated `luci-i18n-ac-agent-zh-cn` are built with the official OpenWrt SDK containers by this repository's [OpenWrt Packages workflow](https://github.com/naoki66/OpenWrt_AC-Controller/actions/workflows/openwrt-packages.yml) and attached to each Release. AP-side package versions match the feed Makefiles; download the ones from the same Release as your controller:

| Package | Version | Source | Contents |
|---|---|---|---|
| `ac-agent` | `0.2.1-1` | `main` (unpinned) | Agent binary, procd init script, `/etc/config/ac` |
| `luci-app-ac-agent` | `1.0.0-5` | in-tree | LuCI page and the `luci-i18n-ac-agent-zh-cn` catalogue |

The optional AC-side package `ac-controller` is `0.1.0-1` and follows `main` as well; see section 5 (experimental; the workflow skips it unless `build_controller` is enabled).

The default matrix publishes only `25.12.6` `.apk` for `aarch64_cortex-a53`; `x86_64`, `mipsel_24kc` and all 24.10 `.ipk` targets have been dropped. For other architectures or `.ipk`, run the [OpenWrt Packages workflow](https://github.com/naoki66/OpenWrt_AC-Controller/actions/workflows/openwrt-packages.yml) manually and edit the `targets` input. Every Release also carries `build-info.txt` (source commit, Go version, SDK tag list) and `SHA256SUMS`.

```sh
# 1. On a networked machine, download the packages for your controller release
#    (replace <RELEASE_TAG> with the actual tag)
#    https://github.com/naoki66/OpenWrt_AC-Controller/releases
gh release download <RELEASE_TAG> -R naoki66/OpenWrt_AC-Controller \
  -p '*ac-agent*' -D ./pkgs
scp ./pkgs/* root@<AP_IP>:/tmp/

# 2. On the AP (25.12 / snapshots use apk)
apk add --allow-untrusted /tmp/*ac-agent*.apk
```

> The package-manager split follows upstream OpenWrt: **24.10 and older use opkg / `.ipk`; 25.12 and snapshots use apk / `.apk`**. The current Release ships `.apk` only. Packages are published in this repository's [Releases](https://github.com/naoki66/OpenWrt_AC-Controller/releases); use assets from the same Release as your controller. `apk` refuses unsigned packages by default, so local files need `--allow-untrusted`. For an older firmware that needs `.ipk`, run the workflow manually with a 24.10 SDK target.

### Configure the AP in LuCI

Get the signing public key and issue a device key on the AC. Replace the example MAC with the one shown by `ac-agent status` on the AP:

```sh
# Release image (use /usr/local/bin/ac in the local development image)
docker compose exec ac /ac pubkey
docker compose exec ac /ac ap-key 02:00:00:00:00:01

# OpenWrt controller package: use the running service's data directory
AC_DATA_DIR=/etc/ac-controller /usr/sbin/ac-controller pubkey
AC_DATA_DIR=/etc/ac-controller /usr/sbin/ac-controller ap-key 02:00:00:00:00:01
```

Provision the complete public key and device key through a trusted channel. Use the LuCI fields below or UCI options `ac_public_key` and `device_key`. All APs use the same AC public key, and each AP has a different device key. Preserve the AC data directory and signing key when upgrading. Provision credentials before switching versions: legacy agents cannot authenticate to the new AC. Replacing the signing key requires provisioning the new public key on the APs. Use `ap-key-rotate <MAC>` to issue a replacement device key or `ap-key-revoke <MAC>` to revoke it.

Open LuCI → `Control` → `AC Agent`, fill in "Controller connection", and click **Save & apply**:

| Field | What to enter | Required |
|---|---|---|
| Enable | Leave it on (default). This starts the agent and enables it at boot | Default is fine |
| Controller address | **Leave empty** on the same broadcast domain — UDP discovery fills it in and you can save it to pin the address; set `<AC_IP>:8090` across L3 | Required across L3 |
| AC signing public key | Output of `pubkey` | Required |
| AP device key | Output of `ap-key` for this AP | Required |
| Peer coordination shared secret | The same key on APs in the group (at least 12 characters); blank disables peer coordination | When using peer coordination |

Everything else can stay as-is: heartbeat 20s, peer coordination port 29821, local roam suggestion on by default. The status card at the top shows `discovering` / `connected` / `managed`; click **Restart agent** to force an immediate sync.

For offline roaming, set `ac.agent.peer_network` on each AP to the logical OpenWrt network that reaches the other APs on the same management VLAN, and use the same peer key. That network might be `lan3` instead of the default `lan`. LuCI and `ac-agent status` show the resolved device, transport and peer AP/BSS counts; a successful AC heartbeat does not verify the peer path.

On the AP, `AC_SECRET="$(uci -q get ac.agent.secret)" ac-agent peers --peer-network lan3 --listen 45s` passively checks for authenticated peers and matching SSIDs without changing Wi-Fi or sending roam suggestions.

OpenWrt's hostapd handles 802.11k/v/r frames natively. 11k needs valid neighbor reports for the same SSID, 11v depends on the client accepting the advice, and 11r requires matching SSID, security, mobility domain and FT key configuration. Verify roaming and FT with two APs and a test client before tuning thresholds. Keep forced disassociation off initially and use only one active steering system if usteer or DAWN is installed.

## 9. Onboarding flow

1. In LuCI, enter the AC signing public key and this AP's device key, then enable and save. The status card should move from `discovering` to `connected`.
2. The device appears as `pending` on the AC's Devices page → pick a template → **adopt**.
3. The AP pulls rev1 on its next heartbeat and applies it.
4. **Repush once more**: the first revision is built before the AC has received interface/BSS reports, so only rev2 carries sibling APs' 11k neighbour table.

Two rules that prevent a lockout:

- Rehearse the first takeover with `ac-agent once --dry-run -v`: it renders, verifies and stages only, without touching `/etc`.
- For a full network takeover, **set the management address / ports / VLAN first** (`PUT /devices/{mac}/network` or the UI), then adopt the template. The UI runs a `preflight` check to warn about address moves before pushing.
- For DFS channels, set the AP's `ac.agent.health_timeout` to allow the local CAC wait (default `45s`, maximum `15m`). Apply waits for the target BSSes to reach `ENABLED` in hostapd.

`ac-agent status` states: `discovering` (no AC address), `unreachable` (heartbeat failed), `connected` (not adopted), `managed` (adopted).

## 10. API quick reference

Management endpoints require a login session cookie or `Authorization: Bearer <AC_ADMIN_TOKEN>`. `/api/v1/health`, `/api/v1/ac/pubkey` and `/api/v1/ap/hello` are the exceptions.

```sh
# Log in and keep the session cookie
curl -s -c ac.jar -X POST -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"<your-admin-password>"}' http://<AC_IP>:8090/api/v1/auth/login
curl -s -b ac.jar http://<AC_IP>:8090/api/v1/devices
```

<details>
<summary>REST API endpoint reference</summary>

| Endpoint | Purpose |
|---|---|
| `GET /api/v1/health` | Health, version, commit, device count, AC public key |
| `GET/POST /api/v1/devices`, `.../{mac}` | Device list and detail |
| `POST /api/v1/devices/{mac}/adopt` · `repush` · `release` | Adopt / repush / release |
| `GET /api/v1/devices/{mac}/preflight` | Pre-push management-address check |
| `PUT /api/v1/devices/{mac}/network` · `radios` · `group` | Per-device network / radio / group overrides |
| `GET /api/v1/devices/{mac}/package` | Download a signed bundle (AP authenticates and decrypts with its device key; operator uses a session) |
| `GET/POST /api/v1/templates` · `clone` · `ssids` · `name` | Template management |
| `GET/POST /api/v1/groups` | AP groups |
| `GET /api/v1/clients` · `PUT/POST .../{mac}/kick` | Clients, rename, kick |
| `GET /api/v1/radios` · `ports` · `telemetry/*` | RF, ports, telemetry |
| `POST /api/v1/devices/{mac}/actions` · `PUT .../reboot-schedule` · `led-schedule` | One-shot commands and schedules |
| `GET/POST /api/v1/firmwares` · `POST /api/v1/devices/{mac}/firmware` | Firmware repository and upgrade target |
| `GET /api/v1/events` · `diagnostics` · `config/export` · `config/import` | Audit log, diagnostics, config backup |
| `PUT /api/v1/settings/shared-secret` · `ntp` · `PUT /api/v1/auth/account` | Rotate the secret / NTP / account |

</details>

See [`controller/internal/api/server.go`](controller/internal/api/server.go) for the full route list. `GET /api/v1/features` reports which optional subsystems the running profile exposes.

## 11. Security notes

- The AC serves HTTP only. AP configuration bundles are encrypted separately, while browser login, management API calls and heartbeat telemetry remain plaintext. Use an isolated management VLAN and do not expose port 8090 to the internet or WAN.
- APs authenticate with individual device keys and single-use challenges. They verify replies and bundles with the preprovisioned public key. The optional shared secret is for AP peer coordination.
- `ac.db`, `/etc/ac/backup/`, config exports, the signing key and firmware images all contain sensitive data. Protect them equally and never paste them into an issue.
- Firmware upgrades and network pushes rewrite critical AP config: rehearse with dry-run / preflight and keep a serial console or local management port as a fallback.

## 12. Troubleshooting

<details>
<summary>Common symptoms and checks</summary>

| Symptom | Check first |
|---|---|
| AP stuck in `discovering` | Is `network_mode: host` set; is AC listening on UDP 29820; same broadcast domain; is "Controller address" filled in LuCI across L3 |
| 401 every 20s | Does "Shared secret" in LuCI match the effective secret on the AC (environment or System page); was it rotated in the UI (a saved UI value wins) |
| Container fails / DB errors | Does `./data` exist and is it owned by `1000:1000` |
| `connected` but no revision | Device is still `pending` and has not been adopted |
| `sync_state=rollback` | `sync_message`, `/etc/ac/backup/`, `logread` — usually a BSS or management-address self-check failed |
| SSH drops after a push | Was the address move checked by `preflight`; firewall zone / dropbear binding (newer versions repair these automatically) |
| Apply succeeded but 11k is empty | Did you repush a second time |
| 11r enabled in the template but the badge stays grey | Badges are measured at runtime: wait for the next heartbeat (the probe is cached for 5 minutes); if still grey, `cat /var/run/hostapd-*.conf` and check that every BSS section has `ieee80211r=1`; roaming across APs additionally requires the same `mobility_domain` on all of them |
| UI reports the AP offline | Is `last_seen_age` > 90s; is the AP clock drifting (signature checks depend on time) |

</details>

## 13. Versioning & releases

- A semantic tag `vX.Y.Z` maps to an image tag of the same name; `latest` rolls, and `sha-<short-commit>` lets you pin a build.
- Always trust `health.version` / `health.commit` from `GET /api/v1/health`, or `ac version`, over the image tag.
- See [Releases](https://github.com/naoki66/OpenWrt_AC-Controller/releases) and [Tags](https://github.com/naoki66/OpenWrt_AC-Controller/tags).
- Image status: [Docker Hub tags](https://hub.docker.com/r/naoki66/openwrt_ac-controller/tags) lists every tag and architecture (linux/amd64, linux/arm64); the badges above show the latest published version and last update.

## 14. Feedback, license & links

Issues and PRs are welcome. When reporting a problem, include: `ac version`, the `/api/v1/health` output, `ac-agent status`, relevant `logread | grep ac-agent` lines, reproduction steps, and your deployment form (Docker / OpenWrt package). **Never paste the shared secret, PSKs, `ac.db`, config exports or the signing key.**

- License: [MIT](LICENSE) © 2026 naoki66
- Repository: <https://github.com/naoki66/OpenWrt_AC-Controller>
- Image: [Docker Hub · naoki66/openwrt_ac-controller](https://hub.docker.com/r/naoki66/openwrt_ac-controller)
- Companion AP firmware: [ImmortalWrt for XR1710G](https://github.com/naoki66/ImmortalWrt-for-Gemtek-brightspeed/releases)

## 15. Sponsor

If this project helps you, you can buy the author a coffee ☕

<img src="sponsor-wechat.png" alt="WeChat sponsor QR code" width="300">

**In one sentence**: the AC holds desired state; each AP pulls, merges, applies, self-checks and rolls back locally — a dead controller never kills the Wi-Fi, and a bad config never locks an AP out unattended.
