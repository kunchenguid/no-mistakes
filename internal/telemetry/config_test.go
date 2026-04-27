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

	prevHost := buildinfo.TelemetryHost
	prevWebsiteID := buildinfo.TelemetryWebsiteID
	defer func() {
		buildinfo.TelemetryHost = prevHost
		buildinfo.TelemetryWebsiteID = prevWebsiteID
	}()
	buildinfo.TelemetryHost = ""
	buildinfo.TelemetryWebsiteID = ""

	t.Setenv(umamiHostEnv, "")
	t.Setenv(umamiWebsiteIDEnv, "")

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
	if client.endpoint != "https://dotenv.example/api/send" {
		t.Fatalf("endpoint = %q, want %q", client.endpoint, "https://dotenv.example/api/send")
	}
	if client.websiteID != "website-from-dotenv" {
		t.Fatalf("websiteID = %q, want %q", client.websiteID, "website-from-dotenv")
	}
}

func TestDefaultPrefersEnvVarsOverDotEnvAndEmbeddedConfig(t *testing.T) {
	prevSink := defaultSink
	defaultSink = nil
	defer func() { defaultSink = prevSink }()

	prevHost := buildinfo.TelemetryHost
	prevVersion := buildinfo.Version
	prevWebsiteID := buildinfo.TelemetryWebsiteID
	defer func() {
		buildinfo.TelemetryHost = prevHost
		buildinfo.Version = prevVersion
		buildinfo.TelemetryWebsiteID = prevWebsiteID
	}()
	buildinfo.TelemetryHost = "https://embedded.example"
	buildinfo.Version = "v1.2.3"
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
	if client.endpoint != "https://env.example/api/send" {
		t.Fatalf("endpoint = %q, want %q", client.endpoint, "https://env.example/api/send")
	}
	if client.websiteID != "website-from-env" {
		t.Fatalf("websiteID = %q, want %q", client.websiteID, "website-from-env")
	}
}

func TestDefaultUsesEmbeddedTelemetryHostAndWebsiteID(t *testing.T) {
	prevSink := defaultSink
	defaultSink = nil
	defer func() { defaultSink = prevSink }()

	prevHost := buildinfo.TelemetryHost
	prevVersion := buildinfo.Version
	prevWebsiteID := buildinfo.TelemetryWebsiteID
	defer func() {
		buildinfo.TelemetryHost = prevHost
		buildinfo.Version = prevVersion
		buildinfo.TelemetryWebsiteID = prevWebsiteID
	}()
	buildinfo.TelemetryHost = "https://embedded.example"
	buildinfo.Version = "v1.2.3"
	buildinfo.TelemetryWebsiteID = "embedded-website"

	t.Setenv(umamiHostEnv, "")
	t.Setenv(umamiWebsiteIDEnv, "")

	sink := Default()
	client, ok := sink.(*Client)
	if !ok {
		t.Fatalf("Default() type = %T, want *Client", sink)
	}
	if client.endpoint != "https://embedded.example/api/send" {
		t.Fatalf("endpoint = %q, want %q", client.endpoint, "https://embedded.example/api/send")
	}
	if client.websiteID != "embedded-website" {
		t.Fatalf("websiteID = %q, want %q", client.websiteID, "embedded-website")
	}
}

func TestDefaultUsesSelfHostedHostWhenHostConfigMissing(t *testing.T) {
	prevSink := defaultSink
	defaultSink = nil
	defer func() { defaultSink = prevSink }()

	prevHost := buildinfo.TelemetryHost
	prevVersion := buildinfo.Version
	prevWebsiteID := buildinfo.TelemetryWebsiteID
	defer func() {
		buildinfo.TelemetryHost = prevHost
		buildinfo.Version = prevVersion
		buildinfo.TelemetryWebsiteID = prevWebsiteID
	}()
	buildinfo.TelemetryHost = ""
	buildinfo.Version = "v1.2.3"
	buildinfo.TelemetryWebsiteID = "embedded-website"

	t.Setenv(umamiHostEnv, "")
	t.Setenv(umamiWebsiteIDEnv, "")

	sink := Default()
	client, ok := sink.(*Client)
	if !ok {
		t.Fatalf("Default() type = %T, want *Client", sink)
	}
	if client.endpoint != defaultHost+"/api/send" {
		t.Fatalf("endpoint = %q, want %q", client.endpoint, defaultHost+"/api/send")
	}
}

func TestDefaultDisablesTelemetryWhenEnvIsOff(t *testing.T) {
	prevSink := defaultSink
	defaultSink = nil
	defer func() { defaultSink = prevSink }()

	prevWebsiteID := buildinfo.TelemetryWebsiteID
	defer func() {
		buildinfo.TelemetryWebsiteID = prevWebsiteID
	}()
	buildinfo.TelemetryWebsiteID = "embedded-website"

	t.Setenv("NO_MISTAKES_TELEMETRY", "off")
	t.Setenv(umamiWebsiteIDEnv, "website-from-env")

	if _, ok := Default().(*Client); ok {
		t.Fatal("Default() should disable telemetry when NO_MISTAKES_TELEMETRY=off")
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

func TestParseDotEnvStripsInlineCommentsFromUnquotedValues(t *testing.T) {
	values := parseDotEnv([]byte("NO_MISTAKES_UMAMI_WEBSITE_ID=abc123 # dev\n"))

	if got := values[umamiWebsiteIDEnv]; got != "abc123" {
		t.Fatalf("website ID = %q, want %q", got, "abc123")
	}
}

func TestParseDotEnvPreservesHashesInQuotedValues(t *testing.T) {
	values := parseDotEnv([]byte("NO_MISTAKES_UMAMI_WEBSITE_ID=\"abc # dev\"\n"))

	if got := values[umamiWebsiteIDEnv]; got != "abc # dev" {
		t.Fatalf("website ID = %q, want %q", got, "abc # dev")
	}
}
