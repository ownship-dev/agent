# Ownship agent

The agent that [Ownship](https://ownship.dev) runs on your machine. It pairs with the hosted
control plane, then holds a single **outbound** WebSocket to it and receives commands over that
stream: nothing on your network is opened inbound.

This repository is a read-only mirror of `apps/agent` from the Ownship monorepo, published so you
can audit exactly what runs on your machine. Issues and pull requests are not monitored here.

## What it does

- Builds apps with Nixpacks, or runs a prebuilt image.
- Runs them in Proxmox LXCs or local Docker / Podman (rootless included), with GPU passthrough.
- Health-checks a deploy before routing traffic and configures Cloudflare Tunnel, LAN or
  Tailscale ingress.
- Streams build and runtime logs, metrics and hardware health back to the control plane.
- Provides the web terminal, managed database consoles and sealed (AES-256-GCM) backups.

## Security model

- Every command runs as an argument list, never through a shell, so input can't become a command.
- Secrets are written to `0600` files, never passed on the command line.
- Backups are encrypted on the agent; storage only ever holds ciphertext.
- Self-update only installs a release signed with Ownship's ed25519 key compiled into the
  binary, over HTTPS, and never downgrades. The key is in `internal/updater/updater.go`.

The full picture, including what keeps running when the control plane is down, is at
<https://ownship.dev/docs/trust>.

## Verify a release

Release public key: `5uvfn0cZ7/bKfnJ/nRBNox1/fQ7LlPr9B7nS0oq3QZQ=`

```sh
curl -fsSLO https://assets.ownship.dev/agent/releases/latest/checksums.txt
curl -fsSLO https://assets.ownship.dev/agent/releases/latest/checksums.txt.sig
node -e '
const c = require("crypto"), fs = require("fs");
const key = c.createPublicKey({ format: "jwk", key: { kty: "OKP", crv: "Ed25519",
  x: Buffer.from("5uvfn0cZ7/bKfnJ/nRBNox1/fQ7LlPr9B7nS0oq3QZQ=", "base64").toString("base64url") } });
const sig = Buffer.from(fs.readFileSync("checksums.txt.sig", "utf8").trim(), "base64");
console.log(c.verify(null, fs.readFileSync("checksums.txt"), key, sig) ? "signature OK" : "MISMATCH");'
sha256sum --check --ignore-missing checksums.txt
```

## Install

```sh
curl -fsSL https://assets.ownship.dev/install-agent.sh | sh
```

The script downloads the binary for your platform, verifies its checksum, pairs it with the code
shown in the dashboard and installs a systemd service. To pair by hand: `ownship-agent pair <CODE>`.

## Build from source

Requires Go 1.26.

```sh
make build        # ./ownship-agent for this machine
make build-all    # dist/agent-{linux-amd64,linux-arm64,darwin-arm64} + checksums.txt
make test
```

A build from source has no release key override and still verifies updates against the official
key. Set `ORCHESTRATOR=proxmox` on a Proxmox host; the default is `local` (Docker).

## License

[Apache License 2.0](LICENSE).
