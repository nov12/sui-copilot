package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"sui-copilot/internal/buildinfo"
	"sui-copilot/internal/selfupdate"
)

const updateCheckInterval = 6 * time.Hour

type updateStatus struct {
	CurrentVersion  string `json:"currentVersion"`
	LatestVersion   string `json:"latestVersion"`
	UpdateAvailable bool   `json:"updateAvailable"`
	ReleaseURL      string `json:"releaseUrl"`
	CheckedAt       string `json:"checkedAt"`
}

func (s *Server) refreshUpdateStatus(ctx context.Context) (updateStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	release, err := selfupdate.FetchRelease(ctx, s.httpClient, selfupdate.DefaultRepo, "")
	if err != nil {
		return updateStatus{}, err
	}
	if _, _, _, ok := selfupdate.ParseBaseVersion(release.TagName); !ok {
		return updateStatus{}, fmt.Errorf("invalid release version %q", release.TagName)
	}
	status := updateStatus{
		CurrentVersion:  buildinfo.Version,
		LatestVersion:   release.TagName,
		UpdateAvailable: selfupdate.CompareBaseVersions(buildinfo.Version, release.TagName) < 0,
		ReleaseURL:      "https://github.com/" + selfupdate.DefaultRepo + "/releases/tag/" + url.PathEscape(release.TagName),
	}
	s.updateMu.Lock()
	status.CheckedAt = time.Now().UTC().Format(time.RFC3339Nano)
	s.lastUpdate = status
	s.updateMu.Unlock()
	return status, nil
}

func (s *Server) checkUpdate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	status, err := s.refreshUpdateStatus(r.Context())
	if err != nil {
		s.logger.Warn("check for update failed", "error", err)
		writeError(w, http.StatusBadGateway, "update_check_failed", "检查更新失败，请稍后重试")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) getUpdateStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.updateMu.RLock()
	status := s.lastUpdate
	s.updateMu.RUnlock()
	status.CurrentVersion = buildinfo.Version
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) runUpdateCheckLoop(ctx context.Context) {
	ticker := time.NewTicker(updateCheckInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.refreshUpdateStatus(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("automatic update check failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
