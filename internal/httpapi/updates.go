package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
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
	CanAutoUpdate   bool   `json:"canAutoUpdate"`
	Updating        bool   `json:"updating"`
	UpdatingVersion string `json:"updatingVersion"`
	UpdateError     string `json:"updateError"`
}

func (s *Server) SetUpdateInstaller(installer func(string) error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	s.updateInstaller = installer
}

func (s *Server) updateStatusSnapshot() updateStatus {
	s.updateMu.RLock()
	defer s.updateMu.RUnlock()
	status := s.lastUpdate
	status.CurrentVersion = buildinfo.Version
	status.CanAutoUpdate = s.updateInstaller != nil
	status.Updating = s.updateVersion != ""
	status.UpdatingVersion = s.updateVersion
	status.UpdateError = s.updateError
	return status
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
	return s.updateStatusSnapshot(), nil
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
	writeJSON(w, http.StatusOK, s.updateStatusSnapshot())
}

func (s *Server) installUpdate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "invalid_content_type", "更新请求必须使用 JSON")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host != r.Host || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			writeError(w, http.StatusForbidden, "invalid_origin", "不允许跨站更新请求")
			return
		}
	}
	var request struct {
		Version string `json:"version"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "请指定要更新的版本")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "更新请求格式不正确")
		return
	}
	s.updateMu.Lock()
	installer := s.updateInstaller
	switch {
	case installer == nil:
		s.updateMu.Unlock()
		writeError(w, http.StatusNotImplemented, "manual_update_required", "当前运行方式不支持网页更新，请运行 sui-copilot update 并重启程序")
		return
	case s.updateVersion != "":
		s.updateMu.Unlock()
		writeError(w, http.StatusConflict, "update_in_progress", "正在更新，请勿重复操作")
		return
	case !s.lastUpdate.UpdateAvailable || request.Version == "" || request.Version != s.lastUpdate.LatestVersion:
		s.updateMu.Unlock()
		writeError(w, http.StatusConflict, "update_version_changed", "更新版本已变化，请重新检查更新")
		return
	}
	s.updateVersion = request.Version
	s.updateError = ""
	s.updateMu.Unlock()
	writeJSON(w, http.StatusAccepted, s.updateStatusSnapshot())
	_ = http.NewResponseController(w).Flush()
	go func() {
		err := installer(request.Version)
		s.updateMu.Lock()
		defer s.updateMu.Unlock()
		s.updateVersion = ""
		if err != nil {
			s.updateError = "更新失败：" + err.Error()
			s.logger.Error("install update failed", "version", request.Version, "error", err)
		}
	}()
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
