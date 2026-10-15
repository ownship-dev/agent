// Package cloudflare adds a deployment's ingress rule and DNS CNAME to the user's tunnel.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const apiBase = "https://api.cloudflare.com/client/v4"

type Target struct {
	Hostname  string
	TunnelID  string
	APIToken  string
	AccountID string
	ZoneID    string
}

type Router struct {
	http  *http.Client
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewRouter() *Router {
	return &Router{http: &http.Client{Timeout: 30 * time.Second}, locks: map[string]*sync.Mutex{}}
}

// Route points hostname at service. The ingress list is replaced whole, so writes are
// serialized per tunnel.
func (r *Router) Route(ctx context.Context, t Target, service string) error {
	lock := r.lockFor(t.TunnelID)
	lock.Lock()
	defer lock.Unlock()

	if err := r.upsertIngress(ctx, t, service); err != nil {
		return fmt.Errorf("tunnel ingress: %w", err)
	}
	if err := r.upsertDNS(ctx, t); err != nil {
		return fmt.Errorf("dns record: %w", err)
	}
	return nil
}

// RouteAll routes every hostname with one tunnel config write and concurrent DNS upserts.
// Targets must share a tunnel, account and zone.
func (r *Router) RouteAll(ctx context.Context, targets []Target, service string) error {
	if len(targets) == 0 {
		return nil
	}
	tun := targets[0]
	lock := r.lockFor(tun.TunnelID)
	lock.Lock()
	defer lock.Unlock()

	if err := r.upsertIngressAll(ctx, tun, targets, service); err != nil {
		return fmt.Errorf("tunnel ingress: %w", err)
	}
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t Target) {
			defer wg.Done()
			if err := r.upsertDNS(ctx, t); err != nil {
				errs[i] = fmt.Errorf("dns record %s: %w", t.Hostname, err)
			}
		}(i, t)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Router) upsertIngressAll(ctx context.Context, tun Target, targets []Target, service string) error {
	url := fmt.Sprintf("%s/accounts/%s/cfd_tunnel/%s/configurations", apiBase, tun.AccountID, tun.TunnelID)

	var current struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := r.do(ctx, http.MethodGet, url, tun.APIToken, nil, &current); err != nil {
		return err
	}
	if current.Config == nil {
		current.Config = map[string]json.RawMessage{}
	}
	var ingress []ingressRule
	if raw, ok := current.Config["ingress"]; ok {
		if err := json.Unmarshal(raw, &ingress); err != nil {
			return err
		}
	}
	for _, t := range targets {
		ingress = withRule(ingress, ingressRule{Hostname: t.Hostname, Service: service})
	}

	updated, err := json.Marshal(ingress)
	if err != nil {
		return err
	}
	current.Config["ingress"] = updated
	return r.do(ctx, http.MethodPut, url, tun.APIToken, map[string]any{"config": current.Config}, nil)
}

// Unroute removes a hostname's ingress rule and DNS record.
func (r *Router) Unroute(ctx context.Context, t Target) error {
	lock := r.lockFor(t.TunnelID)
	lock.Lock()
	defer lock.Unlock()

	if err := r.removeIngress(ctx, t); err != nil {
		return fmt.Errorf("tunnel ingress: %w", err)
	}
	if err := r.deleteDNS(ctx, t); err != nil {
		return fmt.Errorf("dns record: %w", err)
	}
	return nil
}

func (r *Router) lockFor(tunnelID string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.locks[tunnelID]
	if !ok {
		m = &sync.Mutex{}
		r.locks[tunnelID] = m
	}
	return m
}

type ingressRule struct {
	Hostname      string          `json:"hostname,omitempty"`
	Path          string          `json:"path,omitempty"`
	Service       string          `json:"service"`
	OriginRequest json.RawMessage `json:"originRequest,omitempty"`
}

func (r *Router) upsertIngress(ctx context.Context, t Target, service string) error {
	url := fmt.Sprintf("%s/accounts/%s/cfd_tunnel/%s/configurations", apiBase, t.AccountID, t.TunnelID)

	var current struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := r.do(ctx, http.MethodGet, url, t.APIToken, nil, &current); err != nil {
		return err
	}
	if current.Config == nil {
		current.Config = map[string]json.RawMessage{}
	}

	var ingress []ingressRule
	if raw, ok := current.Config["ingress"]; ok {
		if err := json.Unmarshal(raw, &ingress); err != nil {
			return err
		}
	}
	ingress = withRule(ingress, ingressRule{Hostname: t.Hostname, Service: service})

	updated, err := json.Marshal(ingress)
	if err != nil {
		return err
	}
	current.Config["ingress"] = updated

	body := map[string]any{"config": current.Config}
	return r.do(ctx, http.MethodPut, url, t.APIToken, body, nil)
}

