package telemetry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
)

// The resolved collector configuration no longer reaches a sender in this fork
// (see Default), so these tests assert the resolvers themselves rather than the
// sink they used to build.
func TestTelemetryConfigUsesDotEnvInDevBuildWhenEnvMissing(t *testing.T) {
	prevHost := buildinfo.TelemetryHost
	prevWebsiteID := buildinfo.TelemetryWebsiteID
	defer func() {
		buildinfo.TelemetryHost = prevHost
		buildinfo.TelemetryWebsiteID = prevWebsiteID
	}()
	buildinfo.TelemetryHost = ""
	buildinfo.TelemetryWebsiteID = ""

	t.Setenv(telemetryEnv, "")
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

	if got := defaultHostValue(); got != "https://dotenv.example" {
		t.Fatalf("host = %q, want %q", got, "https://dotenv.example")
	}
	if got := defaultWebsiteID(); got != "website-from-dotenv" {
		t.Fatalf("websiteID = %q, want %q", got, "website-from-dotenv")
	}
}

func TestTelemetryConfigPrefersEnvVarsOverDotEnvAndEmbeddedConfig(t *testing.T) {
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

	t.Setenv(telemetryEnv, "")
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

	if got := defaultHostValue(); got != "https://env.example" {
		t.Fatalf("host = %q, want %q", got, "https://env.example")
	}
	if got := defaultWebsiteID(); got != "website-from-env" {
		t.Fatalf("websiteID = %q, want %q", got, "website-from-env")
	}
}

func TestTelemetryConfigUsesEmbeddedHostAndWebsiteID(t *testing.T) {
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

	t.Setenv(telemetryEnv, "")
	t.Setenv(umamiHostEnv, "")
	t.Setenv(umamiWebsiteIDEnv, "")

	if got := defaultHostValue(); got != "https://embedded.example" {
		t.Fatalf("host = %q, want %q", got, "https://embedded.example")
	}
	if got := defaultWebsiteID(); got != "embedded-website" {
		t.Fatalf("websiteID = %q, want %q", got, "embedded-website")
	}
}

func TestTelemetryConfigUsesSelfHostedHostWhenHostConfigMissing(t *testing.T) {
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

	t.Setenv(telemetryEnv, "")
	t.Setenv(umamiHostEnv, "")
	t.Setenv(umamiWebsiteIDEnv, "")

	if got := defaultHostValue(); got != defaultHost {
		t.Fatalf("host = %q, want %q", got, defaultHost)
	}
}

func TestTelemetryConfigReadsOffEnvAsDisabled(t *testing.T) {
	t.Setenv("NO_MISTAKES_TELEMETRY", "off")

	if !telemetryDisabled() {
		t.Fatal("telemetryDisabled() = false, want true when NO_MISTAKES_TELEMETRY=off")
	}
}

func TestTelemetryConfigIgnoresDotEnvOutsideRepo(t *testing.T) {
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

	if got := defaultWebsiteID(); got != "" {
		t.Fatalf("websiteID = %q, want empty: dotenv outside the repo is ignored", got)
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
