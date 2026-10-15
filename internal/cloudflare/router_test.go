package cloudflare

import "testing"

func catchAllLast(t *testing.T, rules []ingressRule) {
	t.Helper()
	if len(rules) == 0 || rules[len(rules)-1].Service != "http_status:404" {
		t.Fatalf("expected catch-all 404 as the last rule, got %+v", rules)
	}
	for i, r := range rules[:len(rules)-1] {
		if r.Hostname == "" {
			t.Fatalf("non-terminal rule %d has no hostname (a stray catch-all): %+v", i, rules)
		}
	}
}

func TestWithRuleEmpty(t *testing.T) {
	out := withRule(nil, ingressRule{Hostname: "a.example.com", Service: "http://10.0.0.1:3000"})
	if len(out) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(out))
	}
	catchAllLast(t, out)
}

func TestWithRuleReplacesSameHostAndKeepsOthers(t *testing.T) {
	existing := []ingressRule{
		{Hostname: "a.example.com", Service: "http://old:1"},
		{Hostname: "b.example.com", Service: "http://keep:2"},
		{Service: "http_status:404"},
	}
	out := withRule(existing, ingressRule{Hostname: "a.example.com", Service: "http://new:3"})

	catchAllLast(t, out)
	var a, b string
	for _, r := range out {
		switch r.Hostname {
		case "a.example.com":
			a = r.Service
		case "b.example.com":
			b = r.Service
		}
	}
	if a != "http://new:3" {
		t.Fatalf("a.example.com not updated: %q", a)
	}
	if b != "http://keep:2" {
		t.Fatalf("b.example.com should be preserved: %q", b)
	}
	// Exactly one catch-all.
	count := 0
	for _, r := range out {
		if r.Service == "http_status:404" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one catch-all, got %d", count)
	}
}
