package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"sui-copilot/internal/accounting"
	"sui-copilot/internal/config"
	"sui-copilot/internal/llm"
	"sui-copilot/internal/store"
	"sui-copilot/internal/sui"
)

func TestIngestIdempotencyAPI(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServerWithConfigPathAndDebug(db, slog.Default(), config.Config{
		Sui:        config.SuiConfig{BookName: "自己账本"},
		OpenRouter: config.OpenRouterConfig{APIKey: "test-api-key", Model: "openai/gpt-4o-mini", SupplementPrompt: "午餐优先归入食品"},
	}, "", true)
	metadata := sui.Metadata{}
	metadata.CurrentBook = sui.Book{ID: "1", Name: "自己账本", Current: true}
	metadata.Categories.Expense = []sui.Item{
		{ID: "100", Name: "日常", Type: "expense", Level: 1},
		{ID: "101", Name: "食品", Type: "expense", Level: 2, ParentID: "100", ParentName: "日常", Leaf: true},
	}
	metadata.Accounts = []sui.Item{{ID: "200", Name: "银行卡", Type: "account"}}
	payload, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMetadata(context.Background(), "1", string(payload)); err != nil {
		t.Fatal(err)
	}
	var parseCalls int
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		parseCalls++
		if r.URL.Path != "/api/v1/chat/completions" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		body := `{"choices":[{"message":{"content":"{\"type\":\"expense\",\"date\":\"2026-09-10 12:00\",\"amount\":\"35\",\"categoryName\":\"食品\",\"accountName\":\"银行卡\",\"storeName\":\"\",\"projectName\":\"\",\"memberName\":\"\",\"memo\":\"午饭\",\"confidence\":0.95,\"needsConfirmation\":false}"}}]}`
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	body := `{"source":"ios-shortcut","inputType":"text","text":"今天12:00午饭 35 元","clientRequestId":"shortcut-1"}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(body))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("unexpected status: %d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"state":"queued"`) {
		t.Fatalf("ingest did not return a queued entry immediately: %s", response.Body.String())
	}
	processed := waitForEntryState(t, db, 1, accounting.StatePendingSubmission)
	var first map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(body))
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("expected idempotent OK, got %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), `"id":`+strconv.FormatInt(processed.ID, 10)) {
		t.Fatalf("idempotent retry returned another entry: %s", response.Body.String())
	}
	if parseCalls != 1 {
		t.Fatalf("idempotent retry called the model %d times", parseCalls)
	}
}

func TestIngestRejectsDuplicateInputBeforeAIParsing(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServer(db, slog.Default(), config.Config{
		Sui:        config.SuiConfig{BookName: "自己账本"},
		OpenRouter: config.OpenRouterConfig{APIKey: "test-api-key", Model: "openai/gpt-4o-mini"},
	})
	draft, err := accounting.EncodeDraft(accounting.Draft{Type: "expense", Date: "2026-09-10 12:00", Amount: "35", CategoryName: "食品", AccountName: "银行卡"})
	if err != nil {
		t.Fatal(err)
	}
	existing, _, err := db.CreateEntry(context.Background(), "web", "duplicate-1", "text", " 午饭 35 元 ", draft)
	if err != nil {
		t.Fatal(err)
	}
	var parseCalls int
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		parseCalls++
		return nil, errors.New("AI should not be called for duplicate input")
	})}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(`{"text":"  午饭 35 元  "}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"duplicate_entry"`) || !strings.Contains(response.Body.String(), `"entryId":`+strconv.FormatInt(existing.ID, 10)) {
		t.Fatalf("unexpected duplicate response: %d body=%s", response.Code, response.Body.String())
	}
	if parseCalls != 0 {
		t.Fatalf("duplicate input called the model %d times", parseCalls)
	}
	entries, err := db.List(context.Background(), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != existing.ID {
		t.Fatalf("duplicate input created another entry: %+v", entries)
	}
}

