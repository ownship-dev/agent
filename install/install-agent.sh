#!/usr/bin/env sh
# Install the ownship machine agent as a systemd service. Run on the host.
#
#   curl -fsSL https://assets.ownship.dev/install-agent.sh | sh -s -- pair <CODE>   # install + pair + start
#   curl -fsSL https://assets.ownship.dev/install-agent.sh | sh                     # install only
#   curl -fsSL https://assets.ownship.dev/install-agent.sh | sh -s -- --uninstall   # remove the agent
#
# Re-running the installer is a clean reinstall: if an agent is already present it
# is fully overridden — the old binary, service and ALL credentials (token) are
# wiped before the new agent is laid down, so pass `pair <CODE>` again to re-pair.
# The workloads it deployed and its map of them are preserved. For an in-place binary
# upgrade that keeps pairing, use `ownship-agent update` or the hourly auto-update instead.
#
# Proxmox is detected automatically (LXC per project, no Docker on the host).
# Force a mode with ORCHESTRATOR=local or ORCHESTRATOR=proxmox.
#
# CONTAINER_ENGINE=docker|podman|podman-rootless readies this host for that engine
# (default docker); the agent always reports every engine it actually finds.
#
# Self-contained: downloads the binary, (optionally) pairs, writes a systemd
# unit and enables it — nothing to configure or start by hand.
set -eu

DEST="${DEST:-/usr/local/bin/ownship-agent}"
TOKEN_PATH="/etc/ownship/token"
UNIT="/etc/systemd/system/ownship-agent.service"
# VERSION is a specific release (e.g. 1.4.0) or "latest".
VERSION="${VERSION:-latest}"
RELEASE_BASE="https://assets.ownship.dev/agent/releases/$VERSION"

# Root is needed for /usr/local/bin, /etc and systemctl; use sudo when not root.
SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  if command -v sudo >/dev/null 2>&1; then
    SUDO="sudo"
  else
    echo "run as root, or install sudo" >&2
    exit 1
  fi
fi

# Remove the binary, service and token, never the workloads (user data). Only "all" (--uninstall)
# drops the state dir: a reinstall needs its map of which LXC holds which workload.
purge_agent() {
  $SUDO systemctl stop ownship-agent 2>/dev/null || true
  $SUDO systemctl disable ownship-agent 2>/dev/null || true
  $SUDO rm -f "$UNIT"
  $SUDO rm -rf "${UNIT}.d"
  $SUDO systemctl daemon-reload 2>/dev/null || true
  $SUDO systemctl reset-failed ownship-agent 2>/dev/null || true
  $SUDO rm -f "$DEST" "${DEST}.bak" "${DEST}.new"
  $SUDO rm -rf /etc/ownship
  if [ "${1:-}" = "all" ]; then
    $SUDO rm -rf /root/.ownship /var/lib/ownship
  else
    $SUDO rm -f /root/.ownship/token /var/lib/ownship/.ownship/token
  fi
}

# Whether any trace of a prior install is present (binary, service, or token).
agent_present() {
  [ -f "$DEST" ] || [ -f "$UNIT" ] || $SUDO test -f "$TOKEN_PATH" 2>/dev/null
}

# Uninstall: purge everything and stop.
if [ "${1:-}" = "--uninstall" ]; then
  echo "Uninstalling ownship-agent..."
  purge_agent all
  echo "Done — agent, service, token and local state removed."
  echo "Containers/LXCs the agent deployed were left running (they're your data)."
  exit 0
fi

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64) arch="amd64" ;;
  aarch64 | arm64) arch="arm64" ;;
  *) echo "unsupported arch: $arch" >&2; exit 1 ;;
esac

bin="agent-${os}-${arch}"
echo "Downloading $bin..."
tmp="$(mktemp)"
curl -fsSL "$RELEASE_BASE/$bin" -o "$tmp"

