package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sui-bridge/internal/accounting"
	"sui-bridge/internal/config"
	"sui-bridge/internal/store"
	"sui-bridge/internal/sui"
)

func TestSubmitResolvedEntryPersistsResultAfterCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, body, state, code string
		cancelBeforeWrite       bool
		waitForDeadline         bool
	}{
		{name: "success", body: `{"id":{"id":12345},"price":35}`, state: accounting.StateCommitted},
		{name: "uncertain", state: accounting.StateUncertain, code: "sui_uncertain"},
		{name: "deadline exceeded", waitForDeadline: true, state: accounting.StateUncertain, code: "sui_uncertain"},
		{name: "rejected", body: `{"id":-999,"errorInfo":"分类错误"}`, state: accounting.StateRejected, code: "sui_rejected"},
		{name: "book check cancelled", cancelBeforeWrite: true, state: accounting.StateRejected, code: "sui_rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := store.Open(t.TempDir() + "/test.db")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			server := NewServer(db, slog.Default(), config.Config{
				Sui: withTestSession(t, config.SuiConfig{BaseURL: "https://sui.example", BookName: "自己账本"}),
			})
			const rawDraft = `{"type":"expense","amount":"35","memo":"保留草稿"}`
			entry, _, err := db.CreateEntryWithState(context.Background(), "test", tc.name, "text", tc.name, accounting.StatePendingSubmission, rawDraft)
			if err != nil {
				t.Fatal(err)
			}
			var ctx context.Context
			var cancel context.CancelFunc
			if tc.waitForDeadline {
				ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			writes := 0
			server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := `<li data-bookId="1" class="current">自己账本</li>`
				switch r.URL.Path {
				case "/report_index.do":
					if tc.cancelBeforeWrite {
						cancel()
						return nil, r.Context().Err()
					}
				case "/tally/payout.rmi":
					writes++
					if tc.waitForDeadline {
						<-r.Context().Done()
					} else {
						cancel()
					}
					if tc.body == "" {
						return nil, r.Context().Err()
					}
					body = tc.body
				default:
					t.Fatalf("unexpected request: %s", r.URL.Path)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
			})}

			updated, result, err := server.submitResolvedEntry(ctx, entry, sui.WriteInput{Type: "expense", Amount: "35"})
			if err != nil {
				t.Fatalf("cancelled request prevented result persistence: %v", err)
			}
			stored, err := db.Get(context.Background(), entry.ID)
			if err != nil {
				t.Fatal(err)
			}
			if updated.State != tc.state || stored.State != tc.state || stored.ErrorCode != tc.code || stored.DraftJSON != rawDraft || stored.Version != entry.Version+2 {
				t.Fatalf("unexpected persisted result: returned=%+v stored=%+v", updated, stored)
			}
			wantWrites := 1
			if tc.cancelBeforeWrite {
				wantWrites = 0
			}
			if writes != wantWrites {
				t.Fatalf("write was retried or sent after failed preflight: writes=%d want=%d", writes, wantWrites)
			}
			if tc.state == accounting.StateCommitted && result.UpstreamID != "12345" {
				t.Fatalf("successful upstream result was lost: %+v", result)
			}
		})
	}
}

func TestBackgroundQueueContinuesAfterCancelledWrite(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServer(db, slog.Default(), config.Config{
		Sui:        withTestSession(t, config.SuiConfig{BaseURL: "https://sui.example", BookName: "自己账本"}),
		OpenRouter: config.OpenRouterConfig{APIKey: "test-api-key", Model: "test-model"},
	})
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
	first, _, err := db.CreateEntryWithState(context.Background(), "test", "first", "text", "第一笔午饭 35 元", accounting.StateQueued, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := db.CreateEntryWithState(context.Background(), "test", "second", "text", "第二笔晚饭 35 元", accounting.StateQueued, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var writes, parses atomic.Int32
	server.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/api/v1/chat/completions":
			parses.Add(1)
			draft, err := accounting.EncodeDraft(accounting.Draft{Type: "expense", Date: "2026-10-22 12:00", Amount: "35", CategoryName: "食品", AccountName: "银行卡"})
			if err != nil {
				return nil, err
			}
			encoded, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": draft}}}})
			if err != nil {
				return nil, err
			}
			body = string(encoded)
		case "/report_index.do":
			body = `<li data-bookId="1" class="current">自己账本</li>`
		case "/tally/payout.rmi":
			if writes.Add(1) == 1 {
				server.cancelProcessing(first.ID)
				return nil, r.Context().Err()
			}
			body = `{"id":12345}`
		default:
			return nil, errors.New("unexpected request: " + r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}

	server.enqueueProcessing(first.ID)
	server.enqueueProcessing(second.ID)
	uncertain := waitForEntryState(t, db, first.ID, accounting.StateUncertain)
	waitForEntryState(t, db, second.ID, accounting.StateCommitted)
	if uncertain.ErrorCode != "sui_uncertain" || uncertain.DraftJSON == "{}" || writes.Load() != 2 || parses.Load() != 2 {
		t.Fatalf("queue did not preserve the uncertain write and continue once: first=%+v writes=%d parses=%d", uncertain, writes.Load(), parses.Load())
	}
}
