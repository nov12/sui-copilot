package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testClient() *http.Client { return &http.Client{Timeout: 10 * time.Second} }

// fakeGitHub serves /repos/{repo}/releases/latest and /releases/tags/{tag}
// with an in-memory release and downloadable assets.
type fakeGitHub struct {
	server  *httptest.Server
	release *Release
	bodies  map[string][]byte
}

func newFakeGitHub(t *testing.T, release *Release, bodies map[string][]byte) *fakeGitHub {
	t.Helper()
	fake := &fakeGitHub{release: release, bodies: bodies}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/nov12/sui-copilot/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, fake.release)
	})
	mux.HandleFunc("/repos/nov12/sui-copilot/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		tag := strings.TrimPrefix(r.URL.Path, "/repos/nov12/sui-copilot/releases/tags/")
		if tag != fake.release.TagName {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, fake.release)
	})
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/download/")
		body, ok := fake.bodies[name]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Write(body)
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	APIBase = fake.server.URL
	t.Cleanup(func() { APIBase = "https://api.github.com" })
	return fake
}

func (f *fakeGitHub) downloadURL(name string) string {
	return f.server.URL + "/download/" + name
}

func writeJSON(w http.ResponseWriter, release *Release) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"tag_name": release.TagName,
		"assets":   release.Assets,
	})
}

func TestParseBaseVersion(t *testing.T) {
	cases := []struct {
		input    string
		major    int
		minor    int
		patch    int
		ok       bool
		against  string
		compares int
	}{
		{input: "v0.1.0", major: 0, minor: 1, patch: 0, ok: true, against: "v0.2.0", compares: -1},
		{input: "0.1.0", major: 0, minor: 1, patch: 0, ok: true},
		{input: "v0.1.0-5-gabcdef", major: 0, minor: 1, patch: 0, ok: true, against: "v0.1.0", compares: 0},
		{input: "v0.1.0-dirty", major: 0, minor: 1, patch: 0, ok: true, against: "v0.1.0", compares: 0},
		{input: "dev", ok: false, against: "v0.0.1", compares: -1},
		{input: "", ok: false, against: "v0.0.1", compares: -1},
		{input: "v1.0.0", major: 1, minor: 0, patch: 0, ok: true, against: "v0.9.9", compares: 1},
	}
	for _, test := range cases {
		major, minor, patch, ok := ParseBaseVersion(test.input)
		if ok != test.ok || major != test.major || minor != test.minor || patch != test.patch {
			t.Errorf("ParseBaseVersion(%q) = %d.%d.%d, %v; want %d.%d.%d, %v", test.input, major, minor, patch, ok, test.major, test.minor, test.patch, test.ok)
		}
		if test.against != "" && CompareBaseVersions(test.input, test.against) != test.compares {
			t.Errorf("CompareBaseVersions(%q, %q) = %d, want %d", test.input, test.against, CompareBaseVersions(test.input, test.against), test.compares)
		}
	}
}

func TestFindChecksum(t *testing.T) {
	digestA := strings.Repeat("a", 64)
	digestB := strings.Repeat("b", 64)
	digestC := strings.Repeat("c", 64)
	content := digestA + "  sui-copilot-linux-amd64\n" + digestB + " *sui-copilot-windows-amd64.exe\n" + digestC + "  sui-copilot.toml.sample\n"
	if got, ok := findChecksum(content, "sui-copilot-linux-amd64"); !ok || got != digestA {
		t.Errorf("findChecksum simple = %q, %v", got, ok)
	}
	if got, ok := findChecksum(content, "sui-copilot-windows-amd64.exe"); !ok || got != digestB {
		t.Errorf("findChecksum binary-mode = %q, %v", got, ok)
	}
	if _, ok := findChecksum(content, "missing"); ok {
		t.Error("missing entry should not be found")
	}
	if _, ok := findChecksum("short  file\n", "file"); ok {
		t.Error("invalid digest line should be ignored")
	}
}

func TestAssetName(t *testing.T) {
	if got := AssetName("linux", "amd64"); got != "sui-copilot-linux-amd64" {
		t.Errorf("AssetName linux/amd64 = %q", got)
	}
	if got := AssetName("windows", "amd64"); got != "sui-copilot-windows-amd64.exe" {
		t.Errorf("AssetName windows/amd64 = %q", got)
	}
}