func TestStartupRecoveryMarksProcessingEntryRejected(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	entry, _, err := db.CreateEntryWithState(context.Background(), "web", "startup-recovery-1", "text", "午饭 35 元", accounting.StateProcessing, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(db, slog.Default(), config.Config{})
	if err := server.recoverInterruptedEntries(context.Background()); err != nil {
		t.Fatal(err)
	}
	updated, err := db.Get(context.Background(), entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != accounting.StateRejected || updated.ErrorCode != "processing_interrupted" {
		t.Fatalf("startup recovery did not reject processing entry: %+v", updated)
	}
}

func TestInterruptProcessingCancelsInFlightParse(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	server := NewServer(db, slog.Default(), config.Config{
		Sui:        config.SuiConfig{BookName: "自己账本"},
		OpenRouter: config.OpenRouterConfig{APIKey: "test-api-key", Model: "openai/gpt-4o-mini"},
	})
	metadata := sui.Metadata{}
	metadata.CurrentBook = sui.Book{ID: "1", Name: "自己账本", Current: true}
	metadata.Categories.Expense = []sui.Item{{ID: "101", Name: "食品", Type: "expense", Level: 2, Leaf: true}}
	metadata.Accounts = []sui.Item{{ID: "201", Name: "银行卡", Type: "account", Leaf: true}}
	payload, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMetadata(context.Background(), "1", string(payload)); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	cancelled := make(chan struct{})
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
		return nil, r.Context().Err()
	})}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(`{"text":"今天12:00午饭 35 元"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("unexpected ingest status: %d body=%s", response.Code, response.Body.String())
	}
	entries, err := db.List(context.Background(), "", 50)
	if err != nil || len(entries) != 1 {
		t.Fatalf("unexpected entries: %+v err=%v", entries, err)
	}
	processing := waitForEntryState(t, db, entries[0].ID, accounting.StateProcessing)
	other, _, err := db.CreateEntryWithState(context.Background(), "web", "interrupt-other", "text", "晚饭 50 元", accounting.StateQueued, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("parse request did not start")
	}

	interrupt := httptest.NewRequest(http.MethodPost, "/api/v1/entries/"+strconv.FormatInt(processing.ID, 10)+"/interrupt", nil)
	interruptResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(interruptResponse, interrupt)
	if interruptResponse.Code != http.StatusOK || !strings.Contains(interruptResponse.Body.String(), `"processing_cancelled"`) {
		t.Fatalf("unexpected interrupt response: %d body=%s", interruptResponse.Code, interruptResponse.Body.String())
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight parse request was not cancelled")
	}

	updated, err := db.Get(context.Background(), processing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != accounting.StateRejected || updated.ErrorCode != "processing_cancelled" || updated.ErrorMessage != "已手动中断解析，请重新解析" {
		t.Fatalf("manual interrupt did not retain cancellation state: %+v", updated)
	}
	unchanged, err := db.Get(context.Background(), other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.State != accounting.StateQueued {
		t.Fatalf("interrupt changed another queued entry: %+v", unchanged)
	}

	secondInterrupt := httptest.NewRequest(http.MethodPost, "/api/v1/entries/"+strconv.FormatInt(processing.ID, 10)+"/interrupt", nil)
	secondResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(secondResponse, secondInterrupt)
	if secondResponse.Code != http.StatusConflict {
		t.Fatalf("interrupt endpoint accepted a non-processing entry: %d body=%s", secondResponse.Code, secondResponse.Body.String())
	}
}

func TestReparseRejectsInputAlreadyUsedByAnotherEntry(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServer(db, slog.Default(), config.Config{Sui: config.SuiConfig{BookName: "自己账本"}})
	target, _, err := db.CreateEntry(context.Background(), "web", "reparse-duplicate-target", "text", "早餐 20 元", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	existing, _, err := db.CreateEntry(context.Background(), "web", "reparse-duplicate-existing", "text", "午饭 35 元", `{}`)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/entries/"+strconv.FormatInt(target.ID, 10)+"/reparse", strings.NewReader(`{"text":"午饭 35 元"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"duplicate_entry"`) || !strings.Contains(response.Body.String(), `"entryId":`+strconv.FormatInt(existing.ID, 10)) {
		t.Fatalf("unexpected duplicate reparse response: %d body=%s", response.Code, response.Body.String())
	}
	updated, err := db.Get(context.Background(), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.InputText != "早餐 20 元" {
		t.Fatalf("duplicate reparse changed target entry: %+v", updated)
	}
}

func TestIngestRequiresLedgerMetadataInsteadOfUsingLocalParser(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServer(db, slog.Default(), config.Config{Sui: config.SuiConfig{BookName: "自己账本"}})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(`{"text":"今天午饭 35 元"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("expected queued response, got %d body=%s", response.Code, response.Body.String())
	}
	entries, err := db.List(context.Background(), "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("queued ingest did not create one entry: %+v", entries)
	}
	entry := waitForEntryState(t, db, entries[0].ID, accounting.StateRejected)
	if entry.ErrorCode != "category_metadata_not_synced" {
		t.Fatalf("background metadata failure was not retained: %+v", entry)
	}
}

func TestBackgroundAIErrorRetainsSpecificProviderDiagnostic(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServer(db, slog.Default(), config.Config{
		Sui:        config.SuiConfig{BookName: "自己账本"},
		OpenRouter: config.OpenRouterConfig{APIKey: "test-api-key", Model: "missing/model"},
	})
	metadata := sui.Metadata{}
	metadata.CurrentBook = sui.Book{ID: "1", Name: "自己账本", Current: true}
	metadata.Categories.Expense = []sui.Item{{ID: "101", Name: "食品", Type: "expense", Level: 2, Leaf: true}}
	metadata.Accounts = []sui.Item{{ID: "200", Name: "银行卡", Type: "account"}}
	payload, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMetadata(context.Background(), "1", string(payload)); err != nil {
		t.Fatal(err)
	}
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"error":{"code":401,"message":"invalid model credentials"}}`
		return &http.Response{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(`{"text":"午饭 35 元"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("expected queued response, got %d body=%s", response.Code, response.Body.String())
	}
	entries, err := db.List(context.Background(), "", 50)
	if err != nil || len(entries) != 1 {
		t.Fatalf("unexpected entries: %+v err=%v", entries, err)
	}
	entry := waitForEntryState(t, db, entries[0].ID, accounting.StateRejected)
	if entry.ErrorCode != "llm_parse_failed" {
		t.Fatalf("unexpected parse failure code: %+v", entry)
	}
	if !strings.Contains(entry.ErrorMessage, "HTTP 401") || !strings.Contains(entry.ErrorMessage, "invalid model credentials") {
		t.Fatalf("specific provider diagnostic was not retained: %q", entry.ErrorMessage)
	}
	if strings.Contains(entry.ErrorMessage, "AI 解析失败，请检查 OpenRouter 配置、模型和网络后重试") {
		t.Fatalf("generic provider diagnostic was retained: %q", entry.ErrorMessage)
	}
}

func TestReparseEntryUsesAIAndReturnsToPendingSubmission(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServerWithConfigPathAndDebug(db, slog.Default(), config.Config{
		Sui:        config.SuiConfig{BookName: "自己账本"},
		OpenRouter: config.OpenRouterConfig{APIKey: "test-api-key", Model: "openai/gpt-4o-mini"},
	}, "", true)
	metadata := sui.Metadata{}
	metadata.CurrentBook = sui.Book{ID: "1", Name: "自己账本", Current: true}
	metadata.Categories.Expense = []sui.Item{{ID: "101", Name: "食品", Type: "expense", Level: 2, Leaf: true}}
	metadata.Accounts = []sui.Item{{ID: "200", Name: "银行卡", Type: "account"}}
	metadata.Stores = []sui.Item{{ID: "300", Name: "餐馆", Type: "store"}}
	payload, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMetadata(context.Background(), "1", string(payload)); err != nil {
		t.Fatal(err)
	}
	initialDraft, err := accounting.EncodeDraft(accounting.Draft{Type: "expense", Date: "2026-09-10 12:00", Amount: "35", CategoryName: "食品", AccountName: "银行卡", NeedsConfirm: true})
	if err != nil {
		t.Fatal(err)
	}
	entry, _, err := db.CreateEntry(context.Background(), "web", "reparse-1", "text", "午饭 35 元", initialDraft)
	if err != nil {
		t.Fatal(err)
	}
	entry, err = db.SetState(context.Background(), entry.ID, accounting.StateNeedsConfirmation, accounting.StateRejected, "old_parse", "旧解析结果")
	if err != nil {
		t.Fatal(err)
	}
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"choices":[{"message":{"content":"{\"type\":\"expense\",\"date\":\"2026-09-10 12:00\",\"amount\":\"35\",\"categoryName\":\"食品\",\"accountName\":\"银行卡\",\"storeName\":\"餐馆\",\"projectName\":\"\",\"memberName\":\"\",\"memo\":\"重新解析\",\"confidence\":0.9,\"needsConfirmation\":false}"}}]}`
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/entries/"+strconv.FormatInt(entry.ID, 10)+"/reparse", strings.NewReader(`{"text":"改成今天12:00午饭 36 元"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("unexpected reparse status: %d body=%s", response.Code, response.Body.String())
	}
	updated := waitForEntryState(t, db, entry.ID, accounting.StatePendingSubmission)
	draft, err := accounting.DecodeDraft(updated.DraftJSON)
	if err != nil || draft.Memo != "重新解析" || draft.StoreName != "餐馆" || updated.InputText != "改成今天12:00午饭 36 元" {
		t.Fatalf("reparse did not replace draft: %+v err=%v", draft, err)
	}
}

