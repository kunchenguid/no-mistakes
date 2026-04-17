package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"path/filepath"
)

func (u *updater) extractBinary(archive []byte) ([]byte, error) {
	name := binaryName(u.appName, u.platform)
	if u.platform.GOOS == "windows" {
		return extractBinaryFromZip(archive, name)
	}
	return extractBinaryFromTarGz(archive, name)
}

func extractBinaryFromTarGz(archive []byte, binaryName string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open tar.gz: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar.gz: %w", err)
		}
		if filepath.Base(hdr.Name) != binaryName {
			continue
		}
		return readExtractedBinary(tr)
	}
	return nil, fmt.Errorf("binary not found in tar.gz: %s", binaryName)
}

func extractBinaryFromZip(archive []byte, binaryName string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	for _, file := range zr.File {
		if filepath.Base(file.Name) != binaryName {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open zip entry: %w", err)
		}
		defer rc.Close()
		return readExtractedBinary(rc)
	}
	return nil, fmt.Errorf("binary not found in zip: %s", binaryName)
}

func readExtractedBinary(r io.Reader) ([]byte, error) {
	limited := io.LimitReader(r, maxExtractedSize+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read extracted binary: %w", err)
	}
	if len(data) > maxExtractedSize {
		return nil, fmt.Errorf("extracted binary exceeds %d bytes", maxExtractedSize)
	}
	return data, nil
}
