# 1.<commits on HEAD>: orderable, so the updater's downgrade guard works. Needs full history.
VERSION ?= $(shell n=$$(git rev-list --count HEAD 2>/dev/null) && echo 1.$$n || echo dev)
# Base64 ed25519 public key that signs releases. The real one is the compiled-in
# default in updater.go — leave this EMPTY unless you run your own release bucket
# signed with a different key. It must stay conditional below: an unconditional
# -X with an empty value would blank the default and disable update verification.
RELEASE_PUBKEY ?=
# The hosted control plane, baked into official builds. Overridden here only to
# point release builds at prod vs staging — never a user-facing knob.
CONTROL_PLANE_URL ?= https://gateway.ownship.dev
LDFLAGS := -s -w \
	-X github.com/ownship/agent/internal/config.Version=$(VERSION) \
	-X github.com/ownship/agent/internal/config.ControlPlaneURL=$(CONTROL_PLANE_URL)
ifneq ($(strip $(RELEASE_PUBKEY)),)
LDFLAGS += -X github.com/ownship/agent/internal/updater.ReleasePublicKey=$(RELEASE_PUBKEY)
endif
BIN := ownship-agent
# Release artifact basename, matching the bucket object <REL>-<os>-<arch> at
# assets.ownship.dev/agent/releases/<version>/.
REL := agent
DIST := dist

.PHONY: build install build-all clean test

# Build for the host platform.
build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) .

# Build + install to /usr/local/bin (run on the homelab host).
install: build
	install -m 0755 $(BIN) /usr/local/bin/$(BIN)

# Cross-compile release binaries the install script downloads.
build-all:
	@mkdir -p $(DIST)
	GOOS=linux  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(REL)-linux-amd64 .
	GOOS=linux  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(REL)-linux-arm64 .
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(REL)-darwin-arm64 .
	@cd $(DIST) && shasum -a 256 $(REL)-* > checksums.txt
	@echo "$(VERSION)" > $(DIST)/version.txt

test:
	go test ./...

clean:
	rm -rf $(BIN) $(DIST)
