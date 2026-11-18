package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sui-copilot/internal/sui"
)

// Every upstream Sui operation holds suiMu for its entire duration. This
// keeps a session exchange from racing with a write, book switch, or SSO
// recovery that would save the old session over the new one.
func (s *Server) suiClient() *sui.Client {
	cfg := s.runtimeConfig()
	client := &sui.Client{BaseURL: cfg.Sui.BaseURL, BookName: cfg.Sui.BookName, HTTP: s.httpClient}
	if s.suiSession == nil {
		session, err := sui.LoadSession(cfg.Sui.SessionFile, cfg.Sui.SessionKeyFile)
		switch {
		case errors.Is(err, os.ErrNotExist), errors.Is(err, sui.ErrSessionNotConfigured):
			client.SessionErr = sui.ErrSessionNotConfigured
		case err != nil:
			client.SessionErr = sui.ErrSessionStoreCorrupted
		default:
			s.suiSession = session
		}
	}
	if s.suiSession != nil {
		client.Session = s.suiSession
		client.Jar = s.suiSession.Jar
	}
	return client
}

func (s *Server) checkSuiSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	s.suiMu.Lock()
	defer s.suiMu.Unlock()
	if _, err := s.suiClient().ListBooks(ctx); err != nil {
		status, code, message := suiError(err)
		writeError(w, status, code, message)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "authenticated"})
}

var suiLoginOriginProtection http.CrossOriginProtection

func (s *Server) loginSui(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if suiLoginOriginProtection.Check(r) != nil {
		writeError(w, http.StatusForbidden, "invalid_origin", "请在当前网页内登录随手记")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "invalid_content_type", "登录请求必须使用 JSON")
		return
	}
	var request struct {
		Account  string `json:"account"`
		Password string `json:"password"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(new(any)) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_json", "登录请求格式不正确")
		return
	}
	request.Account = strings.TrimSpace(request.Account)
	if request.Account == "" || request.Password == "" {
		writeError(w, http.StatusBadRequest, "sui_credentials_required", "请输入随手记账号和密码")
		return
	}

	if !s.suiMu.TryLock() {
		writeError(w, http.StatusConflict, "sui_busy", "随手记正在处理其他请求，请稍后再试")
		return
	}
	defer s.suiMu.Unlock()

	cfg := s.runtimeConfig().Sui
	session, err := newSuiLoginSession(cfg.SessionFile, cfg.SessionKeyFile)
	if err != nil {
		s.writeSuiSessionSaveError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	metadata, err := session.Authenticate(ctx, s.httpClient, cfg.BaseURL, request.Account, request.Password)
	if ctx.Err() != nil {
		writeError(w, http.StatusGatewayTimeout, "sui_login_timeout", "随手记登录超时，请稍后重试")
		return
	}
	if errors.Is(err, sui.ErrLoginRejected) {
		writeError(w, http.StatusUnauthorized, "sui_login_rejected", "随手记未接受登录，请核对账号和密码，或在随手记官网完成账号验证")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "sui_login_failed", "暂时无法完成随手记登录，请稍后重试")
		return
	}
	verifiedBookID := ""
	if sui.ValidateBook(metadata, cfg.BookName) == nil {
		verifiedBookID = metadata.CurrentBook.ID
	}
	metadataReady, err := s.db.PrepareSuiLogin(ctx, verifiedBookID)
	if err != nil {
		s.logger.Error("invalidate sui cache before login failed", "error", err)
		writeError(w, http.StatusInternalServerError, "store_error", "无法更新本地账簿缓存，请检查数据目录后重新登录")
		return
	}
	if err := session.Save(); err != nil {
		s.writeSuiSessionSaveError(w, err)
		return
	}
	s.suiSession = session
	s.previewSalt = rand.Text()

	updatedAt, err := s.saveSuiBookCatalog(ctx, metadata)
	if err != nil {
		// The login is already usable. An empty timestamp tells the UI that
		// the returned book list wasn't cached and can be synced separately.
		s.logger.Warn("cache sui book list after login failed", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "authenticated",
		"currentBookName": metadata.CurrentBook.Name,
		"books":           publicBooks(metadata.Books),
		"updatedAt":       updatedAt,
		"metadataReady":   metadataReady,
	})
}

func newSuiLoginSession(sessionPath, keyPath string) (*sui.Session, error) {
	if sessionPath == "" || keyPath == "" {
		return nil, sui.ErrSessionNotConfigured
	}
	for _, path := range []string{sessionPath, keyPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
	}
	return sui.NewSession(sessionPath, keyPath)
}

func (s *Server) writeSuiSessionSaveError(w http.ResponseWriter, err error) {
	s.logger.Error("save sui login session failed", "error", err)
	writeError(w, http.StatusInternalServerError, "sui_session_write_failed", "无法保存随手记会话，请检查会话文件及数据目录权限")
}
