package executors

import (
	"regexp"
	"strings"
	"testing"
)

func TestParsePveamAvailable(t *testing.T) {
	out := `mladdb
	updating pve container template list...
system          debian-12-standard_12.7-1_amd64.tar.zst
system          debian-13-standard_13.0-1_amd64.tar.zst

turnkeylinux    turnkey-wordpress_18.0-1_amd64.tar.gz
system          ubuntu-22.04-standard_22.04-1_amd64.tar.zst
   malformed line with too many fields here
system
turnkeylinux    turnkey-drupal7_16.1-1_amd64.tar.gz
`
	got := parsePveamAvailable(out)
	want := []string{
		"debian-12-standard_12.7-1_amd64.tar.zst",
		"debian-13-standard_13.0-1_amd64.tar.zst",
		"ubuntu-22.04-standard_22.04-1_amd64.tar.zst",
	}
	if len(got) != len(want) {
		t.Fatalf("parsePveamAvailable() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parsePveamAvailable()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if names := parsePveamAvailable(""); len(names) != 0 {
		t.Errorf("parsePveamAvailable(\"\") = %v, want empty", names)
	}
	if names := parsePveamAvailable("turnkeylinux    only-non-system_1.0-1_amd64.tar.gz\n"); len(names) != 0 {
		t.Errorf("parsePveamAvailable(non-system only) = %v, want empty", names)
	}
}

func TestNewestMatching(t *testing.T) {
	names := []string{
		"debian-12-standard_12.2-1_amd64.tar.zst",
		"debian-12-standard_12.7-1_amd64.tar.zst",
		"debian-12-standard_12.10-1_amd64.tar.zst",
	}
	// The load-bearing case: 12.10-1 beats 12.7-1 numerically, even though
	// "12.10-1" < "12.7-1" lexicographically.
	got := newestMatching(names, "debian-12-standard")
	want := "debian-12-standard_12.10-1_amd64.tar.zst"
	if got != want {
		t.Errorf("newestMatching(debian) = %q, want %q", got, want)
	}

	// Prefix boundary: "ubuntu-22.04-standard" must not match
	// "ubuntu-24.04-standard_..." names.
	boundary := []string{
		"ubuntu-22.04-standard_22.04-1_amd64.tar.zst",
		"ubuntu-24.04-standard_24.04-3_amd64.tar.zst",
	}
	got = newestMatching(boundary, "ubuntu-22.04-standard")
	want = "ubuntu-22.04-standard_22.04-1_amd64.tar.zst"
	if got != want {
		t.Errorf("newestMatching(ubuntu-22.04 boundary) = %q, want %q", got, want)
	}

	if got := newestMatching(names, "centos-9-stream-default"); got != "" {
		t.Errorf("newestMatching(no match) = %q, want \"\"", got)
	}
	if got := newestMatching(nil, "debian-12-standard"); got != "" {
		t.Errorf("newestMatching(nil) = %q, want \"\"", got)
	}

	// Case must not defeat the match — some mirrors ship capitalized names.
	mixed := []string{"Debian-12-Standard_12.7-1_amd64.tar.zst"}
	if got := newestMatching(mixed, "debian-12-standard"); got != mixed[0] {
		t.Errorf("newestMatching(case-insensitive) = %q, want %q", got, mixed[0])
	}
}

func TestTemplateFilenames(t *testing.T) {
	got := templateFilenames([]string{
		"local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst",
		"cephfs:vztmpl/ubuntu-22.04-standard_22.04-1_amd64.tar.zst",
		"weird-volid-without-marker",
	})
	want := []string{
		"debian-12-standard_12.7-1_amd64.tar.zst",
		"ubuntu-22.04-standard_22.04-1_amd64.tar.zst",
		"weird-volid-without-marker",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("templateFilenames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMatchVolid(t *testing.T) {
	volids := []string{
		"local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst",
		"local:vztmpl/debian-12-standard_12.10-1_amd64.tar.zst",
		"nfs-templates:vztmpl/ubuntu-22.04-standard_22.04-1_amd64.tar.zst",
	}
	got := matchVolid(volids, "debian-12-standard")
	want := "local:vztmpl/debian-12-standard_12.10-1_amd64.tar.zst"
	if got != want {
		t.Errorf("matchVolid(debian) = %q, want %q", got, want)
	}

	// Works across storages.
	got = matchVolid(volids, "ubuntu-22.04-standard")
	want = "nfs-templates:vztmpl/ubuntu-22.04-standard_22.04-1_amd64.tar.zst"
	if got != want {
		t.Errorf("matchVolid(ubuntu across storage) = %q, want %q", got, want)
	}

	if got := matchVolid(volids, "centos-9-stream-default"); got != "" {
		t.Errorf("matchVolid(no match) = %q, want \"\"", got)
	}
	if got := matchVolid(nil, "debian-12-standard"); got != "" {
		t.Errorf("matchVolid(nil) = %q, want \"\"", got)
	}
}

func TestFamilyForTemplate(t *testing.T) {
	cases := map[string]baseOSFamily{
		"debian-12-standard_12.7-1_amd64.tar.zst":              familyDebian,
		"local:vztmpl/debian-13-standard_13.0-1_amd64.tar.zst": familyDebian,
		"ubuntu-24.04-standard_24.04-3_amd64.tar.zst":          familyUbuntu,
		"centos-9-stream-default_20240828_amd64.tar.xz":        familyRHEL,
		"rockylinux-9-default_20231017_amd64.tar.xz":           familyRHEL,
		"almalinux-9-default_20240523_amd64.tar.xz":            familyRHEL,
		"fedora-40-default_20240523_amd64.tar.xz":              familyRHEL,
		"alpine-3.19-default_20240207_amd64.tar.xz":            "",
		"archlinux-base_20240911-1_amd64.tar.zst":              "",
	}
	for input, want := range cases {
		if got := familyForTemplate(input); got != want {
			t.Errorf("familyForTemplate(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRankFallback(t *testing.T) {
	t.Run("debian preferred over newer ubuntu", func(t *testing.T) {
		volids := []string{
			"local:vztmpl/ubuntu-24.04-standard_24.04-3_amd64.tar.zst",
			"local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst",
		}
		got, err := rankFallback(volids)
		if err != nil {
			t.Fatalf("rankFallback() error = %v", err)
		}
		want := "local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst"
		if got != want {
			t.Errorf("rankFallback() = %q, want %q", got, want)
		}
	})

	t.Run("ubuntu when no debian", func(t *testing.T) {
		volids := []string{
			"local:vztmpl/centos-9-stream-default_20240828_amd64.tar.xz",
			"local:vztmpl/ubuntu-22.04-standard_22.04-1_amd64.tar.zst",
		}
		got, err := rankFallback(volids)
		if err != nil {
			t.Fatalf("rankFallback() error = %v", err)
		}
		want := "local:vztmpl/ubuntu-22.04-standard_22.04-1_amd64.tar.zst"
		if got != want {
			t.Errorf("rankFallback() = %q, want %q", got, want)
		}
	})

	t.Run("rhel last resort", func(t *testing.T) {
		volids := []string{
			"local:vztmpl/centos-9-stream-default_20240828_amd64.tar.xz",
		}
		got, err := rankFallback(volids)
		if err != nil {
			t.Fatalf("rankFallback() error = %v", err)
		}
		want := "local:vztmpl/centos-9-stream-default_20240828_amd64.tar.xz"
		if got != want {
			t.Errorf("rankFallback() = %q, want %q", got, want)
		}
	})

	t.Run("unsupported only errors naming what was found", func(t *testing.T) {
		volids := []string{
			"local:vztmpl/alpine-3.19-default_20240207_amd64.tar.xz",
			"local:vztmpl/archlinux-base_20240911-1_amd64.tar.zst",
		}
		_, err := rankFallback(volids)
		if err == nil {
			t.Fatal("rankFallback() error = nil, want error")
		}
		for _, want := range []string{"alpine-3.19-default_20240207_amd64.tar.xz", "archlinux-base_20240911-1_amd64.tar.zst"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("rankFallback() error %q does not mention %q", err.Error(), want)
			}
		}
	})

	t.Run("empty errors", func(t *testing.T) {
		_, err := rankFallback(nil)
		if err == nil {
			t.Fatal("rankFallback(nil) error = nil, want error")
		}
	})
}

func TestBaseOSCatalogueSanity(t *testing.T) {
	wantIDs := map[string]struct {
		family baseOSFamily
		prefix string
	}{
		"debian-12":        {familyDebian, "debian-12-standard"},
		"debian-13":        {familyDebian, "debian-13-standard"},
		"ubuntu-22.04":     {familyUbuntu, "ubuntu-22.04-standard"},
		"ubuntu-24.04":     {familyUbuntu, "ubuntu-24.04-standard"},
		"centos-9-stream":  {familyRHEL, "centos-9-stream-default"},
		"centos-10-stream": {familyRHEL, "centos-10-stream-default"},
	}

	if len(baseOSCatalogue) != len(wantIDs) {
		t.Fatalf("baseOSCatalogue has %d entries, want %d", len(baseOSCatalogue), len(wantIDs))
	}

	idPattern := regexp.MustCompile(`^[a-z0-9.-]+$`)
	validFamilies := map[baseOSFamily]bool{familyDebian: true, familyUbuntu: true, familyRHEL: true}

	for id, want := range wantIDs {
		spec, ok := baseOSCatalogue[id]
		if !ok {
			t.Errorf("baseOSCatalogue missing id %q", id)
			continue
		}
		if spec.ID != id {
			t.Errorf("baseOSCatalogue[%q].ID = %q, want %q", id, spec.ID, id)
		}
		if spec.Family != want.family {
			t.Errorf("baseOSCatalogue[%q].Family = %q, want %q", id, spec.Family, want.family)
		}
		if spec.Prefix != want.prefix {
			t.Errorf("baseOSCatalogue[%q].Prefix = %q, want %q", id, spec.Prefix, want.prefix)
		}
		if spec.Prefix == "" {
			t.Errorf("baseOSCatalogue[%q].Prefix is empty", id)
		}
		if !idPattern.MatchString(id) {
			t.Errorf("baseOSCatalogue id %q does not match %s", id, idPattern.String())
		}
		if !validFamilies[spec.Family] {
			t.Errorf("baseOSCatalogue[%q].Family = %q is not a valid family", id, spec.Family)
		}
	}
}
