package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "nm-update-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create test NM_HOME: %v\n", err)
		os.Exit(1)
	}
	home, err := os.MkdirTemp("", "nm-update-home-")
	if err != nil {
		_ = os.RemoveAll(root)
		fmt.Fprintf(os.Stderr, "create test HOME: %v\n", err)
		os.Exit(1)
	}
	_ = os.Setenv("NM_HOME", root)
	_ = os.Setenv("HOME", home)
	_ = os.Setenv("NO_MISTAKES_TELEMETRY", "off")

	code := m.Run()

	_ = os.RemoveAll(root)
	_ = os.RemoveAll(home)
	os.Exit(code)
}

func stringsRepeat(s string, count int) string {
	buf := bytes.NewBuffer(nil)
	for i := 0; i < count; i++ {
		buf.WriteString(s)
	}
	return buf.String()
}

func makeTarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