func TestArchFromExecutableName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sui-copilot-linux-armv7")
	if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	// os.Executable cannot be stubbed directly; exercise the parsing logic
	// through the exported behavior instead.
	if got := archFromName(filepath.Base(path)); got != "armv7" {
		t.Errorf("archFromName(armv7) = %q", got)
	}
	if got := archFromName("sui-copilot-linux-amd64"); got != "" {
		t.Errorf("archFromName(amd64) = %q", got)
	}
	if got := archFromName("sui-copilot-linux-arm64"); got != "" {
		t.Errorf("archFromName(arm64) = %q", got)
	}
	if got := archFromName("busybox"); got != "" {
		t.Errorf("archFromName(busybox) = %q", got)
	}
}

func TestFetchReleaseLatest(t *testing.T) {
	release := &Release{TagName: "v0.2.0", Assets: []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}{}}
	fake := newFakeGitHub(t, release, nil)
	got, err := FetchRelease(context.Background(), testClient(), DefaultRepo, "")
	if err != nil {
		t.Fatalf("FetchRelease: %v", err)
	}
	if got.TagName != "v0.2.0" {
		t.Errorf("tag = %q", got.TagName)
	}
	_ = fake
}

func TestFetchReleaseByTag(t *testing.T) {
	release := &Release{TagName: "v0.1.0"}
	newFakeGitHub(t, release, nil)
	got, err := FetchRelease(context.Background(), testClient(), DefaultRepo, "v0.1.0")
	if err != nil {
		t.Fatalf("FetchRelease: %v", err)
	}
	if got.TagName != "v0.1.0" {
		t.Errorf("tag = %q", got.TagName)
	}
	if _, err := FetchRelease(context.Background(), testClient(), DefaultRepo, "v9.9.9"); err == nil {
		t.Error("unknown tag should fail")
	}
}

// buildUpgradeFixture builds a raw release asset with known content and
// returns the asset bytes, the binary content, and the expected SHA-256 of the
// asset as published in SHA256SUMS.
func buildUpgradeFixture(t *testing.T, content []byte) (asset []byte, binary []byte, digest string) {
	t.Helper()
	sum := sha256.Sum256(content)
	return content, content, hex.EncodeToString(sum[:])
}

func TestFetchAndVerifyAsset(t *testing.T) {
	binary := append([]byte{0x7f, 'E', 'L', 'F'}, bytes.Repeat([]byte{0}, 64)...)
	archive, _, digest := buildUpgradeFixture(t, binary)
	assetName := "sui-copilot-linux-amd64"
	sums := digest + "  " + assetName + "\n" + "deadbeef  sui-copilot.toml.sample\n"

	release := &Release{TagName: "v0.2.0"}
	release.Assets = append(release.Assets, struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}{Name: assetName, URL: ""}, struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}{Name: "SHA256SUMS", URL: ""})

	bodies := map[string][]byte{assetName: archive, "SHA256SUMS": []byte(sums)}
	fake := newFakeGitHub(t, release, bodies)
	// Fill real download URLs now that the server exists.
	for index := range release.Assets {
		release.Assets[index].URL = fake.downloadURL(release.Assets[index].Name)
	}

	dir := t.TempDir()
	extractDir := filepath.Join(dir, "extract")
	if err := os.MkdirAll(extractDir, 0o700); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(dir, assetName)
	downloadDigest, err := DownloadAndVerify(context.Background(), testClient(), release, assetName, archivePath)
	if err != nil {
		t.Fatalf("DownloadAndVerify: %v", err)
	}
	if downloadDigest != digest {
		t.Errorf("digest = %q, want %q", downloadDigest, digest)
	}

	extracted, err := ExtractBinary(archivePath, extractDir)
	if err != nil {
		t.Fatalf("ExtractBinary: %v", err)
	}
	got, err := os.ReadFile(extracted)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, binary) {
		t.Errorf("extracted content mismatch")
	}
}

