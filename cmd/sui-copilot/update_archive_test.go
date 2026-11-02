package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"sui-copilot/internal/selfupdate"
)

func updateArchive(t *testing.T, binary []byte) (string, []byte) {
	t.Helper()
	var content bytes.Buffer
	if runtime.GOOS == "windows" {
		archive := zip.NewWriter(&content)
		writer, err := archive.Create(assetName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(binary); err != nil {
			t.Fatal(err)
		}
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSuffix(assetName, ".exe") + ".zip", content.Bytes()
	}
	compressed := gzip.NewWriter(&content)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: assetName, Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return assetName + ".tar.gz", content.Bytes()
}

func TestUpdateFromArchive(t *testing.T) {
	for _, scenario := range []string{"success", "checksum-mismatch", "not-executable"} {
		t.Run(scenario, func(t *testing.T) {
			dir, restore := installUpdateFixture(t)
			defer restore()
			executable := filepath.Join(dir, assetName)
			oldContent := binaryContent(t, executable)
			newContent := append(magicForPlatform(), []byte("new program")...)
			if scenario == "not-executable" {
				newContent = []byte("not an executable")
			}
			downloadName, content := updateArchive(t, newContent)
			server := newFakeUpdateAssetServer(t, "v9.8.0", downloadName, content)
			if scenario == "checksum-mismatch" {
				server.sums = strings.Repeat("0", 64) + "  " + downloadName + "\n"
			}
			restarts := 0
			withHooks(t, &fakeManager{programPath: executable}, func(string) error { restarts++; return nil })
			err := runUpdate(nil)
			if scenario == "success" {
				if err != nil || !bytes.Equal(binaryContent(t, executable), newContent) || restarts != 1 {
					t.Fatalf("archive update failed: %v, restarts %d", err, restarts)
				}
			} else {
				if err == nil || !bytes.Equal(binaryContent(t, executable), oldContent) || restarts != 0 {
					t.Fatalf("invalid asset changed installation: %v, restarts %d", err, restarts)
				}
				if scenario == "checksum-mismatch" && !errors.Is(err, selfupdate.ErrChecksumMismatch) {
					t.Fatalf("want checksum mismatch, got %v", err)
				}
				if selfupdate.RestartPending(executable) {
					t.Fatal("failed update left a restart marker")
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".sui-copilot-update-") {
					t.Fatal("temporary update directory was not removed")
				}
			}
		})
	}
}