// withRule replaces this hostname's rule and keeps the catch-all 404 last.
func withRule(rules []ingressRule, rule ingressRule) []ingressRule {
	out := make([]ingressRule, 0, len(rules)+1)
	for _, existing := range rules {
		if existing.Hostname == rule.Hostname || existing.Hostname == "" {
			continue // drop our old rule and any catch-all
		}
		out = append(out, existing)
	}
	out = append(out, rule)
	out = append(out, ingressRule{Service: "http_status:404"})
	return out
}

func (r *Router) removeIngress(ctx context.Context, t Target) error {
	url := fmt.Sprintf("%s/accounts/%s/cfd_tunnel/%s/configurations", apiBase, t.AccountID, t.TunnelID)

	var current struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := r.do(ctx, http.MethodGet, url, t.APIToken, nil, &current); err != nil {
		return err
	}
	if current.Config == nil {
		return nil
	}

	var ingress []ingressRule
	if raw, ok := current.Config["ingress"]; ok {
		if err := json.Unmarshal(raw, &ingress); err != nil {
			return err
		}
	}
	out := make([]ingressRule, 0, len(ingress))
	for _, existing := range ingress {
		if existing.Hostname == t.Hostname || existing.Hostname == "" {
			continue
		}
		out = append(out, existing)
	}
	out = append(out, ingressRule{Service: "http_status:404"})

	updated, err := json.Marshal(out)
	if err != nil {
		return err
	}
	current.Config["ingress"] = updated
	return r.do(ctx, http.MethodPut, url, t.APIToken, map[string]any{"config": current.Config}, nil)
}

func (r *Router) deleteDNS(ctx context.Context, t Target) error {
	listURL := fmt.Sprintf("%s/zones/%s/dns_records?type=CNAME&name=%s", apiBase, t.ZoneID, url.QueryEscape(t.Hostname))
	var existing []struct {
		ID string `json:"id"`
	}
	if err := r.do(ctx, http.MethodGet, listURL, t.APIToken, nil, &existing); err != nil {
		return err
	}
	if len(existing) == 0 {
		return nil
	}
	url := fmt.Sprintf("%s/zones/%s/dns_records/%s", apiBase, t.ZoneID, existing[0].ID)
	return r.do(ctx, http.MethodDelete, url, t.APIToken, nil, nil)
}

func (r *Router) upsertDNS(ctx context.Context, t Target) error {
	content := t.TunnelID + ".cfargotunnel.com"
	listURL := fmt.Sprintf("%s/zones/%s/dns_records?type=CNAME&name=%s", apiBase, t.ZoneID, url.QueryEscape(t.Hostname))

	var existing []struct {
		ID string `json:"id"`
	}
	if err := r.do(ctx, http.MethodGet, listURL, t.APIToken, nil, &existing); err != nil {
		return err
	}

	record := map[string]any{
		"type":    "CNAME",
		"name":    t.Hostname,
		"content": content,
		"proxied": true,
	}
	if len(existing) > 0 {
		url := fmt.Sprintf("%s/zones/%s/dns_records/%s", apiBase, t.ZoneID, existing[0].ID)
		return r.do(ctx, http.MethodPut, url, t.APIToken, record, nil)
	}
	url := fmt.Sprintf("%s/zones/%s/dns_records", apiBase, t.ZoneID)
	return r.do(ctx, http.MethodPost, url, t.APIToken, record, nil)
}

func (r *Router) do(ctx context.Context, method, url, token string, body any, out any) error {
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var envelope struct {
		Success bool              `json:"success"`
		Errors  []json.RawMessage `json:"errors"`
		Result  json.RawMessage   `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode response (%d): %w", resp.StatusCode, err)
	}
	if !envelope.Success {
		return fmt.Errorf("cloudflare api error (%d): %v", resp.StatusCode, envelope.Errors)
	}
	if out != nil {
		return json.Unmarshal(mustResult(envelope.Result), out)
	}
	return nil
}

func mustResult(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	return raw
}