func TestDownloadVerifyWrongChecksum(t *testing.T) {
	binary := []byte("binary")
	archive, _, digest := buildUpgradeFixture(t, binary)
	assetName := "sui-copilot-linux-amd64"
	wrongSums := strings.Repeat("0", 64) + "  " + assetName + "\n"
	_ = digest

	release := &Release{TagName: "v0.2.0"}
	release.Assets = append(release.Assets,
		struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}{Name: assetName, URL: ""},
		struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}{Name: "SHA256SUMS", URL: ""},
	)
	bodies := map[string][]byte{assetName: archive, "SHA256SUMS": []byte(wrongSums)}
	fake := newFakeGitHub(t, release, bodies)
	for index := range release.Assets {
		release.Assets[index].URL = fake.downloadURL(release.Assets[index].Name)
	}

	dir := t.TempDir()
	if _, err := DownloadAndVerify(context.Background(), testClient(), release, assetName, filepath.Join(dir, assetName)); !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("want ErrChecksumMismatch, got %v", err)
	}
}

func TestDownloadVerifyMissingSums(t *testing.T) {
	binary := []byte("binary")
	archive, _, _ := buildUpgradeFixture(t, binary)
	assetName := "sui-copilot-linux-amd64"

	release := &Release{TagName: "v0.2.0"}
	release.Assets = append(release.Assets,
		struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}{Name: assetName, URL: ""},
	)
	bodies := map[string][]byte{assetName: archive}
	fake := newFakeGitHub(t, release, bodies)
	for index := range release.Assets {
		release.Assets[index].URL = fake.downloadURL(release.Assets[index].Name)
	}

	dir := t.TempDir()
	if _, err := DownloadAndVerify(context.Background(), testClient(), release, assetName, filepath.Join(dir, assetName)); !errors.Is(err, ErrChecksumMissing) {
		t.Errorf("want ErrChecksumMissing, got %v", err)
	}
}

func TestReplaceBinary(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "sui-copilot")
	if err := os.WriteFile(executable, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBinary := filepath.Join(dir, "staged-new")
	if err := os.WriteFile(newBinary, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceBinary(executable, newBinary); err != nil {
		t.Fatalf("ReplaceBinary: %v", err)
	}
	got, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Errorf("executable content = %q, want new", got)
	}
	info, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Error("executable bit not set")
	}
	if _, err := os.Stat(newBinary); !errors.Is(err, os.ErrNotExist) {
		t.Error("staged file should be consumed by the rename")
	}
	if _, err := os.Stat(executable + ".old"); !errors.Is(err, os.ErrNotExist) {
		t.Error("no .old backup should remain")
	}
	if _, err := os.Stat(executable + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Error("no shared .new staging file should remain")
	}
}

func TestReplaceBinaryConcurrent(t *testing.T) {
	// Two updates staging independently and racing the final rename must
	// leave exactly one complete version installed, and neither may keep
	// writing into the installed file afterwards.
	dir := t.TempDir()
	executable := filepath.Join(dir, "sui-copilot")
	if err := os.WriteFile(executable, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(dir, "staged-first")
	second := filepath.Join(dir, "staged-second")
	firstContent := bytes.Repeat([]byte("A"), 1<<20)
	secondContent := bytes.Repeat([]byte("B"), 1<<20)
	if err := os.WriteFile(first, firstContent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, secondContent, 0o755); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 2)
	go func() {
		time.Sleep(time.Millisecond)
		done <- ReplaceBinary(executable, first)
	}()
	go func() {
		done <- ReplaceBinary(executable, second)
	}()
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent ReplaceBinary: %v", err)
		}
	}
	got, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, firstContent) && !bytes.Equal(got, secondContent) {
		t.Fatalf("installed file is a mix of concurrent updates: %d bytes", len(got))
	}
	if len(got) != len(firstContent) {
		t.Errorf("installed size = %d, want %d", len(got), len(firstContent))
	}
}

func TestRestartPendingMarker(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "sui-copilot")
	if err := os.WriteFile(executable, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if RestartPending(executable) {
		t.Error("no marker expected initially")
	}
	if err := MarkRestartPending(executable); err != nil {
		t.Fatalf("MarkRestartPending: %v", err)
	}
	if !RestartPending(executable) {
		t.Error("marker should be present after MarkRestartPending")
	}
	if err := ClearRestartPending(executable); err != nil {
		t.Fatalf("ClearRestartPending: %v", err)
	}
	if RestartPending(executable) {
		t.Error("marker should be gone after ClearRestartPending")
	}
	// Clearing without a marker is not an error.
	if err := ClearRestartPending(executable); err != nil {
		t.Errorf("ClearRestartPending without marker: %v", err)
	}
}

