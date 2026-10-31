// Package selfupdate upgrades the running binary from GitHub Releases.
package selfupdate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// DefaultRepo is the GitHub repository queried for releases.
const DefaultRepo = "nov12/sui-copilot"

// APIBase is the GitHub API base URL. It is a variable so command-level tests
// can point the update command at a local server instead of GitHub.
var APIBase = "https://api.github.com"

// userAgent identifies requests to api.github.com, which requires one.
const userAgent = "sui-copilot-selfupdate"

// AssetName is the release asset file name for the running platform.
func AssetName(goos, arch string) string {
	name := "sui-copilot-" + goos + "-" + arch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// AssetNameForExecutable returns the release asset name for the platform.
// ARM32 variants are taken from the executable's own release-style file name
// when available; otherwise the software-float armv5 build is used, which
// runs on every ARM32 CPU regardless of floating-point support.
func AssetNameForExecutable(executable string) string {
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv5"
		if variant := archFromName(filepath.Base(executable)); variant != "" {
			arch = variant
		}
	}
	return AssetName(runtime.GOOS, arch)
}

// archFromName extracts the armvN variant from a release-style binary name.
func archFromName(base string) string {
	if !strings.HasPrefix(base, "sui-copilot-") {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(base, "sui-copilot-"), "-")
	if len(parts) < 2 {
		return ""
	}
	arch := parts[len(parts)-1]
	if arch == "armv5" || arch == "armv6" || arch == "armv7" {
		return arch
	}
	return ""
}

// Release is the subset of the GitHub release payload used here.
type Release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// checksumsName is the checksum manifest asset published with every release.
const checksumsName = "SHA256SUMS"

// ErrNoAsset reports that a release exists but has no binary for this platform.
var ErrNoAsset = errors.New("release has no asset for this platform")

// ErrChecksumMissing reports that the release provides no SHA256SUMS entry for
// the asset, so its integrity cannot be verified.
var ErrChecksumMissing = errors.New("release has no checksum for the asset")

// ErrChecksumMismatch reports that the downloaded asset digest does not match
// SHA256SUMS.
var ErrChecksumMismatch = errors.New("asset checksum mismatch")

// DownloadAndVerify downloads the platform asset to destinationPath and its
// SHA256SUMS from the same release, then verifies the digest. The returned
// digest is the computed SHA-256 of the downloaded file.
func DownloadAndVerify(ctx context.Context, client *http.Client, release *Release, assetName, destinationPath string) (string, error) {
	assetURL, err := release.assetURL(assetName)
	if err != nil {
		return "", err
	}
	sumsURL, err := release.assetURL(checksumsName)
	if err != nil {
		return "", fmt.Errorf("%w: SHA256SUMS asset is required for verification", ErrChecksumMissing)
	}
	sums, err := downloadString(ctx, client, sumsURL)
	if err != nil {
		return "", err
	}
	expected, ok := findChecksum(sums, assetName)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrChecksumMissing, assetName)
	}
	digest, err := downloadFile(ctx, client, assetURL, destinationPath)
	if err != nil {
		return "", err
	}
	if digest != expected {
		return "", fmt.Errorf("%w: got %s, want %s", ErrChecksumMismatch, digest, expected)
	}
	return digest, nil
}

// fetchJSON performs a GET and decodes the JSON body.
func fetchJSON(ctx context.Context, client *http.Client, endpoint string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: unexpected status %s", endpoint, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(target)
}

// repoRegexp validates the GitHub repository slug before it is used in API
// URLs.
var repoRegexp = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// FetchRelease returns the release for tag, or the latest release when tag is
// empty.
func FetchRelease(ctx context.Context, client *http.Client, repo, tag string) (*Release, error) {
	if !repoRegexp.MatchString(repo) {
		return nil, fmt.Errorf("invalid repository %q, want owner/name", repo)
	}
	endpoint := APIBase + "/repos/" + repo + "/releases/latest"
	if tag != "" {
		endpoint = APIBase + "/repos/" + repo + "/releases/tags/" + url.PathEscape(tag)
	}
	var release Release
	if err := fetchJSON(ctx, client, endpoint, &release); err != nil {
		return nil, err
	}
	if release.TagName == "" {
		return nil, fmt.Errorf("release response has no tag_name")
	}
	return &release, nil
}

// assetURL finds the download URL for the platform asset in the release.
func (r *Release) assetURL(name string) (string, error) {
	for _, asset := range r.Assets {
		if asset.Name == name {
			return asset.URL, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrNoAsset, name)
}

// versionRegexp matches release tags vX.Y.Z and build metadata suffixes from
// git describe output.
var versionRegexp = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

// ParseBaseVersion extracts the X.Y.Z triple from a version string. Local
// describe suffixes like -5-gabcdef or -dirty are ignored. The second return
// value is false when the version is not a release version, e.g. "dev".
func ParseBaseVersion(version string) (major, minor, patch int, ok bool) {
	match := versionRegexp.FindStringSubmatch(strings.TrimSpace(version))
	if match == nil {
		return 0, 0, 0, false
	}
	major, _ = strconv.Atoi(match[1])
	minor, _ = strconv.Atoi(match[2])
	patch, _ = strconv.Atoi(match[3])
	return major, minor, patch, true
}

// CompareBaseVersions returns -1 when a is older than b, 0 when equal, and 1
// when a is newer. Non-release versions sort below every release so that a
// self-built "dev" binary treats any release as an upgrade.
func CompareBaseVersions(a, b string) int {
	aMajor, aMinor, aPatch, aOK := ParseBaseVersion(a)
	bMajor, bMinor, bPatch, bOK := ParseBaseVersion(b)
	switch {
	case !aOK && !bOK:
		return 0
	case !aOK:
		return -1
	case !bOK:
		return 1
	}
	if aMajor != bMajor {
		return sign(aMajor - bMajor)
	}
	if aMinor != bMinor {
		return sign(aMinor - bMinor)
	}
	return sign(aPatch - bPatch)
}

func sign(value int) int {
	switch {
	case value < 0:
		return -1
	case value > 0:
		return 1
	default:
		return 0
	}
}

// shaLineRegexp matches one SHA256SUMS line: <hex> [space *]<filename>.
var shaLineRegexp = regexp.MustCompile(`^([0-9a-fA-F]{64})\s+\*?(.+)$`)

// findChecksum extracts the expected digest for name from SHA256SUMS content.
func findChecksum(content, name string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		match := shaLineRegexp.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		if match[2] == name {
			return strings.ToLower(match[1]), true
		}
	}
	return "", false
}

