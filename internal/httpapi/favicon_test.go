package httpapi

import (
	"bytes"
	"encoding/xml"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sui-copilot/internal/store"
)

func TestPageFavicons(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	handler := NewServer(db, slog.Default()).Handler()
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("index status=%d", page.Code)
	}
	for _, path := range []string{"/favicon.ico", "/favicon.svg"} {
		t.Run(path, func(t *testing.T) {
			if !strings.Contains(page.Body.String(), `href="`+path+`"`) {
				t.Fatalf("page does not reference %s", path)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("icon status=%d body=%s", response.Code, response.Body.String())
			}
			contentType := response.Header().Get("Content-Type")
			if path == "/favicon.svg" {
				if contentType != "image/svg+xml" {
					t.Fatalf("SVG content type=%q", contentType)
				}
				var icon struct{ XMLName xml.Name }
				if err := xml.Unmarshal(response.Body.Bytes(), &icon); err != nil {
					t.Fatal(err)
				}
				if icon.XMLName.Local != "svg" || icon.XMLName.Space != "http://www.w3.org/2000/svg" {
					t.Fatalf("invalid SVG root: %+v", icon.XMLName)
				}
			} else {
				if contentType != "image/vnd.microsoft.icon" && contentType != "image/x-icon" {
					t.Fatalf("ICO content type=%q", contentType)
				}
				if !bytes.HasPrefix(response.Body.Bytes(), []byte{0, 0, 1, 0}) {
					t.Fatal("response is not an ICO image")
				}
			}
			head := httptest.NewRecorder()
			handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, path, nil))
			if head.Code != http.StatusOK || head.Header().Get("Content-Type") != contentType || head.Body.Len() != 0 {
				t.Fatalf("invalid icon HEAD response: status=%d headers=%v body=%q", head.Code, head.Header(), head.Body.String())
			}
		})
	}
}
