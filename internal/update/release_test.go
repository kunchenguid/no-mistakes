package update

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestPickReleaseAssets(t *testing.T) {
	assets := []releaseAsset{
		{Name: "no-mistakes-v1.2.3-darwin-arm64.tar.gz", BrowserDownloadURL: "https://example.com/archive"},
		{Name: "checksums.txt", BrowserDownloadURL: "https://example.com/checksums"},
		{Name: "ignored.txt", BrowserDownloadURL: "https://example.com/ignored"},
	}

	archive, checksums, err := pickReleaseAssets("no-mistakes", "v1.2.3", assets, platformSpec{GOOS: "darwin", GOARCH: "arm64"})
	if err != nil {
		t.Fatalf("pickReleaseAssets error = %v", err)
	}
	if archive.Name != "no-mistakes-v1.2.3-darwin-arm64.tar.gz" {
		t.Fatalf("archive = %q", archive.Name)
	}
	if checksums.Name != "checksums.txt" {
		t.Fatalf("checksums = %q", checksums.Name)
	}

	_, _, err = pickReleaseAssets("no-mistakes", "v1.2.3", assets[:1], platformSpec{GOOS: "darwin", GOARCH: "arm64"})
	if err == nil {
		t.Fatal("pickReleaseAssets should fail when checksums asset is missing")
	}
}

func TestParseChecksums(t *testing.T) {
	archiveName := "no-mistakes-v1.2.3-darwin-arm64.tar.gz"
	data := []byte("abc123  no-mistakes-v1.2.3-darwin-arm64.tar.gz\ndef456  other.tar.gz\n")

	checksums, err := parseChecksums(data)
	if err != nil {
		t.Fatalf("parseChecksums error = %v", err)
	}
	if checksums[archiveName] != "abc123" {
		t.Fatalf("checksum = %q", checksums[archiveName])
	}

	if _, err := parseChecksums([]byte("bad-line")); err == nil {
		t.Fatal("parseChecksums should reject malformed input")
	}
}

func TestVerifyChecksum(t *testing.T) {
	payload := []byte("archive-bytes")
	hash := sha256.Sum256(payload)
	want := hex.EncodeToString(hash[:])

	if err := verifyChecksum(payload, want); err != nil {
		t.Fatalf("verifyChecksum error = %v", err)
	}
	if err := verifyChecksum(payload, stringsRepeat("0", 64)); err == nil {
		t.Fatal("verifyChecksum should fail on mismatch")
	}
}

func TestEnsureHTTPS(t *testing.T) {
	if err := ensureHTTPS("https://example.com/release"); err != nil {
		t.Fatalf("ensureHTTPS rejected https url: %v", err)
	}
	if err := ensureHTTPS("http://example.com/release"); err == nil {
		t.Fatal("ensureHTTPS should reject http urls")
	}
}