func TestIngestBackgroundProcessingAutoSubmitsCompleteDraft(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runtimeConfig := config.Config{Sui: withTestSession(t, config.SuiConfig{BaseURL: "https://sui.example", BookName: "自己账本"}), OpenRouter: config.OpenRouterConfig{APIKey: "test-api-key", Model: "openai/gpt-4o-mini"}}
	server := NewServer(db, slog.Default(), runtimeConfig)
	metadata := sui.Metadata{}
	metadata.CurrentBook = sui.Book{ID: "1", Name: "自己账本", Current: true}
	metadata.Categories.Expense = []sui.Item{{ID: "101", Name: "食品", Type: "expense", Level: 2, Leaf: true}}
	metadata.Accounts = []sui.Item{{ID: "201", Name: "银行卡", Type: "account", Leaf: true}}
	payload, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMetadata(context.Background(), "1", string(payload)); err != nil {
		t.Fatal(err)
	}
	var parseCalls, writeCalls int
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/api/v1/chat/completions":
			parseCalls++
			body = `{"choices":[{"message":{"content":"{\"type\":\"expense\",\"date\":\"2026-09-10 12:00\",\"amount\":\"35\",\"categoryName\":\"食品\",\"accountName\":\"银行卡\",\"storeName\":\"\",\"projectName\":\"\",\"memberName\":\"\",\"memo\":\"午饭\",\"confidence\":0.95,\"needsConfirmation\":false}"}}]}`
		case "/report_index.do":
			body = `<li data-bookId="1" class="current">自己账本</li>`
		case "/tally/payout.rmi":
			writeCalls++
			body = `{id:{id:21693739113481},budget:0,price:35}`
		default:
			t.Fatalf("unexpected background processing request: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(`{"text":"今天12:00午饭 35 元"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"state":"queued"`) {
		t.Fatalf("ingest did not return queued immediately: %d body=%s", response.Code, response.Body.String())
	}
	entries, err := db.List(context.Background(), "", 50)
	if err != nil || len(entries) != 1 {
		t.Fatalf("unexpected queued entries: %+v err=%v", entries, err)
	}
	updated := waitForEntryState(t, db, entries[0].ID, accounting.StateCommitted)
	if parseCalls != 1 || writeCalls != 1 || updated.ErrorCode != "" || updated.ErrorMessage != "" {
		t.Fatalf("complete background entry was not auto-submitted: parse=%d write=%d entry=%+v", parseCalls, writeCalls, updated)
	}
}

func TestBackgroundProcessingQueuesEntriesInSubmissionOrder(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runtimeConfig := config.Config{
		Sui:        config.SuiConfig{BookName: "自己账本"},
		OpenRouter: config.OpenRouterConfig{APIKey: "test-api-key", Model: "openai/gpt-4o-mini"},
	}
	server := NewServerWithConfigPathAndDebug(db, slog.Default(), runtimeConfig, "", true)
	metadata := sui.Metadata{}
	metadata.CurrentBook = sui.Book{ID: "1", Name: "自己账本", Current: true}
	metadata.Categories.Expense = []sui.Item{{ID: "101", Name: "食品", Type: "expense", Level: 2, Leaf: true}}
	metadata.Accounts = []sui.Item{{ID: "201", Name: "银行卡", Type: "account", Leaf: true}}
	payload, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMetadata(context.Background(), "1", string(payload)); err != nil {
		t.Fatal(err)
	}
	first, _, err := db.CreateEntryWithState(context.Background(), "web", "queue-first", "text", "first lunch at 12:00 35 yuan", accounting.StateQueued, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := db.CreateEntryWithState(context.Background(), "web", "queue-second", "text", "second dinner at 13:00 50 yuan", accounting.StateQueued, `{}`)
	if err != nil {
		t.Fatal(err)
	}

	firstParseStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	activeParses := 0
	maxActiveParses := 0
	var parseOrder []string
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/v1/chat/completions" {
			t.Fatalf("unexpected queued processing request: %s", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		label := "unknown"
		if strings.Contains(string(body), "first lunch at 12:00") {
			label = "first"
		} else if strings.Contains(string(body), "second dinner at 13:00") {
			label = "second"
		}
		mu.Lock()
		activeParses++
		if activeParses > maxActiveParses {
			maxActiveParses = activeParses
		}
		parseOrder = append(parseOrder, label)
		mu.Unlock()
		if label == "first" {
			close(firstParseStarted)
			<-releaseFirst
		}
		mu.Lock()
		activeParses--
		mu.Unlock()
		responseBody := `{"choices":[{"message":{"content":"{\"type\":\"expense\",\"date\":\"2026-09-10 12:00\",\"amount\":\"35\",\"categoryName\":\"食品\",\"accountName\":\"银行卡\",\"storeName\":\"\",\"projectName\":\"\",\"memberName\":\"\",\"memo\":\"午饭\",\"confidence\":0.95,\"needsConfirmation\":false}"}}]}`
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(responseBody)), Header: make(http.Header), Request: r}, nil
	})}

	server.enqueueProcessing(first.ID)
	server.enqueueProcessing(second.ID)
	select {
	case <-firstParseStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first queued parse did not start")
	}
	firstProcessing := waitForEntryState(t, db, first.ID, accounting.StateProcessing)
	secondQueued, err := db.Get(context.Background(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstProcessing.State != accounting.StateProcessing || secondQueued.State != accounting.StateQueued {
		t.Fatalf("queued entries were not processed one at a time: first=%+v second=%+v", firstProcessing, secondQueued)
	}

	close(releaseFirst)
	waitForEntryState(t, db, first.ID, accounting.StatePendingSubmission)
	waitForEntryState(t, db, second.ID, accounting.StatePendingSubmission)
	mu.Lock()
	defer mu.Unlock()
	if maxActiveParses != 1 {
		t.Fatalf("queued parses overlapped: max concurrent parses=%d", maxActiveParses)
	}
	if len(parseOrder) != 2 || parseOrder[0] != "first" || parseOrder[1] != "second" {
		t.Fatalf("queued parses ran out of order: %v", parseOrder)
	}
}

func TestBackgroundSubmitFailureKeepsParsedDraft(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runtimeConfig := config.Config{Sui: withTestSession(t, config.SuiConfig{BaseURL: "https://sui.example", BookName: "自己账本"}), OpenRouter: config.OpenRouterConfig{APIKey: "test-api-key", Model: "openai/gpt-4o-mini"}}
	server := NewServer(db, slog.Default(), runtimeConfig)
	metadata := sui.Metadata{}
	metadata.CurrentBook = sui.Book{ID: "1", Name: "自己账本", Current: true}
	metadata.Categories.Expense = []sui.Item{{ID: "101", Name: "食品", Type: "expense", Level: 2, Leaf: true}}
	metadata.Accounts = []sui.Item{{ID: "201", Name: "银行卡", Type: "account", Leaf: true}}
	payload, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMetadata(context.Background(), "1", string(payload)); err != nil {
		t.Fatal(err)
	}
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/api/v1/chat/completions":
			body = `{"choices":[{"message":{"content":"{\"type\":\"expense\",\"date\":\"2026-09-10 12:00\",\"amount\":\"35\",\"categoryName\":\"食品\",\"accountName\":\"银行卡\",\"storeName\":\"\",\"projectName\":\"\",\"memberName\":\"\",\"memo\":\"午饭\",\"confidence\":0.95,\"needsConfirmation\":false}"}}]}`
		case "/report_index.do":
			body = `<li data-bookId="1" class="current">自己账本</li>`
		case "/tally/payout.rmi":
			body = `{"id":-999,"errorInfo":"分类错误"}`
		default:
			t.Fatalf("unexpected submit-failure request: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(`{"text":"今天12:00午饭 35 元"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("ingest did not return queued: %d body=%s", response.Code, response.Body.String())
	}
	entries, err := db.List(context.Background(), "", 50)
	if err != nil || len(entries) != 1 {
		t.Fatalf("unexpected entries: %+v err=%v", entries, err)
	}
	updated := waitForEntryState(t, db, entries[0].ID, accounting.StateRejected)
	if updated.ErrorCode != "sui_rejected" {
		t.Fatalf("unexpected submit failure state: %+v", updated)
	}
	draft, err := accounting.DecodeDraft(updated.DraftJSON)
	if err != nil || draft.Amount != "35" || draft.CategoryName != "食品" || draft.AccountName != "银行卡" {
		t.Fatalf("parsed draft was lost after submit failure: draft=%+v raw=%q err=%v", draft, updated.DraftJSON, err)
	}
}

func TestOpenRouterPromptPreviewUsesCurrentLedgerMetadata(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServer(db, slog.Default(), config.Config{Sui: config.SuiConfig{BookName: "自己账本"}, OpenRouter: config.OpenRouterConfig{SupplementPrompt: "用户偏好不可出现在基础 prompt"}})
	metadata := sui.Metadata{}
	metadata.CurrentBook = sui.Book{ID: "1", Name: "自己账本", Current: true}
	metadata.Categories.Expense = []sui.Item{{ID: "101", Name: "食品", Type: "expense", Level: 2, Leaf: true}}
	payload, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMetadata(context.Background(), "1", string(payload)); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/openrouter/prompt", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "支出分类:[食品]") || !strings.Contains(response.Body.String(), "只输出符合JSON Schema的JSON") || strings.Contains(response.Body.String(), "用户偏好不可出现在基础 prompt") {
		t.Fatalf("unexpected prompt preview: %d body=%s", response.Code, response.Body.String())
	}
}

