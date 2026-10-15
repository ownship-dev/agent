package executors

import "strings"

// nonRootUser is forced when the image declares no non-root user.
const nonRootUser = "1000:1000"

// dockerUserArgs returns nil when the image already runs non-root.
func dockerUserArgs(imageUser string) []string {
	if isNonRootUser(imageUser) {
		return nil
	}
	return []string{"-u", nonRootUser}
}

// isNonRootUser treats empty, "root" and "0" as root.
func isNonRootUser(imageUser string) bool {
	imageUser = strings.TrimSpace(imageUser)
	if imageUser == "" {
		return false
	}
	name, _, _ := strings.Cut(imageUser, ":")
	name = strings.ToLower(name)
	return name != "root" && name != "0"
}
