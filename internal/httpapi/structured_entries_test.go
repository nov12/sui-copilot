package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"sui-copilot/internal/accounting"
	"sui-copilot/internal/config"
	"sui-copilot/internal/store"
	"sui-copilot/internal/sui"
)

func TestStructuredEntryThroughHTTPAndMCP(t *testing.T) {
	server, db := newStructuredEntryTestServer(t)
	var writes atomic.Int32
	server.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/report_index.do":
			body = `<li data-bookId="book-1" class="current">测试账簿</li>`
		case "/tally/payout.rmi":
			writes.Add(1)
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			if r.Form.Get("category") != "101" || r.Form.Get("account") != "201" || r.Form.Get("price") != "35.00" || r.Form.Get("memo") != "午饭" {
				t.Errorf("unexpected submitted fields: %v", r.Form)
			}
			body = `{"id":12345}`
		default:
			t.Errorf("unexpected upstream request: %s", r.URL.Path)
			return nil, errors.New("unexpected upstream request")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})

	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "integration-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpServer.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 3 {
		t.Fatalf("list MCP tools: %+v, %v", tools, err)
	}
	book := callMCPTool[bookMetadataResult](t, ctx, session, "get_book_metadata", struct{}{})
	if book.BookID != "book-1" || len(book.ExpenseCategories) != 1 || book.ExpenseCategories[0] != "餐饮 / 午餐" || len(book.Accounts) != 1 {
		t.Fatalf("unexpected book metadata: %+v", book)
	}

	req := exampleStructuredRequest()
	created := postStructuredEntry(t, server, req, http.StatusAccepted)
	if created.EntryID == 0 || created.State != accounting.StateQueued || created.Existing {
		t.Fatalf("unexpected HTTP creation result: %+v", created)
	}
	waitForEntryState(t, db, created.EntryID, accounting.StateCommitted)
	retried := callMCPTool[structuredEntryResponse](t, ctx, session, "submit_entry", req)
	if !retried.Existing || retried.EntryID != created.EntryID || retried.State != accounting.StateCommitted {
		t.Fatalf("MCP retry did not return the record created by HTTP: %+v", retried)
	}
	status := callMCPTool[entryStatus](t, ctx, session, "get_entry_status", entryStatusRequest{created.EntryID})
	if status.EntryID != created.EntryID || status.State != accounting.StateCommitted || writes.Load() != 1 {
		t.Fatalf("unexpected status or write count: %+v, %d", status, writes.Load())
	}

	req.ClientRequestID = "needs-review"
	req.Draft.NeedsConfirmation = true
	pending := callMCPTool[structuredEntryResponse](t, ctx, session, "submit_entry", req)
	entry := waitForEntryState(t, db, pending.EntryID, accounting.StateNeedsConfirmation)
	if pending.Existing || pending.EntryID == created.EntryID || writes.Load() != 1 {
		t.Fatalf("unexpected result for flagged entry: %+v, writes=%d", pending, writes.Load())
	}
	if _, err := db.UpdateDraft(ctx, entry.ID, entry.Version, strings.Replace(entry.DraftJSON, "35.00", "36.00", 1)); err != nil {
		t.Fatal(err)
	}
	if retried := postStructuredEntry(t, server, req, http.StatusOK); !retried.Existing || retried.EntryID != entry.ID {
		t.Fatalf("editing the draft changed retry identity: %+v", retried)
	}
	req.Draft.Memo = "不同的原始账目"
	postStructuredEntry(t, server, req, http.StatusConflict)
}

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

func callMCPTool[T any](t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, args any) T {
	t.Helper()
	response, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("MCP %s: %v", name, err)
	}
	if err := response.GetError(); err != nil {
		t.Fatalf("MCP %s: %v", name, err)
	}
	raw, err := json.Marshal(response.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var result T
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