func TestSubmitPreviewIsDebugOnlyAndUsesResolvedIDs(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runtimeConfig := config.Config{Sui: config.SuiConfig{BaseURL: "https://sui.example", BookName: "自己账本"}}
	metadata := sui.Metadata{}
	metadata.CurrentBook = sui.Book{ID: "1", Name: "自己账本", Current: true}
	metadata.Categories.Expense = []sui.Item{{ID: "101", Name: "食品零食", ParentName: "生活消费", Level: 2, Leaf: true}}
	metadata.Accounts = []sui.Item{{ID: "201", Name: "总账", Type: "account", Leaf: true}}
	metadata.Stores = []sui.Item{{ID: "301", Name: "外卖", Type: "store", Leaf: true}}
	payload, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMetadata(context.Background(), "1", string(payload)); err != nil {
		t.Fatal(err)
	}
	draft, err := accounting.EncodeDraft(accounting.Draft{Type: "expense", Date: "2026-09-10 12:00", Amount: "79.1", CategoryName: "生活消费 / 食品零食", AccountName: "总账", StoreName: "外卖", Memo: "面"})
	if err != nil {
		t.Fatal(err)
	}
	entry, _, err := db.CreateEntry(context.Background(), "web", "submit-preview-1", "text", "外卖 面 79.1", draft)
	if err != nil {
		t.Fatal(err)
	}
	debugServer := NewServerWithConfigPathAndDebug(db, slog.Default(), runtimeConfig, "", true)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/entries/"+strconv.FormatInt(entry.ID, 10)+"/submit-preview", nil)
	response := httptest.NewRecorder()
	debugServer.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"method":"POST"`) || !strings.Contains(response.Body.String(), "category=101") || !strings.Contains(response.Body.String(), "account=201") || !strings.Contains(response.Body.String(), "store=301") {
		t.Fatalf("unexpected debug submit preview: %d body=%s", response.Code, response.Body.String())
	}
	normalServer := NewServer(db, slog.Default(), runtimeConfig)
	response = httptest.NewRecorder()
	normalServer.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("submit preview was exposed without debug: %d body=%s", response.Code, response.Body.String())
	}
}

func TestExecuteSubmitPreviewReturnsUpstreamFailureBody(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runtimeConfig := config.Config{Sui: withTestSession(t, config.SuiConfig{BaseURL: "https://sui.example", BookName: "自己账本"})}
	metadata := sui.Metadata{}
	metadata.CurrentBook = sui.Book{ID: "1", Name: "自己账本", Current: true}
	metadata.Categories.Expense = []sui.Item{{ID: "101", Name: "食品零食", ParentName: "生活消费", Level: 2, Leaf: true}}
	metadata.Accounts = []sui.Item{{ID: "201", Name: "总账", Type: "account", Leaf: true}}
	payload, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMetadata(context.Background(), "1", string(payload)); err != nil {
		t.Fatal(err)
	}
	draft, err := accounting.EncodeDraft(accounting.Draft{Type: "expense", Date: "2026-09-10 12:00", Amount: "79.1", CategoryName: "生活消费 / 食品零食", AccountName: "总账"})
	if err != nil {
		t.Fatal(err)
	}
	entry, _, err := db.CreateEntry(context.Background(), "web", "submit-execute-1", "text", "午饭 79.1", draft)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServerWithConfigPathAndDebug(db, slog.Default(), runtimeConfig, "", true)
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/tally/payout.rmi" {
			t.Fatalf("unexpected debug submit request: %s %s", r.Method, r.URL.String())
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "category=101") || !strings.Contains(string(body), "account=201") {
			t.Fatalf("debug submit did not send edited payload: %s", body)
		}
		return &http.Response{StatusCode: http.StatusBadRequest, Status: "400 Bad Request", Body: io.NopCloser(strings.NewReader(`{"id":-999,"errorInfo":"参数错误"}`)), Header: make(http.Header), Request: r}, nil
	})}
	preview := sui.RequestPreview{Method: http.MethodPost, URL: "https://sui.example/tally/payout.rmi", Headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded; charset=UTF-8"}, Payload: "category=101&account=201&price=79.1"}
	body, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/entries/"+strconv.FormatInt(entry.ID, 10)+"/submit-preview/execute", strings.NewReader(string(body)))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"statusCode":400`) || !strings.Contains(response.Body.String(), "参数错误") {
		t.Fatalf("upstream failure body was not returned: %d body=%s", response.Code, response.Body.String())
	}
}

