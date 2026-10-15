package executors

import (
	"net"
	"testing"
)

func TestFirstHostPort(t *testing.T) {
	cases := map[string]string{
		"":             "",
		"0 ":           "",
		"49153 ":       "49153",
		" 0 49200 ":    "49200",
		"garbage 8080": "8080",
		"70000 ":       "",
	}
	for in, want := range cases {
		if got := firstHostPort(in); got != want {
			t.Errorf("firstHostPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPortFreeAndFreePort(t *testing.T) {
	p, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	if !portFree(p) {
		t.Fatalf("freshly allocated port %s reported busy", p)
	}
	l, err := net.Listen("tcp", "127.0.0.1:"+p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if portFree(p) {
		t.Fatalf("port %s reported free while bound", p)
	}
}
