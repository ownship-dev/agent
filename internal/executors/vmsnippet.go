package executors

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// The storage the vendor snippet lives on: every Proxmox node has a directory-backed "local".
const snippetStorage = "local"

const vendorSnippetName = "ownship-vendor.yaml"

// vendorSnippet is cloud-init vendor data, fixed at build time: no user text ever reaches it.
// Many cloud images ship without the guest agent the VM layer depends on.
const vendorSnippet = `#cloud-config
packages:
  - qemu-guest-agent
runcmd:
  - [systemctl, enable, --now, qemu-guest-agent]
`

var reStorageContent = regexp.MustCompile(`^[a-z]+(,[a-z]+)*$`)

// storageConfig reads a storage's definition; tests replace it.
var storageConfig = func(ctx context.Context, storage string) ([]byte, error) {
	return exec.CommandContext(ctx, "pvesh", "get", "/storage/"+storage, "--output-format", "json").Output()
}

// setStorageContent replaces a storage's content types; tests replace it.
var setStorageContent = func(ctx context.Context, storage, content string) ([]byte, error) {
	return exec.CommandContext(ctx, "pvesm", "set", storage, "--content", content).CombinedOutput()
}

var vendorSnippetMu sync.Mutex

// ensureVendorSnippet writes the snippet and returns its volume id for qm's --cicustom. It
// adds "snippets" to the storage's content types once, keeping the ones already there.
func (o *ProxmoxLXC) ensureVendorSnippet(ctx context.Context) (string, error) {
	vendorSnippetMu.Lock()
	defer vendorSnippetMu.Unlock()
	out, err := storageConfig(ctx, snippetStorage)
	if err != nil {
		return "", fmt.Errorf("read storage %s: %s", snippetStorage, execErrDetail(err))
	}
	var cfg struct {
		Type    string `json:"type"`
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return "", fmt.Errorf("unreadable storage %s config", snippetStorage)
	}
	if cfg.Type != "dir" || !filepath.IsAbs(cfg.Path) || strings.Contains(cfg.Path, "..") {
		return "", fmt.Errorf("storage %s is not a directory storage", snippetStorage)
	}
	if !strings.Contains(","+cfg.Content+",", ",snippets,") {
		content := "snippets"
		if cfg.Content != "" {
			content = cfg.Content + ",snippets"
		}
		if !reStorageContent.MatchString(content) {
			return "", fmt.Errorf("unexpected content types on storage %s", snippetStorage)
		}
		if out, err := setStorageContent(ctx, snippetStorage, content); err != nil {
			return "", fmt.Errorf("enable snippets on storage %s: %s", snippetStorage, strings.TrimSpace(string(out)))
		}
	}
	dir := filepath.Join(cfg.Path, "snippets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	file := filepath.Join(dir, vendorSnippetName)
	if cur, err := os.ReadFile(file); err != nil || !bytes.Equal(cur, []byte(vendorSnippet)) {
		if err := writeFileAtomic(file, []byte(vendorSnippet), 0o644); err != nil {
			return "", err
		}
	}
	return snippetStorage + ":snippets/" + vendorSnippetName, nil
}
