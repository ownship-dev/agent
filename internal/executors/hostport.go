package executors

import (
	"context"
	"net"
	"strconv"
	"strings"
)

// previousHostPort returns the port the first existing container publishes, so a redeploy
// keeps its routes.
func previousHostPort(ctx context.Context, eng *Engine, names ...string) string {
	for _, name := range names {
		out, err := eng.Cmd(ctx, "inspect", "--format",
			"{{range $p, $b := .HostConfig.PortBindings}}{{range $b}}{{.HostPort}} {{end}}{{end}}", name).Output()
		if err != nil {
			continue
		}
		if port := firstHostPort(string(out)); port != "" {
			return port
		}
	}
	return ""
}

// firstHostPort skips "0", which means unpinned.
func firstHostPort(out string) string {
	for _, f := range strings.Fields(out) {
		if n, err := strconv.Atoi(f); err == nil && n > 0 && n < 65536 {
			return f
		}
	}
	return ""
}

func portFree(port string) bool {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// freePort is racy in theory, but docker run fails loudly rather than share a port.
func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port), nil
}
