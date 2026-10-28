package store

import (
	"context"
	"testing"
	"time"
)

func TestSuccessfulTransitionsClearErrorsAndKeepEvents(t *testing.T) {
	db, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, from := range []string{"committing", "uncertain"} {
		entry, _, err := db.CreateEntryWithState(ctx, "test", from, "text", from, from, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.sql.Exec(`UPDATE entries SET error_code='sui_uncertain', error_message='旧错误' WHERE id=?`, entry.ID); err != nil {
			t.Fatal(err)
		}
		updated, err := db.SetState(ctx, entry.ID, from, "committed", "sui_reconciled", "核对成功")
		if err != nil || updated.State != "committed" || updated.ErrorCode != "" || updated.ErrorMessage != "" {
			t.Fatalf("entry=%+v err=%v", updated, err)
		}
		var message string
		if err := db.sql.QueryRow(`SELECT message FROM entry_events WHERE entry_id=? ORDER BY id DESC LIMIT 1`, entry.ID).Scan(&message); err != nil || message != "核对成功" {
			t.Fatalf("event=%s err=%v", message, err)
		}
	}
}

func TestRecoverInterruptedEntriesMarksProcessingAsRejected(t *testing.T) {
	db, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	processing, _, err := db.CreateEntryWithState(ctx, "web", "processing-1", "text", "午饭 35 元", "processing", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	queued, _, err := db.CreateEntryWithState(ctx, "web", "queued-1", "text", "晚饭 50 元", "queued", `{}`)
	if err != nil {
		t.Fatal(err)
	}

	recovered, err := db.RecoverInterruptedEntries(ctx)
	if err != nil || recovered != 1 {
		t.Fatalf("recovered=%d err=%v", recovered, err)
	}
	updated, err := db.Get(ctx, processing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != "rejected" || updated.ErrorCode != "processing_interrupted" || updated.ErrorMessage != "服务重启导致解析中断，请重新解析" || updated.Version != processing.Version+1 {
		t.Fatalf("interrupted entry was not rejected correctly: %+v", updated)
	}
	var event struct {
		From    string
		To      string
		Message string
	}
	if err := db.sql.QueryRowContext(ctx, `SELECT from_state, to_state, message FROM entry_events WHERE entry_id=? ORDER BY id DESC LIMIT 1`, processing.ID).Scan(&event.From, &event.To, &event.Message); err != nil {
		t.Fatal(err)
	}
	if event.From != "processing" || event.To != "rejected" || event.Message != updated.ErrorMessage {
		t.Fatalf("unexpected recovery event: %+v", event)
	}
	unchanged, err := db.Get(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.State != "queued" {
		t.Fatalf("queued entry changed during recovery: %+v", unchanged)
	}

	recovered, err = db.RecoverInterruptedEntries(ctx)
	if err != nil || recovered != 0 {
		t.Fatalf("recovery was not idempotent: recovered=%d err=%v", recovered, err)
	}
	var eventCount int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM entry_events WHERE entry_id=?`, processing.ID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("recovery duplicated events: %d", eventCount)
	}
}

func TestRecoverInterruptedCommitRequiresReconciliation(t *testing.T) {
	db, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	const draft = `{"type":"expense","amount":"44.4","memo":"保留解析结果"}`
	entry, _, err := db.CreateEntryWithState(ctx, "test", "interrupted-commit", "text", "测试账单", "committing", draft)
	if err != nil {
		t.Fatal(err)
	}
	var unchanged []Entry
	for _, state := range []string{"queued", "pending_submission", "needs_confirmation", "uncertain", "rejected", "committed"} {
		other, _, err := db.CreateEntryWithState(ctx, "test", state, "text", state, state, draft)
		if err != nil {
			t.Fatal(err)
		}
		unchanged = append(unchanged, other)
	}
	recovered, err := db.RecoverInterruptedEntries(ctx)
	if err != nil || recovered != 1 {
		t.Fatalf("recovered=%d err=%v", recovered, err)
	}
	updated, err := db.Get(ctx, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != "uncertain" || updated.ErrorCode != "sui_commit_interrupted" || updated.ErrorMessage == "" || updated.DraftJSON != draft || updated.Version != entry.Version+1 || updated.ReconcileMisses != 0 {
		t.Fatalf("interrupted commit was not preserved for reconciliation: %+v", updated)
	}
	var from, to, message string
	if err := db.sql.QueryRowContext(ctx, `SELECT from_state, to_state, message FROM entry_events WHERE entry_id=?`, entry.ID).Scan(&from, &to, &message); err != nil {
		t.Fatal(err)
	}
	if from != "committing" || to != "uncertain" || message != updated.ErrorMessage {
		t.Fatalf("unexpected recovery event: %s -> %s: %s", from, to, message)
	}
	for _, other := range unchanged {
		stored, err := db.Get(ctx, other.ID)
		if err != nil || stored != other {
			t.Fatalf("recovery changed an unrelated entry: before=%+v after=%+v err=%v", other, stored, err)
		}
	}
	recovered, err = db.RecoverInterruptedEntries(ctx)
	if err != nil || recovered != 0 {
		t.Fatalf("recovery was not idempotent: recovered=%d err=%v", recovered, err)
	}
	var events int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM entry_events WHERE entry_id=?`, entry.ID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("recovery duplicated events: count=%d err=%v", events, err)
	}
}

func TestMigrationClearsHistoricalSuccessErrorsOnly(t *testing.T) {
	path := t.TempDir() + "/test.db"
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, state := range []string{"committed", "uncertain", "rejected"} {
		if _, _, err := db.CreateEntryWithState(ctx, "test", state, "text", state, state, `{}`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.sql.Exec(`UPDATE entries SET error_code='old_code', error_message='old_message'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`DELETE FROM schema_migrations WHERE version=4`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	entries, err := db.List(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.State == "committed" {
			if entry.ErrorCode != "" || entry.ErrorMessage != "" || entry.Version != 2 {
				t.Fatalf("success not repaired: %+v", entry)
			}
		} else if entry.ErrorCode != "old_code" || entry.ErrorMessage != "old_message" || entry.Version != 1 {
			t.Fatalf("failure changed: %+v", entry)
		}
	}
	if err := db.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	entry, err := db.Get(ctx, 1)
	if err != nil || entry.Version != 2 {
		t.Fatalf("migration repeated: %+v err=%v", entry, err)
	}
}

func TestMigrationCopiesLegacyOpenRouterModelCatalog(t *testing.T) {
	path := t.TempDir() + "/test.db"
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := db.sql.ExecContext(ctx, `INSERT INTO openrouter_model_catalog(id, payload_json, fetched_at) VALUES(1, ?, ?)`, `[{"id":"legacy/model"}]`, "2026-09-10T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx, `DELETE FROM llm_model_catalog`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version=5`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot, err := db.LatestLLMModelCatalog(ctx, "openrouter|https://openrouter.ai/api/v1")
	if err != nil || snapshot.Provider != "openrouter" || snapshot.Payload != `[{"id":"legacy/model"}]` {
		t.Fatalf("legacy model catalog was not migrated: %+v err=%v", snapshot, err)
	}
}

func TestIdempotentCreateAndVersionedUpdate(t *testing.T) {
	db, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	one, existing, err := db.CreateEntry(ctx, "web", "req-1", "text", "午饭 35 元", `{"amount":"35"}`)
	if err != nil || existing {
		t.Fatalf("first create: entry=%+v existing=%v err=%v", one, existing, err)
	}
	two, existing, err := db.CreateEntry(ctx, "web", "req-1", "text", "午饭 35 元", `{"amount":"35"}`)
	if err != nil || !existing || two.ID != one.ID {
		t.Fatalf("idempotent create: entry=%+v existing=%v err=%v", two, existing, err)
	}
	if _, _, err := db.CreateEntry(ctx, "web", "req-1", "text", "晚饭 50 元", `{}`); err != ErrConflict {
		t.Fatalf("expected conflict, got %v", err)
	}
	updated, err := db.UpdateDraft(ctx, one.ID, one.Version, `{"amount":"36"}`)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != one.Version+1 {
		t.Fatalf("version did not increment: %+v", updated)
	}
	if _, err := db.UpdateDraft(ctx, one.ID, one.Version, `{}`); err != ErrConflict {
		t.Fatalf("expected stale version conflict, got %v", err)
	}
}

func TestFindByInputCanExcludeEntryBeingReparsed(t *testing.T) {
	db, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	entry, _, err := db.CreateEntry(ctx, "web", "find-input-1", "text", "午饭 35 元", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	found, err := db.FindByInput(ctx, "text", "午饭 35 元", 0)
	if err != nil || found.ID != entry.ID {
		t.Fatalf("find duplicate input: entry=%+v err=%v", found, err)
	}
	if _, err := db.FindByInput(ctx, "text", "午饭 35 元", entry.ID); err != ErrNotFound {
		t.Fatalf("expected excluded entry to be ignored, got %v", err)
	}
}

func TestRecordReconcileMissRequiresManualConfirmationAfterTwoChecks(t *testing.T) {
	db, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	entry, _, err := db.CreateEntryWithState(ctx, "web", "reconcile-miss-1", "text", "午饭 35 元", "uncertain", `{}`)
	if err != nil {
		t.Fatal(err)
	}

	entry, err = db.RecordReconcileMiss(ctx, entry.ID)
	if err != nil || entry.State != "uncertain" || entry.ReconcileMisses != 1 {
		t.Fatalf("first reconcile miss: entry=%+v err=%v", entry, err)
	}
	entry, err = db.RecordReconcileMiss(ctx, entry.ID)
	if err != nil || entry.State != "needs_confirmation" || entry.ReconcileMisses != 2 {
		t.Fatalf("second reconcile miss did not require manual confirmation: entry=%+v err=%v", entry, err)
	}
}

func TestCleanupCommittedDeletesOldEntriesAndVacuum(t *testing.T) {
	db, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	old, _, err := db.CreateEntryWithState(ctx, "web", "cleanup-old", "text", "旧记录", "committed", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	recent, _, err := db.CreateEntryWithState(ctx, "web", "cleanup-recent", "text", "新记录", "committed", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	manual, _, err := db.CreateEntryWithState(ctx, "web", "cleanup-manual", "text", "待确认", "needs_confirmation", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().UTC().Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.sql.ExecContext(ctx, `UPDATE entries SET updated_at=? WHERE id=?`, oldTime, old.ID); err != nil {
		t.Fatal(err)
	}
	deleted, err := db.CleanupCommitted(ctx, time.Now().UTC().Add(-7*24*time.Hour))
	if err != nil || deleted != 1 {
		t.Fatalf("cleanup result: deleted=%d err=%v", deleted, err)
	}
	if _, err := db.Get(ctx, old.ID); err != ErrNotFound {
		t.Fatalf("old committed entry remains: %v", err)
	}
	if _, err := db.Get(ctx, recent.ID); err != nil {
		t.Fatalf("recent committed entry was deleted: %v", err)
	}
	if _, err := db.Get(ctx, manual.ID); err != nil {
		t.Fatalf("manual entry was deleted: %v", err)
	}
}

func TestCleanupCommittedPermanentRetentionOnlyVacuums(t *testing.T) {
	db, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	entry, _, err := db.CreateEntryWithState(ctx, "web", "cleanup-permanent", "text", "永久保留", "committed", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().UTC().Add(-365 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.sql.ExecContext(ctx, `UPDATE entries SET updated_at=? WHERE id=?`, oldTime, entry.ID); err != nil {
		t.Fatal(err)
	}
	deleted, err := db.CleanupCommitted(ctx, time.Time{})
	if err != nil || deleted != 0 {
		t.Fatalf("permanent cleanup result: deleted=%d err=%v", deleted, err)
	}
	if _, err := db.Get(ctx, entry.ID); err != nil {
		t.Fatalf("permanent retention deleted entry: %v", err)
	}
}

func TestDeleteEntryRemovesEntryEvents(t *testing.T) {
	db, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	entry, _, err := db.CreateEntry(ctx, "web", "delete-1", "text", "午饭 35 元", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetState(ctx, entry.ID, "needs_confirmation", "rejected", "test", "待处理"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteEntry(ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Get(ctx, entry.ID); err != ErrNotFound {
		t.Fatalf("deleted entry is still readable: %v", err)
	}
	var eventCount int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM entry_events WHERE entry_id=?`, entry.ID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 {
		t.Fatalf("deleted entry events remain: %d", eventCount)
	}
}

func TestConfigurationSettingsAreRemovedFromSQLite(t *testing.T) {
	path := t.TempDir() + "/test.db"
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.SetSetting(ctx, "sui_book_name", "旧配置"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting(ctx, "processing_paused", "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version=2`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if value, err := db.GetSetting(ctx, "sui_book_name"); err != nil || value != "" {
		t.Fatalf("configuration setting remains in SQLite: %q err=%v", value, err)
	}
	if value, err := db.GetSetting(ctx, "processing_paused"); err != nil || value != "true" {
		t.Fatalf("operational setting was removed: %q err=%v", value, err)
	}
}
