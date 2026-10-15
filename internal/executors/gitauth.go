package executors

import (
	"fmt"
	"os"
	"regexp"
)

// gitProtocolArgs allows only https and ssh: other transports (ext::, file://) can run
// commands or read local files.
func gitProtocolArgs() []string {
	return []string{
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"-c", "protocol.ssh.allow=always",
	}
}

// Agent-generated paths only, since they end up in a shell command line.
var reSafePath = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// sshCommandArg enforces a pinned host key; without one, the first key seen is recorded
// and enforced afterwards.
func sshCommandArg(keyPath, knownHostsPath string, pinned bool) ([]string, error) {
	if !reSafePath.MatchString(keyPath) || !reSafePath.MatchString(knownHostsPath) {
		return nil, fmt.Errorf("unsafe ssh path")
	}
	strict := "accept-new"
	if pinned {
		strict = "yes"
	}
	return []string{"-c", fmt.Sprintf(
		"core.sshCommand=ssh -F /dev/null -i %s -o IdentitiesOnly=yes -o BatchMode=yes -o UserKnownHostsFile=%s -o StrictHostKeyChecking=%s",
		keyPath, knownHostsPath, strict)}, nil
}

// hostGitAuth passes an HTTPS token via the credential helper, or an SSH key in a 0600 temp
// file that cleanup removes.
func hostGitAuth(repo interface {
	GetInstallationToken() string
	GetSshPrivateKey() string
	GetSshKnownHosts() string
}, dest string) (args []string, env []string, cleanup func(), err error) {
	args = gitProtocolArgs()
	cleanup = func() {}
	if token := repo.GetInstallationToken(); token != "" {
		args = append(args,
			"-c", "credential.helper=",
			"-c", `credential.helper=!f() { echo username=x-access-token; echo "password=$OWNSHIP_GIT_TOKEN"; }; f`)
		env = append(env, "OWNSHIP_GIT_TOKEN="+token)
	}
	key := repo.GetSshPrivateKey()
	if key == "" {
		return args, env, cleanup, nil
	}
	keyFile, err := os.CreateTemp("", "ownship-deploy-key-")
	if err != nil {
		return nil, nil, nil, err
	}
	removeKey := func() { _ = os.Remove(keyFile.Name()) }
	if _, err := keyFile.WriteString(key + "\n"); err != nil {
		keyFile.Close()
		removeKey()
		return nil, nil, nil, err
	}
	keyFile.Close()

	// Next to the workspace, so the recorded host key survives redeploys.
	knownHosts := dest + ".known_hosts"
	pinned := repo.GetSshKnownHosts() != ""
	if pinned {
		if err := os.WriteFile(knownHosts, []byte(repo.GetSshKnownHosts()+"\n"), 0o600); err != nil {
			removeKey()
			return nil, nil, nil, err
		}
	}
	sshArgs, err := sshCommandArg(keyFile.Name(), knownHosts, pinned)
	if err != nil {
		removeKey()
		return nil, nil, nil, err
	}
	return append(args, sshArgs...), env, removeKey, nil
}
