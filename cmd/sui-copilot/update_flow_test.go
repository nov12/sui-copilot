package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"sui-copilot/internal/selfupdate"
	"sui-copilot/internal/service"
)

// fakeManager records service commands and reports a configurable program
// path, standing in for the real init backends.
type fakeManager struct {
	programPath  string
	pathErr      error
	restartErr   error
	restartCalls int
}

func (m *fakeManager) Name() string { return "fake" }
func (m *fakeManager) Install(service.Options) error {
	return errors.New("not used in these tests")
}
func (m *fakeManager) Uninstall() error { return errors.New("not used in these tests") }
func (m *fakeManager) Start() error     { return errors.New("not used in these tests") }
func (m *fakeManager) Stop() error      { return errors.New("not used in these tests") }
func (m *fakeManager) Restart() error {
	m.restartCalls++
	return m.restartErr
}
func (m *fakeManager) Status() (string, error) { return "", nil }

// InstalledProgramPath implements service.ProgramPathReader.
func (m *fakeManager) InstalledProgramPath() (string, error) {
	return m.programPath, m.pathErr
}

// fakeUpdateServer serves the release endpoints and asset downloads the
// update command needs, pointing selfupdate.APIBase at itself for the test.
type fakeUpdateServer struct {
	server  *httptest.Server
	tagName string
	binary  []byte
	sums    string
}

func newFakeUpdateServer(t *testing.T, tagName string, binary []byte) *fakeUpdateServer {
	t.Helper()
	sum := sha256.Sum256(binary)
	fake := &fakeUpdateServer{
		tagName: tagName,
		binary:  binary,
		sums:    hex.EncodeToString(sum[:]) + "  " + assetName + "\n",
	}
	mux := http.NewServeMux()
	writeRelease := func(w http.ResponseWriter, tag string) {
		// The command resolves download URLs from the release payload, so the
		// JSON must carry the real URLs of this test server.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": tag,
			"assets": []map[string]string{
				{"name": assetName, "browser_download_url": fake.server.URL + "/download/" + assetName},
				{"name": "SHA256SUMS", "browser_download_url": fake.server.URL + "/download/SHA256SUMS"},
			},
		})
	}
	mux.HandleFunc("/api/repos/"+repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		writeRelease(w, fake.tagName)
	})
	mux.HandleFunc("/api/repos/"+repo+"/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		tag := strings.TrimPrefix(r.URL.Path, "/api/repos/"+repo+"/releases/tags/")
		if tag != fake.tagName {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeRelease(w, fake.tagName)
	})
	mux.HandleFunc("/download/"+assetName, func(w http.ResponseWriter, r *http.Request) {
		w.Write(fake.binary)
	})
	mux.HandleFunc("/download/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(fake.sums))
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	selfupdate.APIBase = fake.server.URL + "/api"
	t.Cleanup(func() { selfupdate.APIBase = "https://api.github.com" })
	return fake
}

const repo = selfupdate.DefaultRepo

// assetName is the release asset for the platform the tests run on, matching
// what AssetNameForExecutable requests on that platform.
var assetName = selfupdate.AssetName(runtime.GOOS, runtime.GOARCH)

// magicForPlatform returns a plausible binary header for the platform the
// tests run on, so the fixture passes the executable-magic check.
func magicForPlatform() []byte {
	switch runtime.GOOS {
	case "darwin":
		return []byte{0xcf, 0xfa, 0xed, 0xfe} // Mach-O 64-bit little-endian
	case "windows":
		return []byte{'M', 'Z', 0x90, 0x00} // PE
	default:
		return []byte{0x7f, 'E', 'L', 'F'} // ELF
	}
}

// installUpdateFixture builds a temporary installation with a fake old binary
// named like a release asset, and restores the production test hooks and the
// working directory when the test ends.
func installUpdateFixture(t *testing.T) (dir string, restore func()) {
	t.Helper()
	dir = t.TempDir()
	binary := filepath.Join(dir, assetName)
	// Platform magic: the update command rejects assets without an executable
	// header even before extraction.
	content := append(magicForPlatform(), bytes.Repeat([]byte{0}, 64)...)
	if err := os.WriteFile(binary, content, 0o755); err != nil {
		t.Fatal(err)
	}
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	workingDir = dir
	restore = func() {
		workingDir = ""
		os.Chdir(originalDir)
	}
	return dir, restore
}

// workingDir tracks the fixture directory for withHooks.
var workingDir = ""

// withHooks swaps the production test hooks for test doubles and restores
// them afterwards.
func withHooks(t *testing.T, manager *fakeManager, restart func(string) error) {
	t.Helper()
	withHooksVersion(t, manager, restart, "dev")
}

