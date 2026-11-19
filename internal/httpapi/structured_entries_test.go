package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"sui-copilot/internal/accounting"
	"sui-copilot/internal/config"
	"sui-copilot/internal/store"
	"sui-copilot/internal/sui"
)

func TestStructuredEntryKeepsOriginalBook(t *testing.T) {
	server, db := newStructuredEntryTestServer(t)
	req := exampleStructuredRequest()
	req.BookID = "different-book"
	created := postStructuredEntry(t, server, req, http.StatusAccepted)
	entry := waitForEntryState(t, db, created.EntryID, accounting.StateNeedsConfirmation)
	if entry.ErrorCode != "entry_book_mismatch" || entry.BookID != req.BookID {
		t.Fatalf("book mismatch was not retained: %+v", entry)
	}
	path := "/api/v1/entries/" + strconv.FormatInt(entry.ID, 10)
	status := httptest.NewRecorder()
	server.Handler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, path, nil))
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"state":"needs_confirmation"`) {
		t.Fatalf("unexpected HTTP status response: %d %s", status.Code, status.Body)
	}
	confirm := httptest.NewRecorder()
	server.Handler().ServeHTTP(confirm, httptest.NewRequest(http.MethodPost, path+"/confirm", nil))
	if confirm.Code != http.StatusUnprocessableEntity || !strings.Contains(confirm.Body.String(), `"code":"entry_book_mismatch"`) {
		t.Fatalf("confirmation bypassed book validation: %d %s", confirm.Code, confirm.Body)
	}
}

func newStructuredEntryTestServer(t *testing.T) (*Server, *store.DB) {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/structured.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	server := NewServer(db, slog.Default(), config.Config{
		Sui: withTestSession(t, config.SuiConfig{BaseURL: "https://sui.example", BookName: "测试账簿"}),
	})
	server.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected upstream request: %s", r.URL.Path)
		return nil, errors.New("unexpected upstream request")
	})
	metadata := sui.Metadata{CurrentBook: sui.Book{ID: "book-1", Name: "测试账簿", Current: true}}
	metadata.Categories.Expense = []sui.Item{{ID: "101", Name: "午餐", ParentName: "餐饮", Level: 2, Leaf: true}}
	metadata.Accounts = []sui.Item{{ID: "201", Name: "银行卡"}}
	payload, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMetadata(t.Context(), metadata.CurrentBook.ID, string(payload)); err != nil {
		t.Fatal(err)
	}
	return server, db
}

func exampleStructuredRequest() structuredEntryRequest {
	return structuredEntryRequest{
		ClientRequestID: "lunch-1", BookID: "book-1",
		Draft: structuredDraft{
			Type: "expense", Date: "2026-11-19 12:30", Amount: "35.00",
			CategoryName: "餐饮 / 午餐", AccountName: "银行卡", Memo: "午饭",
		},
	}
}

func postStructuredEntry(t *testing.T, server *Server, input structuredEntryRequest, wantStatus int) structuredEntryResponse {
	t.Helper()
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/entries", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != wantStatus {
		t.Fatalf("submit HTTP entry: status=%d body=%s", response.Code, response.Body)
	}
	var result structuredEntryResponse
	if wantStatus < 400 {
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
	}
	return result
}