func TestConfirmWritesNestedSuccessAndMarksEntryCommitted(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runtimeConfig := config.Config{Sui: withTestSession(t, config.SuiConfig{BaseURL: "https://sui.example", BookName: "自己账本"})}
	metadata := sui.Metadata{}
	metadata.CurrentBook = sui.Book{ID: "1", Name: "自己账本", Current: true}
	metadata.Categories.Expense = []sui.Item{{ID: "101", Name: "食品零食", ParentName: "生活消费", Level: 2, Leaf: true}}
	metadata.Accounts = []sui.Item{{ID: "201", Name: "总账", Type: "account", Leaf: true}}
	payload, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMetadata(context.Background(), "1", string(payload)); err != nil {
		t.Fatal(err)
	}
	draft, err := accounting.EncodeDraft(accounting.Draft{Type: "expense", Date: "2026-09-10 12:00", Amount: "1234.5", CategoryName: "生活消费 / 食品零食", AccountName: "总账"})
	if err != nil {
		t.Fatal(err)
	}
	entry, _, err := db.CreateEntryWithState(context.Background(), "web", "confirm-success-1", "text", "午饭 1234.5", accounting.StatePendingSubmission, draft)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(db, slog.Default(), runtimeConfig)
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `<li data-bookId="1" class="current">自己账本</li>`
		if r.URL.Path == "/tally/payout.rmi" {
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if _, ok := r.PostForm["debt"]; ok {
				t.Fatalf("confirm payload contains debt: %q", r.PostForm.Encode())
			}
			if r.PostForm.Get("category") != "101" || r.PostForm.Get("account") != "201" || r.PostForm.Get("price") != "1234.5" {
				t.Fatalf("unexpected confirm payload: %q", r.PostForm.Encode())
			}
			body = `{"id":{"id":21693739113481},"budget":0,"price":1234.5}`
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/entries/"+strconv.FormatInt(entry.ID, 10)+"/confirm", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"state":"committed"`) || !strings.Contains(response.Body.String(), `"id":"21693739113481"`) || !strings.Contains(response.Body.String(), `"price":"1234.5"`) {
		t.Fatalf("unexpected committed response: %d body=%s", response.Code, response.Body.String())
	}
	updated, err := db.Get(context.Background(), entry.ID)
	if err != nil || updated.State != accounting.StateCommitted {
		t.Fatalf("entry was not committed: %+v err=%v", updated, err)
	}
}

func TestReconcileUncertainEntryMarksUniqueMatchingTransactionCommitted(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runtimeConfig := config.Config{Sui: withTestSession(t, config.SuiConfig{BaseURL: "https://sui.example", BookName: "自己账本"})}
	metadata := sui.Metadata{}
	metadata.CurrentBook = sui.Book{ID: "1", Name: "自己账本", Current: true}
	metadata.Categories.Expense = []sui.Item{{ID: "101", Name: "食品零食", ParentName: "生活消费", Level: 2, Leaf: true}}
	metadata.Accounts = []sui.Item{{ID: "201", Name: "总账", Type: "account", Leaf: true}}
	payload, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMetadata(context.Background(), "1", string(payload)); err != nil {
		t.Fatal(err)
	}
	draft, err := accounting.EncodeDraft(accounting.Draft{Type: "expense", Date: "2026-09-10 12:00", Amount: "1234.5", CategoryName: "生活消费 / 食品零食", AccountName: "总账", Memo: "午饭"})
	if err != nil {
		t.Fatal(err)
	}
	entry, _, err := db.CreateEntryWithState(context.Background(), "web", "reconcile-1", "text", "午饭 1234.5", accounting.StateUncertain, draft)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(db, slog.Default(), runtimeConfig)
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/tally/new.rmi" && r.URL.Path != "/report_index.do" {
			t.Fatalf("reconcile performed unexpected request: %s", r.URL.Path)
		}
		body := `{"groups":[{"list":[{"tranId":21693739113481,"tranType":"1","tranName":"支出","itemAmount":"1234.50","categoryId":101,"buyerAcountId":201,"memo":"午饭","date":"{\"date\":10,\"hours\":12,\"minutes\":0,\"month\":8,\"year\":126}"}]}]}`
		if r.URL.Path == "/report_index.do" {
			body = `<li title='自己账本' data-bookId="1" class='current'>自己账本</li>`
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/entries/"+strconv.FormatInt(entry.ID, 10)+"/reconcile", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"committed"`) || !strings.Contains(response.Body.String(), `"tranId":"21693739113481"`) {
		t.Fatalf("unexpected reconcile response: %d body=%s", response.Code, response.Body.String())
	}
	updated, err := db.Get(context.Background(), entry.ID)
	if err != nil || updated.State != accounting.StateCommitted || updated.ErrorCode != "" || updated.ErrorMessage != "" {
		t.Fatalf("entry was not committed after reconciliation: %+v err=%v", updated, err)
	}
}

func TestBatchReconcileEndpointReturnsCheckSummary(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServer(db, slog.Default(), config.Config{})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/entries/reconcile-uncertain", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"checked"`) || !strings.Contains(response.Body.String(), `"checked":0`) {
		t.Fatalf("unexpected batch reconcile response: %d body=%s", response.Code, response.Body.String())
	}
}

func TestNormalizeDraftCanonicalizesLedgerNamesAndRequiredConfirmation(t *testing.T) {
	draft := normalizeDraft(accounting.Draft{
		Type:         "expense",
		CategoryName: "生活消费 / 食品零食",
		StoreName:    "不存在的商家",
		Memo:         "西域阿婆鸡汤手擀面",
	}, "西域阿婆鸡汤手擀面", llm.ParseContext{
		ExpenseCategories: []string{"生活消费 / 食品零食"},
		Stores:            []string{"外卖"},
	}, sui.Metadata{CurrentBook: sui.Book{Name: "自己账本"}, Categories: struct {
		Expense []sui.Item `json:"expense"`
		Income  []sui.Item `json:"income"`
	}{Expense: []sui.Item{{ID: "101", Name: "食品零食", ParentName: "生活消费", Level: 2, Leaf: true}}}})
	if draft.CategoryName != "生活消费 / 食品零食" {
		t.Fatalf("category path was not preserved: %+v", draft)
	}
	if draft.StoreName != "" || draft.Memo != "西域阿婆鸡汤手擀面" {
		t.Fatalf("unknown store or memo was not normalized: %+v", draft)
	}
	if !draft.NeedsConfirm {
		t.Fatalf("missing account did not require confirmation: %+v", draft)
	}
}

func TestNormalizeDraftAcceptsModelTimeForFuzzyInput(t *testing.T) {
	metadata := sui.Metadata{CurrentBook: sui.Book{Name: "自己账本"}, Categories: struct {
		Expense []sui.Item `json:"expense"`
		Income  []sui.Item `json:"income"`
	}{Expense: []sui.Item{{ID: "101", Name: "电子产品", ParentName: "生活消费", Level: 2, Leaf: true}}}}
	contextData := llm.ParseContext{Accounts: []string{"账户"}}
	baseDraft := accounting.Draft{
		Type:         "expense",
		Date:         "2026-09-09 15:00",
		Amount:       "9.84",
		CategoryName: "生活消费 / 电子产品",
		AccountName:  "账户",
	}
	for _, input := range []string{"昨天", "昨天下午", "2026年9月9日"} {
		t.Run(input, func(t *testing.T) {
			draft := normalizeDraft(baseDraft, input, contextData, metadata)
			if draft.NeedsConfirm {
				t.Fatalf("model-provided date with hour and minute incorrectly required confirmation: %+v", draft)
			}
		})
	}
}

