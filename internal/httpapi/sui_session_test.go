package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sui-copilot/internal/config"
	"sui-copilot/internal/store"
)

func TestWebSuiLoginReplacesExpiredSessionInRunningServer(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Sui.SessionFile = filepath.Join(dir, "sessions", "session.bin")
	cfg.Sui.SessionKeyFile = filepath.Join(dir, "keys", "session.key")
	cfg.Sui.BookName = "我的账簿"
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var transportCalls int
	cookieValue := "mock-first-session"
	bookID := "317"
	rejectLogin := false
	upstream := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		transportCalls++
		status := http.StatusOK
		body := ""
		headers := make(http.Header)
		switch r.URL.Host + r.URL.Path {
		case "login.sui.com/login.do":
			if r.URL.Query().Get("opt") == "vccode" {
				body = `{"vccode":"challenge","uid":"mock-uid"}`
			} else if rejectLogin {
				body = `{"status":"error"}`
			} else {
				if r.URL.Query().Get("email") != "mock@example.com" || r.URL.Query().Get("password") == "mock-account-password" {
					t.Fatal("login did not use a trimmed account and a password digest")
				}
				headers.Set("Set-Cookie", "session="+cookieValue+"; Domain=sui.com; Path=/; Secure; HttpOnly")
				body = `{"status":"ok"}`
			}
		case "login.sui.com/auth.do":
			body = `authenticated`
		case "www.sui.com/report_index.do", "www.sui.com/tally/new.do":
			cookie, err := r.Cookie("session")
			if err != nil || cookie.Value != cookieValue {
				status = http.StatusUnauthorized
			} else {
				body = `<li data-bookId="` + bookID + `" class="current">我的账簿</li>`
			}
		default:
			t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	server := NewServer(db, slog.Default(), cfg)
	server.httpClient = upstream
	check := func(s *Server, method string, status int, fields ...string) {
		t.Helper()
		request := httptest.NewRequest(method, "https://copilot.example/api/v1/sui/session", strings.NewReader(`{"account":" mock@example.com ","password":"mock-account-password"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "https://copilot.example")
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, request)
		body := response.Body.String()
		if response.Code != status || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Set-Cookie") != "" {
			t.Fatalf("unexpected %s response: %d %s", method, response.Code, body)
		}
		for _, forbidden := range []string{"mock@example.com", "mock-account-password", cookieValue, `"id"`, "auth login"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("unexpected field in %s response: %q", method, forbidden)
			}
		}
		for _, field := range fields {
			if !strings.Contains(body, field) {
				t.Fatalf("response is missing %q: %s", field, body)
			}
		}
	}
	check(server, http.MethodGet, http.StatusBadRequest, `"code":"sui_session_not_configured"`)
	if transportCalls != 0 {
		t.Fatal("missing session check contacted Sui")
	}
	check(server, http.MethodPost, http.StatusOK, `"status":"authenticated"`, `"currentBookName":"我的账簿"`, `"metadataReady":false`)
	check(server, http.MethodGet, http.StatusOK, `"status":"authenticated"`)
	if _, err := db.LatestBookCatalog(context.Background()); err != nil {
		t.Fatalf("first login did not cache the book list: %v", err)
	}
	original, err := os.ReadFile(cfg.Sui.SessionFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(original, []byte(cookieValue)) {
		t.Fatal("session was stored in plaintext")
	}

	ctx := context.Background()
	const cached = `{"currentBook":{"id":"317","name":"我的账簿"},"accounts":[{"id":"1","name":"原账号账户"}]}`
	if err := db.SaveMetadata(ctx, bookID, cached); err != nil {
		t.Fatal(err)
	}
	check(server, http.MethodPost, http.StatusOK, `"metadataReady":true`)
	original, err = os.ReadFile(cfg.Sui.SessionFile)
	if err != nil {
		t.Fatal(err)
	}
	originalPreview := server.previewContextID(bookID)
	cookieValue = "mock-renewed-session"
	bookID = "318" // Another account, same configured book name.
	check(server, http.MethodGet, http.StatusUnauthorized, `"code":"sui_session_expired"`)
	rejectLogin = true
	check(server, http.MethodPost, http.StatusUnauthorized, `"code":"sui_login_rejected"`)
	current, err := os.ReadFile(cfg.Sui.SessionFile)
	if err != nil || !bytes.Equal(original, current) {
		t.Fatalf("failed login replaced the saved session: %v", err)
	}
	check(server, http.MethodGet, http.StatusUnauthorized, `"code":"sui_session_expired"`)

	if _, err := db.LatestMetadata(ctx); err != nil || server.previewContextID("317") != originalPreview {
		t.Fatalf("rejected login invalidated current book or preview: %v", err)
	}
	rejectLogin = false
	check(server, http.MethodPost, http.StatusOK, `"metadataReady":false`)
	if server.previewContextID("317") == originalPreview {
		t.Fatal("successful login kept old preview revision")
	}
	check(server, http.MethodGet, http.StatusOK, `"status":"authenticated"`)
	restarted := NewServer(db, slog.Default(), cfg)
	restarted.httpClient = upstream
	check(restarted, http.MethodGet, http.StatusOK, `"status":"authenticated"`)
	for _, s := range []*Server{server, restarted} {
		metadata := httptest.NewRecorder()
		s.Handler().ServeHTTP(metadata, httptest.NewRequest(http.MethodGet, "/api/v1/metadata", nil))
		if metadata.Code != http.StatusOK || !strings.Contains(metadata.Body.String(), `"metadata":null`) {
			t.Fatalf("refresh returned previous account metadata: %s", metadata.Body.String())
		}
		if _, _, err := s.parseContext(ctx, time.Now()); !errors.Is(err, errCategoryMetadataNotSynced) {
			t.Fatalf("parser accepted previous account metadata: %v", err)
		}
	}
}

func TestWebSuiLoginRejectsInvalidRequests(t *testing.T) {
	const credentials = `{"account":"example","password":"secret"}`
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServer(db, slog.Default())
	for _, test := range []struct {
		name, origin, fetchSite, mediaType, body string
		status                                   int
	}{
		{"foreign origin", "https://external.example", "", "application/json", credentials, http.StatusForbidden},
		{"cross-site browser", "", "cross-site", "application/json", credentials, http.StatusForbidden},
		{"form", "", "", "application/x-www-form-urlencoded", "account=example&password=secret", http.StatusUnsupportedMediaType},
		{"empty account", "", "", "application/json", `{"account":" ","password":"secret"}`, http.StatusBadRequest},
		{"trailing JSON", "", "", "application/json", credentials + ` {}`, http.StatusBadRequest},
		{"oversized JSON", "", "", "application/json", credentials + strings.Repeat(" ", 4096), http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "https://copilot.example/api/v1/sui/session", strings.NewReader(test.body))
			r.Header.Set("Origin", test.origin)
			r.Header.Set("Sec-Fetch-Site", test.fetchSite)
			r.Header.Set("Content-Type", test.mediaType)
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, r)
			if w.Code != test.status || strings.Contains(w.Body.String(), "secret") {
				t.Fatalf("invalid request response: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