// withHooksVersion additionally pins the reported running version so tests
// can exercise downgrade and upgrade decisions.
func withHooksVersion(t *testing.T, manager *fakeManager, restart func(string) error, version string) {
	t.Helper()
	originalDetect := detectService
	originalRoot := requireRootCheck
	originalRestart := updateRestartService
	originalExecutable := currentExecutablePath
	originalVersion := currentVersion
	detectService = func() service.Manager { return manager }
	requireRootCheck = func() bool { return false }
	updateRestartService = restart
	currentExecutablePath = filepath.Join(workingDir, assetName)
	currentVersion = func() string { return version }
	t.Cleanup(func() {
		detectService = originalDetect
		requireRootCheck = originalRoot
		updateRestartService = originalRestart
		currentExecutablePath = originalExecutable
		currentVersion = originalVersion
	})
}

func markerPath(dir string) string { return filepath.Join(dir, assetName+".restart-pending") }

// binaryContent returns the first bytes of the installed binary.
func binaryContent(t *testing.T, path string) []byte {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	return got
}

func TestUpdateReplacesBinaryAndClearsMarker(t *testing.T) {
	dir, restore := installUpdateFixture(t)
	defer restore()
	newBinary := append([]byte{0x7f, 'E', 'L', 'F'}, bytes.Repeat([]byte{'N'}, 100)...)
	newFakeUpdateServer(t, "v9.8.0", newBinary)
	manager := &fakeManager{programPath: filepath.Join(dir, assetName)}
	withHooks(t, manager, restartServiceAfterUpdate)

	if err := runUpdate([]string{}); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if !bytes.Equal(binaryContent(t, filepath.Join(dir, assetName)), newBinary) {
		t.Error("installed binary was not replaced")
	}
	if _, err := os.Stat(markerPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("marker should be cleared after a successful restart")
	}
	if manager.restartCalls != 1 {
		t.Errorf("restart calls = %d, want 1", manager.restartCalls)
	}
}

func TestUpdateRestartFailureKeepsMarkerForRecovery(t *testing.T) {
	dir, restore := installUpdateFixture(t)
	defer restore()
	newFakeUpdateServer(t, "v9.8.0", []byte{0x7f, 'E', 'L', 'F', 1})
	manager := &fakeManager{
		programPath: filepath.Join(dir, assetName),
		restartErr:  errors.New("systemctl failed"),
	}
	withHooks(t, manager, restartServiceAfterUpdate)

	err := runUpdate([]string{})
	if err == nil || !strings.Contains(err.Error(), "重启服务失败") {
		t.Fatalf("want restart failure error, got %v", err)
	}
	if _, err := os.Stat(markerPath(dir)); err != nil {
		t.Errorf("marker must survive a failed restart for the next run to recover, stat err = %v", err)
	}

	// A later run must complete the pending restart before anything else,
	// even when the release query fails. The unreachable API aborts the run
	// before replacement, but only after the recovery restart.
	manager.restartErr = nil
	selfupdate.APIBase = "http://127.0.0.1:1"
	t.Cleanup(func() { selfupdate.APIBase = "https://api.github.com" })
	err = runUpdate([]string{})
	if err == nil || !strings.Contains(err.Error(), "查询 Release 失败") {
		t.Fatalf("recovery run should fail on the unreachable API after restarting, got %v", err)
	}
	if manager.restartCalls != 2 {
		t.Errorf("restart calls after recovery = %d, want 2", manager.restartCalls)
	}
	if _, err := os.Stat(markerPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("marker should be cleared once the restart succeeded")
	}
}

func TestUpdateMarkFailureAbortsBeforeDiskChange(t *testing.T) {
	dir, restore := installUpdateFixture(t)
	defer restore()
	oldContent := binaryContent(t, filepath.Join(dir, assetName))
	newFakeUpdateServer(t, "v9.8.0", []byte{0x7f, 'E', 'L', 'F', 2})
	manager := &fakeManager{programPath: filepath.Join(dir, assetName)}
	// The pending-restart marker must be writable by root but not by others;
	// simulate an unwritable location with a directory nested into the marker
	// path. MarkRestartPending fails with ENOTDIR only when the parent of the
	// marker is a file, so turn the marker path into a non-empty directory
	// tree via a pre-created marker file inside it.
	blocked := filepath.Join(dir, assetName+".restart-pending")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "keep"), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	restarts := 0
	withHooks(t, manager, func(string) error { restarts++; return nil })

	if err := runUpdate([]string{}); err == nil || !strings.Contains(err.Error(), "记录待重启状态失败") {
		t.Fatalf("want marker write failure, got %v", err)
	}
	if !bytes.Equal(binaryContent(t, filepath.Join(dir, assetName)), oldContent) {
		t.Error("binary must stay unchanged when the marker cannot be written")
	}
	// The blocked marker path makes the recovery branch fire once before the
	// abort; the service itself must never restart and no replacement may
	// happen.
	if manager.restartCalls != 0 || restarts != 1 {
		t.Errorf("restarts = %d/%d, want 0/1 (only the recovery restart)", manager.restartCalls, restarts)
	}
}

