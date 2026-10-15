// Package registry reads the digest a tag points at via the Registry v2 API, without pulling.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const acceptManifests = "application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json"

var client = &http.Client{Timeout: 30 * time.Second}

type Ref struct {
	Host string // registry host, e.g. registry-1.docker.io
	Repo string // repository path, e.g. library/nginx
	Tag  string
}

// ParseRef applies Docker's defaults. Digest pins are rejected, since they can't move.
func ParseRef(image string) (Ref, error) {
	if strings.Contains(image, "@") {
		return Ref{}, fmt.Errorf("digest-pinned image cannot be update-checked")
	}
	host := "registry-1.docker.io"
	rest := image
	// The first segment is a host if it has a dot or colon, or is localhost.
	if first, after, ok := strings.Cut(image, "/"); ok {
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			host, rest = first, after
		}
	}
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	repo, tag := rest, "latest"
	// The tag colon is after the last slash; earlier colons are the host port.
	if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		repo, tag = rest[:i], rest[i+1:]
	}
	if repo == "" || tag == "" {
		return Ref{}, fmt.Errorf("invalid image reference %q", image)
	}
	if host == "registry-1.docker.io" && !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	return Ref{Host: host, Repo: repo, Tag: tag}, nil
}

// LatestDigest returns the tag's current digest, anonymously when username is empty.
func LatestDigest(ctx context.Context, image, username, password string) (string, error) {
	ref, err := ParseRef(image)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", ref.Host, ref.Repo, ref.Tag)

	digest, challenge, err := manifestDigest(ctx, url, "", username, password)
	if err != nil {
		return "", err
	}
	if digest != "" {
		return digest, nil
	}
	// On a Bearer challenge, fetch a token and retry once.
	token, err := fetchToken(ctx, challenge, username, password)
	if err != nil {
		return "", err
	}
	digest, _, err = manifestDigest(ctx, url, token, "", "")
	if err != nil {
		return "", err
	}
	if digest == "" {
		return "", fmt.Errorf("registry did not return a digest for %s", image)
	}
	return digest, nil
}

// manifestDigest HEADs the manifest, returning the digest or the 401 challenge.
func manifestDigest(ctx context.Context, url, bearer, username, password string) (digest, challenge string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", acceptManifests)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	} else if username != "" {
		req.SetBasicAuth(username, password)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return "", resp.Header.Get("Www-Authenticate"), nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return "", "", fmt.Errorf("registry manifest request: %s", resp.Status)
	}
	d := resp.Header.Get("Docker-Content-Digest")
	if d == "" {
		return "", "", fmt.Errorf("registry did not send Docker-Content-Digest")
	}
	return d, "", nil
}

// fetchToken answers a `Bearer realm=...,service=...,scope=...` challenge.
func fetchToken(ctx context.Context, challenge, username, password string) (string, error) {
	params := parseChallenge(challenge)
	realm := params["realm"]
	if realm == "" || !strings.HasPrefix(realm, "https://") {
		return "", fmt.Errorf("unsupported registry auth challenge")
	}
	q := make([]string, 0, 2)
	if s := params["service"]; s != "" {
		q = append(q, "service="+s)
	}
	if s := params["scope"]; s != "" {
		q = append(q, "scope="+s)
	}
	url := realm
	if len(q) > 0 {
		url += "?" + strings.Join(q, "&")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if username != "" {
		req.SetBasicAuth(username, password)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry token request: %s", resp.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.Token != "" {
		return body.Token, nil
	}
	return body.AccessToken, nil
}

func parseChallenge(header string) map[string]string {
	params := map[string]string{}
	header = strings.TrimPrefix(strings.TrimSpace(header), "Bearer ")
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		params[strings.ToLower(k)] = strings.Trim(v, `"`)
	}
	return params
}
