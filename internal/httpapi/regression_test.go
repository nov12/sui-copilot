package httpapi

import (
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

	"sui-copilot/internal/accounting"
	"sui-copilot/internal/config"
	"sui-copilot/internal/llm"
	"sui-copilot/internal/store"
	"sui-copilot/internal/sui"
)

// An uncertain entry may already exist upstream, so reparse must never put it
// back on the automatic-submission path.
func TestReparseRejectsUncertainEntry(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServer(db, slog.Default(), config.Config{
		Sui: withTestSession(t, config.SuiConfig{BaseURL: "https://sui.example", BookName: "自己账本"}),
	})
	var requests atomic.Int32
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("uncertain entry must not be reparsed")
	})}
	entry, _, err := db.CreateEntryWithState(context.Background(), "web", "uncertain-reparse", "text", "午饭 35 元", accounting.StateUncertain, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/entries/"+strconv.FormatInt(entry.ID, 10)+"/reparse", strings.NewReader(`{"text":"午饭 36 元"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "invalid_state") {
		t.Fatalf("expected uncertain reparse rejection: %d body=%s", response.Code, response.Body.String())
	}
	stored, err := db.Get(context.Background(), entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != accounting.StateUncertain || stored.InputText != "午饭 35 元" {
		t.Fatalf("uncertain entry was mutated: %+v", stored)
	}
	if requests.Load() != 0 {
		t.Fatalf("upstream was contacted for an uncertain reparse: %d", requests.Load())
	}
}

// The pause switch must actually stop the queue from taking new jobs, and
// resume must let it continue, without dropping already-enqueued entries.
func TestPauseBlocksQueueUntilResumed(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Keep the parsed draft pending so this queue test does not race with
	// automatic submission or need to simulate the upstream write flow.
	server := NewServerWithConfigPathAndDebug(db, slog.Default(), config.Config{
		Sui:        withTestSession(t, config.SuiConfig{BaseURL: "https://sui.example", BookName: "自己账本"}),
		OpenRouter: config.OpenRouterConfig{APIKey: "test-api-key", Model: "test-model"},
	}, "", true)
	if err := db.SetSetting(context.Background(), "processing_paused", "true"); err != nil {
		t.Fatal(err)
	}
	metadata := sui.Metadata{}
	metadata.CurrentBook = sui.Book{ID: "1", Name: "自己账本", Current: true}
	metadata.Categories.Expense = []sui.Item{{ID: "101", Name: "食品", Level: 2, Leaf: true}}
	metadata.Accounts = []sui.Item{{ID: "201", Name: "银行卡"}}
	payload, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMetadata(context.Background(), "1", string(payload)); err != nil {
		t.Fatal(err)
	}
	var parses atomic.Int32
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/chat/completions":
			parses.Add(1)
			return okJSONResponse(r, `{"choices":[{"message":{"content":"{\"type\":\"expense\",\"date\":\"2026-10-30 12:00\",\"amount\":\"35\",\"categoryName\":\"食品\",\"accountName\":\"银行卡\",\"memo\":\"午饭\",\"confidence\":0.9,\"needsConfirmation\":false}"}}]}`)
		default:
			return nil, errors.New("unexpected request: " + r.URL.Path)
		}
	})}
	entry, _, err := db.CreateEntryWithState(context.Background(), "web", "paused-1", "text", "午饭 35 元", accounting.StateQueued, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	server.enqueueProcessing(entry.ID)

	deadline := time.Now().Add(3 * processingPausePollInterval)
	for time.Now().Before(deadline) {
		if parses.Load() != 0 {
			t.Fatalf("paused queue started parsing: parses=%d", parses.Load())
		}
		time.Sleep(processingPausePollInterval / 2)
	}
	stored, err := db.Get(context.Background(), entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != accounting.StateQueued {
		t.Fatalf("paused entry left the queued state: %+v", stored)
	}

	if err := db.SetSetting(context.Background(), "processing_paused", "false"); err != nil {
		t.Fatal(err)
	}
	waitForEntryState(t, db, entry.ID, accounting.StatePendingSubmission)
	if parses.Load() != 1 {
		t.Fatalf("resumed queue parsed %d times, want 1", parses.Load())
	}
}

// Non-amount quantities and fragmented colloquial amounts must not override
// the model's amount; a single explicit amount still must.
func TestExtractExplicitAmountGuards(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "currency marker wins", input: "买了钢化膜花了¥99.96", expected: "99.96"},
		{name: "duration is not an amount", input: "打车花了3小时", expected: ""},
		{name: "count is not an amount", input: "买了两个绿联钢化膜用了3个", expected: ""},
		{name: "days is not an amount", input: "手机用了3天", expected: ""},
		{name: "colloquial fragment is not an amount", input: "午饭吃了4块5", expected: ""},
		{name: "two amounts are ambiguous", input: "门票35元 3天花了99元", expected: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractExplicitAmount(tc.input); got != tc.expected {
				t.Fatalf("extractExplicitAmount(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

// normalizeDraft must keep the model amount when the local extraction is
// guarded off, and override it when a single explicit amount is present.
func TestNormalizeDraftAmountFallbackGuards(t *testing.T) {
	metadata := sui.Metadata{CurrentBook: sui.Book{Name: "自己账本"}}
	contextData := llmParseContext()

	draft := normalizeDraft(accounting.Draft{Type: "expense", Date: "2026-10-30 12:00", Amount: "68", CategoryName: "", AccountName: "账户"}, "打车花了3小时", contextData, metadata)
	if draft.Amount != "68" {
		t.Fatalf("duration quantity overrode the model amount: %q", draft.Amount)
	}
	draft = normalizeDraft(accounting.Draft{Type: "expense", Date: "2026-10-30 12:00", Amount: "68", CategoryName: "", AccountName: "账户"}, "午饭 ¥35", contextData, metadata)
	if draft.Amount != "35" {
		t.Fatalf("explicit currency amount was not applied: %q", draft.Amount)
	}
}

func okJSONResponse(r *http.Request, body string) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
}

func llmParseContext() llm.ParseContext {
	return llm.ParseContext{Accounts: []string{"账户"}, Now: "2026-10-30 12:00"}
}
