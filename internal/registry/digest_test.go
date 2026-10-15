package registry

import "testing"

func TestParseRef(t *testing.T) {
	cases := []struct {
		in              string
		host, repo, tag string
		wantErr         bool
	}{
		{in: "nginx", host: "registry-1.docker.io", repo: "library/nginx", tag: "latest"},
		{in: "nginx:1.27", host: "registry-1.docker.io", repo: "library/nginx", tag: "1.27"},
		{in: "louislam/uptime-kuma:1", host: "registry-1.docker.io", repo: "louislam/uptime-kuma", tag: "1"},
		{in: "ghcr.io/user/app:main", host: "ghcr.io", repo: "user/app", tag: "main"},
		{in: "docker.io/library/redis:7", host: "registry-1.docker.io", repo: "library/redis", tag: "7"},
		{in: "localhost:5000/team/app", host: "localhost:5000", repo: "team/app", tag: "latest"},
		{in: "registry.lan:443/a/b:v2", host: "registry.lan:443", repo: "a/b", tag: "v2"},
		{in: "nginx@sha256:abc", wantErr: true},
	}
	for _, c := range cases {
		ref, err := ParseRef(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if ref.Host != c.host || ref.Repo != c.repo || ref.Tag != c.tag {
			t.Errorf("%q: got %+v", c.in, ref)
		}
	}
}

func TestParseChallenge(t *testing.T) {
	p := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"`)
	if p["realm"] != "https://auth.docker.io/token" || p["service"] != "registry.docker.io" {
		t.Fatalf("got %+v", p)
	}
}