func TestUpdateSkipsOlderReleaseByDefault(t *testing.T) {
	dir, restore := installUpdateFixture(t)
	defer restore()
	oldContent := binaryContent(t, filepath.Join(dir, assetName))
	newFakeUpdateServer(t, "v0.0.1", []byte{0x7f, 'E', 'L', 'F', 3})
	manager := &fakeManager{programPath: filepath.Join(dir, assetName)}
	withHooksVersion(t, manager, restartServiceAfterUpdate, "v9.9.9")

	if err := runUpdate([]string{}); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if !bytes.Equal(binaryContent(t, filepath.Join(dir, assetName)), oldContent) {
		t.Error("default update must not downgrade")
	}
	if manager.restartCalls != 0 {
		t.Errorf("restart calls = %d, want 0", manager.restartCalls)
	}
}

func TestRestartServiceAfterUpdateClearsMarkerWhenNotInstalled(t *testing.T) {
	dir, restore := installUpdateFixture(t)
	defer restore()
	if err := os.WriteFile(markerPath(dir), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &fakeManager{pathErr: service.ErrNotInstalled}
	withHooks(t, manager, restartServiceAfterUpdate)

	if err := restartServiceAfterUpdate(filepath.Join(dir, assetName)); err != nil {
		t.Fatalf("restartServiceAfterUpdate: %v", err)
	}
	if _, err := os.Stat(markerPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("no service installed: marker should be cleared")
	}
}

func TestRestartServiceAfterUpdateFailsOnReadError(t *testing.T) {
	dir, restore := installUpdateFixture(t)
	defer restore()
	if err := os.WriteFile(markerPath(dir), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &fakeManager{pathErr: errors.New("permission denied")}
	withHooks(t, manager, restartServiceAfterUpdate)

	err := restartServiceAfterUpdate(filepath.Join(dir, assetName))
	if err == nil || !strings.Contains(err.Error(), "无法读取服务定义") {
		t.Fatalf("want read failure error, got %v", err)
	}
	if _, err := os.Stat(markerPath(dir)); err != nil {
		t.Errorf("marker must survive an unknown service state, stat err = %v", err)
	}
}

func TestRestartServiceAfterUpdateSkipsForeignService(t *testing.T) {
	dir, restore := installUpdateFixture(t)
	defer restore()
	if err := os.WriteFile(markerPath(dir), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &fakeManager{programPath: "/elsewhere/sui-copilot"}
	withHooks(t, manager, restartServiceAfterUpdate)

	if err := restartServiceAfterUpdate(filepath.Join(dir, assetName)); err != nil {
		t.Fatalf("restartServiceAfterUpdate: %v", err)
	}
	if manager.restartCalls != 0 {
		t.Errorf("restart calls = %d, want 0 for a foreign service path", manager.restartCalls)
	}
	if _, err := os.Stat(markerPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("foreign service: marker should be cleared")
	}
}

func TestServiceInstallRejectsMissingExplicitConfig(t *testing.T) {
	dir, restore := installUpdateFixture(t)
	defer restore()
	manager := &fakeManager{}
	withHooks(t, manager, restartServiceAfterUpdate)

	err := runService([]string{"install", "--config", filepath.Join(dir, "missing.toml")})
	if err == nil || !strings.Contains(err.Error(), "无法读取配置文件") {
		t.Fatalf("want missing config error, got %v", err)
	}
}

func TestCheckOnlyLeavesDirectoryUntouched(t *testing.T) {
	dir, restore := installUpdateFixture(t)
	defer restore()
	newFakeUpdateServer(t, "v9.8.0", []byte{0x7f, 'E', 'L', 'F', 4})
	withHooks(t, &fakeManager{}, restartServiceAfterUpdate)

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if err := runUpdate([]string{"--check"}); err != nil {
		t.Fatalf("runUpdate --check: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != assetName {
			t.Errorf("--check must not create files, found %q", entry.Name())
		}
	}
}