# Verify against published checksums when available.
if curl -fsSL "$RELEASE_BASE/checksums.txt" -o "${tmp}.sums" 2>/dev/null; then
  want="$(grep "$bin" "${tmp}.sums" | awk '{print $1}')"
  got="$( (shasum -a 256 "$tmp" 2>/dev/null || sha256sum "$tmp") | awk '{print $1}')"
  if [ "$want" != "$got" ]; then
    echo "checksum mismatch, aborting" >&2
    exit 1
  fi
fi

# Override an existing install: a re-run is a clean reinstall — wipe the old
# binary, service and credentials, then lay everything down fresh. Done only
# now (after the new binary is downloaded + verified) so a failed download never
# leaves the host with nothing.
if agent_present; then
  echo "Existing ownship-agent found — overriding (removing old binary + credentials)..."
  purge_agent
fi

$SUDO install -m 0755 "$tmp" "$DEST"
rm -f "$tmp" "${tmp}.sums"
echo "Installed to $DEST"

# Pair (forwarded args, e.g. `pair <CODE>`). The token lands in a system path so
# the service — which runs as root — reads the same file.
$SUDO mkdir -p /etc/ownship
paired=0
if [ "$#" -gt 0 ]; then
  echo "Pairing..."
  $SUDO env AGENT_TOKEN_PATH="$TOKEN_PATH" "$DEST" "$@"
  paired=1
elif $SUDO test -f "$TOKEN_PATH"; then
  paired=1
fi

# Orchestrator: auto-detected — a host with the PVE CLIs is a Proxmox node, so
# the agent manages one LXC per project (no Docker on the host); anything else
# runs local Docker. ORCHESTRATOR=local|proxmox overrides the detection. The
# per-project LXC template is picked when creating a project, not here; the
# optional PROXMOX_* knobs below only tune where/how the pct-created LXC lives.
if [ -z "${ORCHESTRATOR:-}" ] && command -v pct >/dev/null 2>&1 && command -v pvesh >/dev/null 2>&1; then
  ORCHESTRATOR=proxmox
  echo "Proxmox detected — the agent will manage one LXC per project."
fi
ORCH_LINES=""
if [ -n "${ORCHESTRATOR:-}" ]; then
  ORCH_LINES="Environment=ORCHESTRATOR=$ORCHESTRATOR"
  [ -n "${PROXMOX_STORAGE:-}" ] && ORCH_LINES="$ORCH_LINES
Environment=PROXMOX_STORAGE=$PROXMOX_STORAGE"
  [ -n "${PROXMOX_BRIDGE:-}" ] && ORCH_LINES="$ORCH_LINES
Environment=PROXMOX_BRIDGE=$PROXMOX_BRIDGE"
  [ -n "${PROXMOX_ROOTFS_GB:-}" ] && ORCH_LINES="$ORCH_LINES
Environment=PROXMOX_ROOTFS_GB=$PROXMOX_ROOTFS_GB"
fi

# Container engine: Podman (rootful or rootless) is set up here; Docker is the user's own
# install, as before. On Proxmox the engine is installed inside each LXC instead.
CONTAINER_ENGINE="${CONTAINER_ENGINE:-docker}"
COMPOSE_VERSION="v2.32.4"
case "$CONTAINER_ENGINE" in
  docker|podman|podman-rootless) ;;
  *) echo "CONTAINER_ENGINE must be docker, podman or podman-rootless" >&2; exit 1 ;;
esac
if [ "${ORCHESTRATOR:-local}" = "proxmox" ] && [ "$CONTAINER_ENGINE" != "docker" ]; then
  [ "$CONTAINER_ENGINE" = "podman-rootless" ] && { echo "podman-rootless is not offered on Proxmox; the LXC is already unprivileged" >&2; exit 1; }
  echo "Proxmox installs Podman inside each LXC that uses it; nothing to set up on the host."