func TestNormalizeDraftInfersMemberAndDiningStoreFromInput(t *testing.T) {
	draft := normalizeDraft(accounting.Draft{
		Type:         "expense",
		CategoryName: "生活消费 / 食品零食",
		AccountName:  "总账",
	}, "昨天早晨9:51贝贝在医院食堂吃了早饭，花了¥3", llm.ParseContext{
		Accounts: []string{"总账"},
		Stores:   []string{"外卖", "堂食"},
		Members:  []string{"本人", "贝贝"},
	}, sui.Metadata{CurrentBook: sui.Book{Name: "自己账本"}, Categories: struct {
		Expense []sui.Item `json:"expense"`
		Income  []sui.Item `json:"income"`
	}{Expense: []sui.Item{{ID: "101", Name: "食品零食", ParentName: "生活消费", Level: 2, Leaf: true}}}})
	if draft.StoreName != "堂食" || draft.MemberName != "贝贝" {
		t.Fatalf("deterministic dining/member inference failed: %+v", draft)
	}
}

func TestNormalizeDraftMapsFirstPersonToSelfAndPreservesExplicitTakeaway(t *testing.T) {
	draft := normalizeDraft(accounting.Draft{
		Type:        "expense",
		StoreName:   "",
		MemberName:  "",
		AccountName: "总账",
	}, "我点了外卖", llm.ParseContext{
		Accounts: []string{"总账"},
		Stores:   []string{"外卖", "堂食"},
		Members:  []string{"本人", "贝贝"},
	}, sui.Metadata{})
	if draft.StoreName != "外卖" || draft.MemberName != "本人" {
		t.Fatalf("first-person/takeaway inference failed: %+v", draft)
	}
}

func TestNormalizeDraftDoesNotAssumeSelf(t *testing.T) {
	for _, input := range []string{"今天中午在小巴依吃了50块钱", "今天买书50元", "我们今天吃饭50元"} {
		t.Run(input, func(t *testing.T) {
			draft := normalizeDraft(accounting.Draft{MemberName: "本人", StoreName: "堂食", Memo: "小巴依"}, input,
				llm.ParseContext{Members: []string{"本人", "贝贝"}, Stores: []string{"堂食", "外卖"}}, sui.Metadata{})
			if draft.MemberName != "" || draft.Memo != "小巴依" {
				t.Fatalf("implicit self or lost venue: %+v", draft)
			}
		})
	}
}

func TestNormalizeDraftPreservesAIMemo(t *testing.T) {
	draft := normalizeDraft(accounting.Draft{
		Type:         "expense",
		CategoryName: "生活消费 / 电子产品",
		AccountName:  "账户",
		StoreName:    "拼多多",
		MemberName:   "贝贝",
		Memo:         "绿联iPhone 17 Pro钢化膜两个",
	}, "今天15:00贝贝在拼多多购买绿联iPhone 17 Pro钢化膜两个100元", llm.ParseContext{
		Accounts: []string{"账户"},
		Stores:   []string{"拼多多"},
		Members:  []string{"贝贝"},
	}, sui.Metadata{CurrentBook: sui.Book{Name: "自己账本"}, Categories: struct {
		Expense []sui.Item `json:"expense"`
		Income  []sui.Item `json:"income"`
	}{Expense: []sui.Item{{ID: "101", Name: "电子产品", ParentName: "生活消费", Level: 2, Leaf: true}}}})
	if draft.Memo != "绿联iPhone 17 Pro钢化膜两个" {
		t.Fatalf("AI memo was altered: %q", draft.Memo)
	}
}

func TestNormalizeDraftExtractsExplicitCurrencyAmount(t *testing.T) {
	input := "贝贝下午在拼多多上买了两个绿联的iPhone 17 Pro钢化膜花了¥99.96"
	draft := normalizeDraft(accounting.Draft{
		Type:         "expense",
		Date:         "2026-09-10 15:00",
		CategoryName: "生活消费 / 电子产品",
		AccountName:  "账户",
		StoreName:    "拼多多",
		MemberName:   "贝贝",
	}, input, llm.ParseContext{
		Accounts: []string{"账户"},
		Stores:   []string{"拼多多"},
		Members:  []string{"贝贝"},
	}, sui.Metadata{CurrentBook: sui.Book{Name: "自己账本"}, Categories: struct {
		Expense []sui.Item `json:"expense"`
		Income  []sui.Item `json:"income"`
	}{Expense: []sui.Item{{ID: "101", Name: "电子产品", ParentName: "生活消费", Type: "expense", Level: 2, Leaf: true}}}})
	if draft.Amount != "99.96" {
		t.Fatalf("explicit amount was not extracted: %+v", draft)
	}
}

func TestEmbeddedAssetRoute(t *testing.T) {
	handler := staticAssetHandler(fstest.MapFS{
		"index-qswAcT9u.js": &fstest.MapFile{Data: []byte("asset-content")},
	})
	request := httptest.NewRequest(http.MethodGet, "/assets/placeholder.txt", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unexpected missing asset status: %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/assets/index-qswAcT9u.js", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected asset status: %d body=%s", response.Code, response.Body.String())
	}
	if response.Body.String() != "asset-content" {
		t.Fatalf("unexpected asset body: %q", response.Body.String())
	}
}

