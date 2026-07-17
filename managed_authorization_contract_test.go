package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/authorization"
)

func TestManagedAuthorizationVersionSurfacesStayInSync(t *testing.T) {
	docs, err := os.ReadFile("docs/src/content/docs/reference/managed-authorization.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Protocol version " + authorization.ProtocolVersion,
		"authorization-protocol=" + authorization.ProtocolVersion,
		`"protocolVersion": "` + authorization.ProtocolVersion + `"`,
	} {
		if !strings.Contains(string(docs), want) {
			t.Fatalf("managed authorization docs missing canonical version surface %q", want)
		}
	}

	manifest, err := os.ReadFile(".release-please-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var versions map[string]string
	if err := json.Unmarshal(manifest, &versions); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+(?:[-+].+)?$`).MatchString(versions["."]) {
		t.Fatalf("release manifest version is not semver: %q", versions["."])
	}

	workflow, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ref: ${{ needs.release-please.outputs.tag_name }}",
		"buildinfo.Version=${TAG}",
		"sha256sum no-mistakes-* > ../checksums.txt",
	} {
		if !strings.Contains(string(workflow), want) {
			t.Fatalf("release workflow missing version/artifact binding %q", want)
		}
	}
}