elif [ "$CONTAINER_ENGINE" != "docker" ]; then
  if ! command -v podman >/dev/null 2>&1; then
    echo "Installing Podman..."
    if command -v apt-get >/dev/null 2>&1; then
      $SUDO env DEBIAN_FRONTEND=noninteractive apt-get update -y -q
      $SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y -q podman passt uidmap
    elif command -v dnf >/dev/null 2>&1; then
      $SUDO dnf install -y podman passt
    else
      echo "install podman first (no apt-get or dnf found)" >&2; exit 1
    fi
  fi
  # Podman has no docker CLI to host the compose plugin; the standalone v2 binary speaks to its socket.
  if ! command -v docker-compose >/dev/null 2>&1 || ! docker-compose version 2>/dev/null | grep -q 'v2\.'; then
    echo "Installing docker-compose $COMPOSE_VERSION..."
    $SUDO curl -fsSL -o /usr/local/bin/docker-compose \
      "https://github.com/docker/compose/releases/download/$COMPOSE_VERSION/docker-compose-linux-$(uname -m)"
    $SUDO chmod 0755 /usr/local/bin/docker-compose
  fi
  $SUDO systemctl enable --now podman.socket podman-restart.service
fi
if [ "$CONTAINER_ENGINE" = "podman-rootless" ] && [ "${ORCHESTRATOR:-local}" != "proxmox" ]; then
  PV=$(podman version --format '{{.Client.Version}}')
  case "$PV" in
    5.[3-9]*|5.[1-9][0-9]*|[6-9].*) ;;
    *) echo "rootless Podman needs version 5.3 or newer (found $PV)" >&2; exit 1 ;;
  esac
  if ! id ownship-podman >/dev/null 2>&1; then
    $SUDO useradd --system --create-home --home-dir /var/lib/ownship-podman --shell /usr/sbin/nologin ownship-podman
  fi
  # 65536 ids above every range already handed out, so no two users share a host uid.
  for f in /etc/subuid /etc/subgid; do
    if ! grep -q '^ownship-podman:' "$f" 2>/dev/null; then
      start=$(awk -F: '{e=$2+$3; if (e>m) m=e} END {print (m>100000 ? m : 100000)}' "$f" 2>/dev/null || echo 100000)
      echo "ownship-podman:$start:65536" | $SUDO tee -a "$f" >/dev/null
    fi
  done
  $SUDO loginctl enable-linger ownship-podman
  PU=$(id -u ownship-podman)
  for i in 1 2 3 4 5; do [ -S "/run/user/$PU/bus" ] && break; sleep 1; done
  $SUDO runuser -u ownship-podman -- env HOME=/var/lib/ownship-podman XDG_RUNTIME_DIR="/run/user/$PU" \
    systemctl --user enable --now podman.socket podman-restart.service
  echo "Rootless Podman ready: workloads run as the ownship-podman user."
fi

# Write the systemd unit (baked with the resolved paths).
echo "Installing service at $UNIT..."
$SUDO tee "$UNIT" >/dev/null <<EOF
[Unit]
Description=Ownship machine agent
After=network-online.target docker.service podman.socket
Wants=network-online.target

[Service]
# systemd doesn't set \$HOME; the agent needs it to resolve its state dir.
Environment=HOME=/root
# systemd's default PATH omits /snap/bin and /usr/local/bin, where docker often
# lives — the agent shells out to \`docker\`, so make sure it's findable.
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin
Environment=AGENT_TOKEN_PATH=$TOKEN_PATH
$ORCH_LINES
ExecStart=$DEST run
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  $SUDO systemctl daemon-reload
  if [ "$paired" -eq 1 ]; then
    $SUDO systemctl enable --now ownship-agent
    echo
    echo "Done — ownship-agent is running and will start on boot."
    echo "Logs: $SUDO journalctl -u ownship-agent -f"
  else
    echo
    echo "Service installed. Pair to start it (get the code from the dashboard):"
    echo "  curl -fsSL https://assets.ownship.dev/install-agent.sh | sh -s -- pair <CODE>"
  fi
else
  echo
  echo "systemd not detected — run the agent yourself: $DEST run"
fi