func TestSettingsDoNotReturnSecretsAndEmptySecretsDoNotOverwrite(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServer(db, slog.Default(), config.Config{
		Sui:        config.SuiConfig{BaseURL: "https://sui.example", BookName: "自己账本"},
		OpenRouter: config.OpenRouterConfig{APIKey: "config-api-key", Model: "openai/gpt-4o-mini"},
	})

	request := httptest.NewRequest(http.MethodPatch, "/api/v1/settings", strings.NewReader(`{"openrouter_api_key":"new-api-key","sui_book_name":"家庭账本","openrouter_prompt":"午餐优先归入食品"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected save status: %d body=%s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); strings.Contains(body, "new-api-key") || strings.Contains(body, "book_id") {
		t.Fatalf("response exposed secret or book id: %s", body)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected get status: %d body=%s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); strings.Contains(body, "new-api-key") || strings.Contains(body, "config-api-key") || strings.Contains(body, "book_id") {
		t.Fatalf("GET exposed secret or book id: %s", body)
	}
	var payload struct {
		Settings map[string]string `json:"settings"`
		Secrets  map[string]bool   `json:"secrets"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Settings["sui_book_name"] != "家庭账本" || payload.Settings["openrouter_prompt"] != "午餐优先归入食品" || !payload.Secrets["openrouter_api_key"] {
		t.Fatalf("unexpected settings response: %+v", payload)
	}
	target, err := db.LatestTargetBook(context.Background())
	if err != nil || target.BookName != "家庭账本" || target.UpdatedAt == "" {
		t.Fatalf("target book was not cached in SQLite: %+v err=%v", target, err)
	}

	request = httptest.NewRequest(http.MethodPatch, "/api/v1/settings", strings.NewReader(`{"openrouter_api_key":""}`))
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"openrouter_api_key":true`) {
		t.Fatalf("empty secret update changed status: %d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPatch, "/api/v1/settings", strings.NewReader(`{"sui_cookie":"legacy-cookie"}`))
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_setting") {
		t.Fatalf("legacy cookie setting was accepted: %d body=%s", response.Code, response.Body.String())
	}

}

func TestCleanupRetentionSettingsAndManualCleanup(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	configPath := t.TempDir() + "/sui-copilot.toml"
	server := NewServerWithConfigPath(db, slog.Default(), config.Default(), configPath)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"cleanup_retention_days":"7"`) {
		t.Fatalf("default cleanup retention was not returned: %d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPatch, "/api/v1/settings", strings.NewReader(`{"cleanup_retention_days":"15"}`))
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"cleanup_retention_days":"15"`) {
		t.Fatalf("cleanup retention was not saved: %d body=%s", response.Code, response.Body.String())
	}
	loaded, err := config.Load(configPath)
	if err != nil || loaded.EffectiveCleanupRetentionDays() != 15 {
		t.Fatalf("cleanup retention was not persisted to TOML: %+v err=%v", loaded.Cleanup, err)
	}

	entry, _, err := db.CreateEntryWithState(context.Background(), "web", "manual-cleanup-current", "text", "当前已入账", accounting.StateCommitted, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/cleanup", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"deleted":0`) || !strings.Contains(response.Body.String(), `"retentionDays":15`) || !strings.Contains(response.Body.String(), `"vacuum":true`) {
		t.Fatalf("manual cleanup response was unexpected: %d body=%s", response.Code, response.Body.String())
	}
	if _, err := db.Get(context.Background(), entry.ID); err != nil {
		t.Fatalf("manual cleanup deleted a current entry: %v", err)
	}

	request = httptest.NewRequest(http.MethodPatch, "/api/v1/settings", strings.NewReader(`{"cleanup_retention_days":"0"}`))
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"cleanup_retention_days":"0"`) {
		t.Fatalf("permanent cleanup retention was not saved: %d body=%s", response.Code, response.Body.String())
	}
	loaded, err = config.Load(configPath)
	if err != nil || loaded.EffectiveCleanupRetentionDays() != 0 {
		t.Fatalf("permanent cleanup retention was not persisted to TOML: %+v err=%v", loaded.Cleanup, err)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/cleanup", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"deleted":0`) || !strings.Contains(response.Body.String(), `"retentionDays":0`) {
		t.Fatalf("permanent manual cleanup response was unexpected: %d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPatch, "/api/v1/settings", strings.NewReader(`{"cleanup_retention_days":"3"}`))
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "cleanup_retention_invalid") {
		t.Fatalf("invalid cleanup retention was accepted: %d body=%s", response.Code, response.Body.String())
	}
}

func TestSettingsPersistPublicValuesToConfigFile(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	configPath := t.TempDir() + "/sui-copilot.toml"
	server := NewServerWithConfigPath(db, slog.Default(), config.Default(), configPath)
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/settings", strings.NewReader(`{"sui_book_name":"家庭账本","openrouter_model":"anthropic/claude-sonnet","openrouter_prompt":"午餐优先归入食品","openrouter_api_key":"new-api-key"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected save status: %d body=%s", response.Code, response.Body.String())
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, expected := range []string{"book_name = \"家庭账本\"", "model = \"anthropic/claude-sonnet\"", "prompt = \"午餐优先归入食品\"", "api_key = \"new-api-key\""} {
		if !strings.Contains(text, expected) {
			t.Fatalf("config file does not contain %q: %s", expected, text)
		}
	}
	loaded, err := config.Load(configPath)
	if err != nil || loaded.Sui.BookName != "家庭账本" || loaded.OpenRouter.APIKey != "new-api-key" || loaded.OpenRouter.SupplementPrompt != "午餐优先归入食品" {
		t.Fatalf("saved config could not be loaded: %+v err=%v", loaded, err)
	}
	for _, forbidden := range []string{"sui_base_url", "sui_book_name", "openrouter_model", "sui_cookie", "openrouter_api_key"} {
		if value, err := db.GetSetting(context.Background(), forbidden); err != nil {
			t.Fatal(err)
		} else if value != "" {
			t.Fatalf("configuration key %q was stored in SQLite: %q", forbidden, value)
		}
	}
}

func TestSuiClientDoesNotFallbackToLegacyCookieForBrokenSessionStore(t *testing.T) {
	sessionDir := t.TempDir()
	sessionPath := sessionDir + "/sui-session.bin"
	keyPath := sessionDir + "/sui-session.key"
	if err := os.WriteFile(sessionPath, []byte("corrupted-session"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServer(db, slog.Default(), config.Config{Sui: config.SuiConfig{
		SessionFile:    sessionPath,
		SessionKeyFile: keyPath,
	}})

	client := server.suiClient()
	if client.Jar != nil || !errors.Is(client.SessionErr, sui.ErrSessionStoreCorrupted) {
		t.Fatalf("broken session store fell through to legacy cookie: jar=%v err=%v", client.Jar != nil, client.SessionErr)
	}
}

func TestOpenRouterModelsAPI(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServerWithSecretKey(db, slog.Default(), config.Default(), []byte("01234567890123456789012345678901"))
	var modelRequests int
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		modelRequests++
		if r.Header.Get("Authorization") != "Bearer test-api-key" {
			t.Fatalf("unexpected authorization header: %q", r.Header.Get("Authorization"))
		}
		body := `{"data":[{"id":"anthropic/claude-sonnet","name":"Claude Sonnet"}]}`
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/settings", strings.NewReader(`{"openrouter_api_key":"test-api-key"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected save status: %d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/openrouter/models/sync", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "anthropic/claude-sonnet") || strings.Contains(response.Body.String(), "test-api-key") {
		t.Fatalf("unexpected model sync response: %d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/openrouter/models", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "anthropic/claude-sonnet") || !strings.Contains(response.Body.String(), "cached") {
		t.Fatalf("unexpected cached model response: %d body=%s", response.Code, response.Body.String())
	}
	if modelRequests != 1 {
		t.Fatalf("cached model read called OpenRouter %d times", modelRequests)
	}
}

func TestLLMSettingsSelectProviderAndUseIsolatedModelCatalog(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServer(db, slog.Default(), config.Config{LLM: config.LLMConfig{Provider: config.ProviderOpenRouter, APIKey: "old-key", Model: "openai/gpt-4o-mini"}})
	var requestedPath string
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requestedPath = r.URL.Path
		if r.Header.Get("Authorization") != "Bearer new-key" {
			t.Fatalf("unexpected authorization header: %q", r.Header.Get("Authorization"))
		}
		body := `{"data":[{"id":"gpt-4o-mini","name":"GPT-4o mini"}]}`
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}

	request := httptest.NewRequest(http.MethodPatch, "/api/v1/settings", strings.NewReader(`{"llm_provider":"openai","llm_model":"gpt-4o-mini"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "llm_api_key_required") {
		t.Fatalf("provider switch without key was accepted: %d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPatch, "/api/v1/settings", strings.NewReader(`{"llm_provider":"openai","llm_model":"gpt-4o-mini","llm_api_key":"new-key"}`))
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "new-key") || !strings.Contains(response.Body.String(), `"llm_provider":"openai"`) {
		t.Fatalf("OpenAI settings were not saved safely: %d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/llm/models/sync", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || requestedPath != "/v1/models" || !strings.Contains(response.Body.String(), "gpt-4o-mini") {
		t.Fatalf("OpenAI model sync failed: %d path=%s body=%s", response.Code, requestedPath, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/llm/models", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "cached") {
		t.Fatalf("OpenAI model catalog was not cached: %d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPatch, "/api/v1/settings", strings.NewReader(`{"llm_provider":"custom","llm_base_url":"http://127.0.0.1:9000/v1/","llm_model":"local/model","llm_api_key":"custom-key"}`))
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"llm_provider":"custom"`) {
		t.Fatalf("custom settings were not saved: %d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/llm/models", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "not_synced") {
		t.Fatalf("provider switch reused OpenAI model catalog: %d body=%s", response.Code, response.Body.String())
	}
}

func TestConnectionCheckRejectsDuplicateBookName(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServer(db, slog.Default(), config.Config{Sui: withTestSession(t, config.SuiConfig{BaseURL: "https://sui.example", BookName: "共同账本"})})
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `<li data-bookId="1" class="current">共同账本</li><li data-bookId="2">共同账本</li>`
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/connection/check", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "book_name_ambiguous") {
		t.Fatalf("unexpected duplicate-book response: %d body=%s", response.Code, response.Body.String())
	}
}

func TestSuiBooksAPICachesNestedBookMarkupWithoutReturningIDs(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServer(db, slog.Default(), config.Config{Sui: withTestSession(t, config.SuiConfig{})})
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/report_index.do" {
			t.Fatalf("book listing fetched unexpected resource: %s", r.URL.Path)
		}
		body := `<ul class="s-accountbook-scroll" style="top: 0px">
<li title='自己账本' data-bookId="2150191014" class='current'>
<span class="b-wrap"><span class="no-char-num">自己账本</span></span>
</li>
<li title='旅游' data-bookId="26571914800" class=''>
<span class="b-wrap"><span class="no-char-num">旅游</span></span>
</li></ul>`
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sui/books/sync", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "自己账本") || !strings.Contains(response.Body.String(), "旅游") || strings.Contains(response.Body.String(), "2150191014") {
		t.Fatalf("unexpected books response: %d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/sui/books", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "自己账本") || !strings.Contains(response.Body.String(), "旅游") || !strings.Contains(response.Body.String(), "updatedAt") {
		t.Fatalf("unexpected cached books response: %d body=%s", response.Code, response.Body.String())
	}
}

func TestSyncSuiBookStoresCategoriesInSQLite(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServer(db, slog.Default(), config.Config{Sui: withTestSession(t, config.SuiConfig{BookName: "自己账本"})})
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/report_index.do":
			body = `<li title='自己账本' data-bookId="1" class='current'>自己账本</li>`
		case "/category/budgetCategory.do":
			body = `<div id="category123"><ul><li class="li-level2" title="食品">食品</li></ul></div>`
		case "/category/incomeCategory.do":
			body = `<div id="category456"><ul><li class="li-level2" title="工资">工资</li></ul></div>`
		case "/category/storeCategory.do":
			body = `<div id="category789"><span title="餐馆">餐馆</span></div>`
		case "/account/account.do":
			body = `<script>var accountNameJson = {"1":"现金账户","2":"金融账户"};</script>`
		case "/category/projectCategory.do":
			body = `<div id="category791"><span title="旅行">旅行</span></div>`
		case "/category/member.do":
			body = `<div id="category792"><span title="我">我</span></div>`
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sui/sync", strings.NewReader(`{"bookName":"自己账本"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "食品") || !strings.Contains(response.Body.String(), "工资") || !strings.Contains(response.Body.String(), "现金账户") || !strings.Contains(response.Body.String(), "金融账户") {
		t.Fatalf("unexpected sync response: %d body=%s", response.Code, response.Body.String())
	}
	snapshot, err := db.LatestMetadata(context.Background())
	if err != nil || !strings.Contains(snapshot.Payload, "食品") || !strings.Contains(snapshot.Payload, "工资") || !strings.Contains(snapshot.Payload, "现金账户") || !strings.Contains(snapshot.Payload, "金融账户") {
		t.Fatalf("categories were not stored: %+v err=%v", snapshot, err)
	}
}

func TestConfirmRejectsFirstLevelCategory(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServer(db, slog.Default(), config.Config{Sui: config.SuiConfig{BookName: "自己账本"}})
	metadata := sui.Metadata{}
	metadata.CurrentBook = sui.Book{ID: "1", Name: "自己账本", Current: true}
	metadata.Categories.Expense = []sui.Item{
		{ID: "100", Name: "日常", Type: "expense", Level: 1},
		{ID: "101", Name: "食品", Type: "expense", Level: 2, ParentID: "100", ParentName: "日常", Leaf: true},
	}
	payload, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMetadata(context.Background(), "1", string(payload)); err != nil {
		t.Fatal(err)
	}
	draft, err := accounting.EncodeDraft(accounting.Draft{Type: "expense", Date: "2026-09-10 12:00", Amount: "35", CategoryName: "日常", AccountName: "银行卡"})
	if err != nil {
		t.Fatal(err)
	}
	entry, _, err := db.CreateEntry(context.Background(), "web", "first-level-category", "text", "午饭 35 元", draft)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/entries/"+strconv.FormatInt(entry.ID, 10)+"/confirm", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "category_not_leaf") {
		t.Fatalf("expected first-level category rejection: %d body=%s", response.Code, response.Body.String())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func waitForEntryState(t *testing.T, db *store.DB, id int64, state string) store.Entry {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		entry, err := db.Get(context.Background(), id)
		if err == nil && entry.State == state {
			return entry
		}
		time.Sleep(5 * time.Millisecond)
	}
	entry, err := db.Get(context.Background(), id)
	t.Fatalf("entry did not reach state %q: entry=%+v err=%v", state, entry, err)
	return store.Entry{}
}

func withTestSession(t *testing.T, cfg config.SuiConfig) config.SuiConfig {
	t.Helper()
	dir := t.TempDir()
	cfg.SessionFile = dir + "/sui-session.bin"
	cfg.SessionKeyFile = dir + "/sui-session.key"
	session, err := sui.NewSession(cfg.SessionFile, cfg.SessionKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Save(); err != nil {
		t.Fatal(err)
	}
	return cfg
}