func TestReplaceBinaryKeepsExecutablePathPopulated(t *testing.T) {
	// The rename onto the executable path must be the only step that touches
	// the path: an interrupted update must leave either the old or the new
	// binary in place, never a missing file.
	dir := t.TempDir()
	executable := filepath.Join(dir, "sui-copilot")
	if err := os.WriteFile(executable, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBinary := filepath.Join(dir, "staged-new")
	if err := os.WriteFile(newBinary, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceBinary(executable, newBinary); err != nil {
		t.Fatalf("ReplaceBinary: %v", err)
	}
	got, err := os.ReadFile(executable)
	if err != nil {
		t.Fatalf("executable missing after replace: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("executable content = %q, want new", got)
	}
}

func TestIsExecutableMagic(t *testing.T) {
	cases := map[string][]byte{
		"elf":           {0x7f, 'E', 'L', 'F', 0x02},
		"pe":            {'M', 'Z', 0x90, 0x00},
		"mach-o-64-le":  {0xcf, 0xfa, 0xed, 0xfe},
		"mach-o-32-le":  {0xce, 0xfa, 0xed, 0xfe},
		"mach-o-64-be":  {0xfe, 0xed, 0xfa, 0xcf},
		"mach-o-32-be":  {0xfe, 0xed, 0xfa, 0xce},
		"mach-o-fat":    {0xca, 0xfe, 0xba, 0xbe},
		"mach-o-fat-64": {0xca, 0xfe, 0xba, 0xbf},
		"shell-script":  []byte("#!/bin/sh\n"),
		"text":          []byte("hello world"),
		"empty":         {},
		"gzip":          {0x1f, 0x8b, 0x08, 0x00},
		"zip":           {'P', 'K', 0x03, 0x04},
	}
	want := map[string]bool{
		"elf": true, "pe": true,
		"mach-o-64-le": true, "mach-o-32-le": true, "mach-o-64-be": true, "mach-o-32-be": true,
		"mach-o-fat": true, "mach-o-fat-64": true,
	}
	for name, header := range cases {
		if got := isExecutableMagic(header); got != want[name] {
			t.Errorf("isExecutableMagic(%s) = %v, want %v", name, got, want[name])
		}
	}
}

func TestExtractBinaryMachOLittleEndian(t *testing.T) {
	// darwin/amd64 and darwin/arm64 release binaries are little-endian
	// Mach-O raw assets.
	content := append([]byte{0xcf, 0xfa, 0xed, 0xfe}, make([]byte, 64)...)
	dir := t.TempDir()
	downloadDir := filepath.Join(dir, "download")
	if err := os.MkdirAll(downloadDir, 0o700); err != nil {
		t.Fatal(err)
	}
	assetPath := filepath.Join(downloadDir, "sui-copilot-darwin-amd64")
	if err := os.WriteFile(assetPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	extracted, err := ExtractBinary(assetPath, dir)
	if err != nil {
		t.Fatalf("ExtractBinary: %v", err)
	}
	got, err := os.ReadFile(extracted)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Error("raw Mach-O content mismatch")
	}
	if want := filepath.Join(dir, "sui-copilot-darwin-amd64"); extracted != want {
		t.Errorf("raw asset should extract to %q, got %q", want, extracted)
	}
}

func TestAssetURL(t *testing.T) {
	release := &Release{TagName: "v0.2.0"}
	release.Assets = append(release.Assets,
		struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}{Name: "sui-copilot-linux-amd64", URL: "https://example.com/x"},
	)
	if got, err := release.assetURL("sui-copilot-linux-amd64"); err != nil || got != "https://example.com/x" {
		t.Errorf("assetURL = %q, %v", got, err)
	}
	if _, err := release.assetURL("sui-copilot-linux-armv7"); !errors.Is(err, ErrNoAsset) {
		t.Errorf("want ErrNoAsset, got %v", err)
	}
}
