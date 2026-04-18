package telemetry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
)

func TestDefaultUsesDotEnvInDevBuildWhenEnvMissing(t *testing.T) {
	prevSink := defaultSink
	defaultSink = nil
	defer func() { defaultSink = prevSink }()

	prevWebsiteID := buildinfo.TelemetryWebsiteID
	defer func() {
		buildinfo.TelemetryWebsiteID = prevWebsiteID
	}()
	buildinfo.TelemetryWebsiteID = ""

	t.Setenv(umamiWebsiteIDEnv, "")

	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "NO_MISTAKES_UMAMI_WEBSITE_ID=website-from-dotenv\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd(): %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(): %v", err)
	}
	defer os.Chdir(prevWD)

	sink := Default()
	client, ok := sink.(*Client)
	if !ok {
		t.Fatalf("Default() type = %T, want *Client", sink)
	}
	if client.endpoint != umamiCloudURL+"/api/send" {
		t.Fatalf("endpoint = %q, want %q", client.endpoint, umamiCloudURL+"/api/send")
	}
	if client.websiteID != "website-from-dotenv" {
		t.Fatalf("websiteID = %q, want %q", client.websiteID, "website-from-dotenv")
	}
}

func TestDefaultPrefersEnvVarWebsiteIDAndIgnoresHostOverride(t *testing.T) {
	prevSink := defaultSink
	defaultSink = nil
	defer func() { defaultSink = prevSink }()

	prevWebsiteID := buildinfo.TelemetryWebsiteID
	defer func() {
		buildinfo.TelemetryWebsiteID = prevWebsiteID
	}()
	buildinfo.TelemetryWebsiteID = "embedded-website"

	t.Setenv(umamiHostEnv, "https://env.example")
	t.Setenv(umamiWebsiteIDEnv, "website-from-env")

	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "NO_MISTAKES_UMAMI_HOST=https://dotenv.example\nNO_MISTAKES_UMAMI_WEBSITE_ID=website-from-dotenv\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd(): %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(): %v", err)
	}
	defer os.Chdir(prevWD)

	sink := Default()
	client, ok := sink.(*Client)
	if !ok {
		t.Fatalf("Default() type = %T, want *Client", sink)
	}
	if client.endpoint != umamiCloudURL+"/api/send" {
		t.Fatalf("endpoint = %q, want %q", client.endpoint, umamiCloudURL+"/api/send")
	}
	if client.websiteID != "website-from-env" {
		t.Fatalf("websiteID = %q, want %q", client.websiteID, "website-from-env")
	}
}

func TestDefaultIgnoresDotEnvOutsideRepo(t *testing.T) {
	prevSink := defaultSink
	defaultSink = nil
	defer func() { defaultSink = prevSink }()

	prevWebsiteID := buildinfo.TelemetryWebsiteID
	defer func() {
		buildinfo.TelemetryWebsiteID = prevWebsiteID
	}()
	buildinfo.TelemetryWebsiteID = ""

	t.Setenv(umamiWebsiteIDEnv, "")

	parentDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(parentDir, ".env"), []byte("NO_MISTAKES_UMAMI_WEBSITE_ID=outside-repo\n"), 0o644); err != nil {
		t.Fatalf("write parent .env: %v", err)
	}

	repoDir := filepath.Join(parentDir, "repo")
	if err := os.Mkdir(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	subDir := filepath.Join(repoDir, "nested")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.Mkdir(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd(): %v", err)
	}
	if err := os.Chdir(subDir); err != nil {
		t.Fatalf("Chdir(): %v", err)
	}
	defer os.Chdir(prevWD)

	if _, ok := Default().(*Client); ok {
		t.Fatal("Default() should ignore dotenv outside repo")
	}
}
