package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type archiveEntry struct {
	name    string
	content []byte
	mode    os.FileMode
}

func writeArchive(t *testing.T, archiveName string, entries []archiveEntry) string {
	t.Helper()
	var content bytes.Buffer
	if filepath.Ext(archiveName) == ".zip" {
		archive := zip.NewWriter(&content)
		for _, entry := range entries {
			header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
			header.SetMode(entry.mode)
			writer, err := archive.CreateHeader(header)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(entry.content); err != nil {
				t.Fatal(err)
			}
		}
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		compressed := gzip.NewWriter(&content)
		archive := tar.NewWriter(compressed)
		for _, entry := range entries {
			header := &tar.Header{Name: entry.name, Mode: 0o755, Size: int64(len(entry.content)), Typeflag: tar.TypeReg}
			if entry.mode&os.ModeSymlink != 0 {
				header.Typeflag = tar.TypeSymlink
				header.Linkname = "../outside"
				header.Size = 0
			}
			if err := archive.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if header.Typeflag == tar.TypeReg {
				if _, err := archive.Write(entry.content); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
		if err := compressed.Close(); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), archiveName)
	if err := os.WriteFile(path, content.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDownloadName(t *testing.T) {
	for _, platform := range []struct{ binary, archive string }{
		{"sui-copilot-linux-amd64", "sui-copilot-linux-amd64.tar.gz"},
		{"sui-copilot-linux-armv7", "sui-copilot-linux-armv7.tar.gz"},
		{"sui-copilot-darwin-arm64", "sui-copilot-darwin-arm64.tar.gz"},
		{"sui-copilot-freebsd-amd64", "sui-copilot-freebsd-amd64.tar.gz"},
		{"sui-copilot-windows-amd64.exe", "sui-copilot-windows-amd64.zip"},
	} {
		t.Run(platform.binary, func(t *testing.T) {
			release := &Release{}
			if _, err := release.DownloadName(platform.binary); !errors.Is(err, ErrNoAsset) {
				t.Fatalf("missing platform: %v", err)
			}
			for _, name := range []string{platform.binary, platform.archive} {
				release.Assets = append(release.Assets, struct {
					Name string `json:"name"`
					URL  string `json:"browser_download_url"`
				}{Name: name, URL: "https://example.com/" + name})
				got, err := release.DownloadName(platform.binary)
				if err != nil || got != name {
					t.Fatalf("DownloadName = %q, %v; want %q", got, err, name)
				}
			}
		})
	}
}

func TestExtractArchives(t *testing.T) {
	for _, platform := range []struct {
		binary, archive string
		content         []byte
	}{
		{"sui-copilot-linux-armv7", "sui-copilot-linux-armv7.tar.gz", []byte{0x7f, 'E', 'L', 'F', 1}},
		{"sui-copilot-darwin-arm64", "sui-copilot-darwin-arm64.tar.gz", []byte{0xcf, 0xfa, 0xed, 0xfe, 1}},
		{"sui-copilot-windows-amd64.exe", "sui-copilot-windows-amd64.zip", []byte{'M', 'Z', 0x90, 0, 1}},
	} {
		t.Run(platform.archive, func(t *testing.T) {
			destination := t.TempDir()
			configPath := filepath.Join(destination, "sui-copilot.toml")
			if err := os.WriteFile(configPath, []byte("existing config"), 0o600); err != nil {
				t.Fatal(err)
			}
			archivePath := writeArchive(t, platform.archive, []archiveEntry{
				{name: "sui-copilot.toml.sample", content: []byte("sample")},
				{name: "../outside", content: []byte("ignored")},
				{name: platform.binary, content: platform.content, mode: 0o755},
			})
			got, err := ExtractBinary(archivePath, destination)
			if err != nil {
				t.Fatal(err)
			}
			if got != filepath.Join(destination, platform.binary) {
				t.Fatalf("unexpected executable path: %s", got)
			}
			content, err := os.ReadFile(got)
			if err != nil || !bytes.Equal(content, platform.content) {
				t.Fatalf("extracted executable = %x, %v", content, err)
			}
			entries, err := os.ReadDir(destination)
			if err != nil || len(entries) != 2 {
				t.Fatalf("only executable and existing config should exist: %v, %v", entries, err)
			}
			config, err := os.ReadFile(configPath)
			if err != nil || string(config) != "existing config" {
				t.Fatalf("configuration changed: %s, %v", config, err)
			}
			if _, err := ExtractBinary(archivePath, destination); err == nil {
				t.Fatal("must not overwrite an existing destination")
			}
		})
	}
}

func TestRejectInvalidArchiveExecutables(t *testing.T) {
	content := []byte{0x7f, 'E', 'L', 'F', 1}
	for _, format := range []struct{ binary, archive string }{
		{"sui-copilot-linux-amd64", "sui-copilot-linux-amd64.tar.gz"},
		{"sui-copilot-windows-amd64.exe", "sui-copilot-windows-amd64.zip"},
	} {
		for _, scenario := range []struct {
			name    string
			entries []archiveEntry
		}{
			{"missing", []archiveEntry{{name: "sui-copilot.toml.sample", content: []byte("sample")}}},
			{"traversal", []archiveEntry{{name: "../" + format.binary, content: content}}},
			{"not-executable", []archiveEntry{{name: format.binary, content: []byte("text")}}},
			{"symlink", []archiveEntry{{name: format.binary, mode: os.ModeSymlink | 0o755}}},
			{"duplicate", []archiveEntry{{name: format.binary, content: content}, {name: format.binary, content: content}}},
		} {
			t.Run(format.archive+"/"+scenario.name, func(t *testing.T) {
				archivePath := writeArchive(t, format.archive, scenario.entries)
				if _, err := ExtractBinary(archivePath, t.TempDir()); err == nil {
					t.Fatal("invalid executable should be rejected")
				}
			})
		}
		t.Run(format.archive+"/corrupt", func(t *testing.T) {
			archivePath := filepath.Join(t.TempDir(), format.archive)
			if err := os.WriteFile(archivePath, []byte("not an archive"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ExtractBinary(archivePath, t.TempDir()); err == nil {
				t.Fatal("corrupt archive should be rejected")
			}
		})
	}
}

func TestRejectTruncatedGzipArchive(t *testing.T) {
	archivePath := writeArchive(t, "sui-copilot-linux-amd64.tar.gz", []archiveEntry{
		{name: "sui-copilot-linux-amd64", content: []byte{0x7f, 'E', 'L', 'F', 1}},
	})
	content, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, content[:len(content)-8], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractBinary(archivePath, t.TempDir()); err == nil {
		t.Fatal("missing gzip trailer should be rejected")
	}
}

func TestRejectOversizedTarExecutable(t *testing.T) {
	var content bytes.Buffer
	compressed := gzip.NewWriter(&content)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: "sui-copilot-linux-amd64", Size: maxExecutableSize + 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "sui-copilot-linux-amd64.tar.gz")
	if err := os.WriteFile(archivePath, content.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractBinary(archivePath, t.TempDir()); err == nil {
		t.Fatal("oversized executable should be rejected")
	}
}