// downloadFile streams url to a local file and returns its SHA-256 hex digest.
func downloadFile(ctx context.Context, client *http.Client, rawURL, destination string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: unexpected status %s", rawURL, resp.Status)
	}
	hash := sha256.New()
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	_, copyErr := io.Copy(io.MultiWriter(file, hash), resp.Body)
	closeErr := file.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// downloadString fetches a small text asset such as SHA256SUMS.
func downloadString(ctx context.Context, client *http.Client, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: unexpected status %s", rawURL, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ExtractBinary materializes the executable from a release asset into
// destinationDir. Release assets are raw binaries; an ELF, Mach-O, or PE
// header is required so that a checksum-verified but unexpected payload fails
// loudly instead of being installed. The asset is copied through unchanged and
// the destination path is returned.
func ExtractBinary(archivePath, destinationDir string) (string, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	bufferedReader := bufio.NewReader(file)
	header, err := bufferedReader.Peek(4)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("inspect asset: %w", err)
	}
	if !isExecutableMagic(header) {
		return "", fmt.Errorf("asset is not a raw executable (want ELF, Mach-O, or PE)")
	}
	destination := filepath.Join(destinationDir, filepath.Base(archivePath))
	if same, err := isSameFile(archivePath, destination); err == nil && same {
		return "", fmt.Errorf("extraction destination equals the source file")
	}
	if err := copyFileMode(bufferedReader, destination, 0o755); err != nil {
		return "", err
	}
	return destination, nil
}

// isSameFile reports whether two paths refer to the same file.
func isSameFile(a, b string) (bool, error) {
	infoA, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	infoB, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(infoA, infoB), nil
}

// isExecutableMagic reports whether the leading bytes look like a raw
// executable for the platforms releases are built for: ELF, PE, or Mach-O in
// either byte order plus fat binaries.
func isExecutableMagic(header []byte) bool {
	if len(header) < 4 {
		return bytes.Equal(header, []byte("MZ"))
	}
	if bytes.Equal(header[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		return true
	}
	if bytes.Equal(header[:2], []byte("MZ")) {
		return true
	}
	switch {
	case bytes.Equal(header[:4], []byte{0xcf, 0xfa, 0xed, 0xfe}), // Mach-O 64-bit little-endian
		bytes.Equal(header[:4], []byte{0xce, 0xfa, 0xed, 0xfe}), // Mach-O 32-bit little-endian
		bytes.Equal(header[:4], []byte{0xfe, 0xed, 0xfa, 0xcf}), // Mach-O 64-bit big-endian
		bytes.Equal(header[:4], []byte{0xfe, 0xed, 0xfa, 0xce}), // Mach-O 32-bit big-endian
		bytes.Equal(header[:4], []byte{0xca, 0xfe, 0xba, 0xbe}), // Mach-O fat binary
		bytes.Equal(header[:4], []byte{0xca, 0xfe, 0xba, 0xbf}): // Mach-O fat 64-bit binary
		return true
	}
	return false
}

// ReplaceBinary atomically replaces the running executable with newBinary in
// one rename. newBinary must be complete (written and closed) and live on the
// same filesystem as executablePath; it is consumed by the rename. Because
// each update stages its own file, concurrent updates never share a staging
// path, and the executable path always holds one complete binary.
func ReplaceBinary(executablePath, newBinary string) error {
	info, err := os.Stat(executablePath)
	if err != nil {
		return fmt.Errorf("stat current executable: %w", err)
	}
	mode := info.Mode().Perm()
	if mode&0o100 == 0 {
		mode |= 0o100
	}
	if err := os.Chmod(newBinary, mode); err != nil {
		return fmt.Errorf("set mode on new binary: %w", err)
	}
	if err := os.Rename(newBinary, executablePath); err != nil {
		return fmt.Errorf("install new binary: %w", err)
	}
	return nil
}

// restartPendingSuffix marks a completed binary replacement whose service
// restart has not yet been confirmed. The marker lives next to the binary so
// it follows the installation and is removed together with it.
const restartPendingSuffix = ".restart-pending"

// MarkRestartPending records that executablePath was replaced but the service
// restart has not been confirmed yet.
func MarkRestartPending(executablePath string) error {
	return os.WriteFile(executablePath+restartPendingSuffix, []byte{}, 0o600)
}

// RestartPending reports whether a previous replacement is waiting for its
// service restart.
func RestartPending(executablePath string) bool {
	_, err := os.Stat(executablePath + restartPendingSuffix)
	return err == nil
}

// ClearRestartPending removes the marker after the restart succeeded or was
// determined to be unnecessary.
func ClearRestartPending(executablePath string) error {
	err := os.Remove(executablePath + restartPendingSuffix)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// copyFileMode copies from a reader to destination and applies mode.
func copyFileMode(source io.Reader, destination string, mode os.FileMode) error {
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, source); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
