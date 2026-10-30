package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

var ErrNotFound = errors.New("entry not found")
var ErrConflict = errors.New("idempotency conflict")

type Entry struct {
	ID              int64  `json:"id"`
	Source          string `json:"source"`
	ClientRequestID string `json:"clientRequestId"`
	InputType       string `json:"inputType"`
	InputText       string `json:"inputText"`
	State           string `json:"state"`
	Version         int    `json:"version"`
	ReconcileMisses int    `json:"reconcileMisses,omitempty"`
	DraftJSON       string `json:"draftJson"`
	ErrorCode       string `json:"errorCode,omitempty"`
	ErrorMessage    string `json:"errorMessage,omitempty"`
	CreatedAt       string `json:"createdAt"`
	UpdatedAt       string `json:"updatedAt"`
}

type MetadataSnapshot struct {
	BookID    string `json:"bookId"`
	Payload   string `json:"payloadJson"`
	FetchedAt string `json:"fetchedAt"`
}

type DB struct{ sql *sql.DB }

func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(ON)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	result := &DB{sql: db}
	if err := result.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return result, nil
}

func (db *DB) Close() error { return db.sql.Close() }

// migrationFiles keeps each applied version paired with its SQL file.
var migrationFiles = map[int]string{
	1: "migrations/001_init.sql",
	2: "migrations/002_remove_configuration_settings.sql",
	3: "migrations/003_reconcile_misses.sql",
	4: "migrations/004_clear_success_errors.sql",
	5: "migrations/005_llm_model_catalog.sql",
}

func (db *DB) migrate(ctx context.Context) error {
	// Each migration runs in one transaction with its version record so a
	// crash between the schema change and the bookkeeping cannot leave a
	// half-applied migration that fails to replay on the next start. The
	// version table is created by the first transaction, so fresh databases
	// cannot query it beforehand.
	for _, version := range []int{1, 2, 3, 4, 5} {
		tx, err := db.sql.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
			tx.Rollback()
			return err
		}
		var applied int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=?`, version).Scan(&applied); err != nil {
			tx.Rollback()
			return err
		}
		if applied == 0 {
			content, err := migrationFS.ReadFile(migrationFiles[version])
			if err != nil {
				tx.Rollback()
				return err
			}
			if _, err := tx.ExecContext(ctx, string(content)); err != nil {
				tx.Rollback()
				return fmt.Errorf("apply migration %d: %w", version, err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, version, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) CreateEntry(ctx context.Context, source, clientRequestID, inputType, inputText, draftJSON string) (Entry, bool, error) {
	return db.CreateEntryWithState(ctx, source, clientRequestID, inputType, inputText, "needs_confirmation", draftJSON)
}

func (db *DB) CreateEntryWithState(ctx context.Context, source, clientRequestID, inputType, inputText, state, draftJSON string) (Entry, bool, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := db.sql.ExecContext(ctx, `INSERT INTO entries(source, client_request_id, input_type, input_text, state, draft_json, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?)`, source, clientRequestID, inputType, inputText, state, draftJSON, now, now)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			entry, getErr := db.GetByIdempotency(ctx, source, clientRequestID)
			if getErr != nil {
				return Entry{}, false, getErr
			}
			if entry.InputType != inputType || entry.InputText != inputText {
				return Entry{}, false, ErrConflict
			}
			return entry, true, nil
		}
		return Entry{}, false, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Entry{}, false, err
	}
	entry, err := db.Get(ctx, id)
	return entry, false, err
}

func (db *DB) Get(ctx context.Context, id int64) (Entry, error) {
	var e Entry
	err := db.sql.QueryRowContext(ctx, `SELECT id, source, client_request_id, input_type, input_text, state, version, reconcile_misses, draft_json, error_code, error_message, created_at, updated_at FROM entries WHERE id=?`, id).Scan(&e.ID, &e.Source, &e.ClientRequestID, &e.InputType, &e.InputText, &e.State, &e.Version, &e.ReconcileMisses, &e.DraftJSON, &e.ErrorCode, &e.ErrorMessage, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	return e, err
}

func (db *DB) GetByIdempotency(ctx context.Context, source, requestID string) (Entry, error) {
	var e Entry
	err := db.sql.QueryRowContext(ctx, `SELECT id, source, client_request_id, input_type, input_text, state, version, reconcile_misses, draft_json, error_code, error_message, created_at, updated_at FROM entries WHERE source=? AND client_request_id=?`, source, requestID).Scan(&e.ID, &e.Source, &e.ClientRequestID, &e.InputType, &e.InputText, &e.State, &e.Version, &e.ReconcileMisses, &e.DraftJSON, &e.ErrorCode, &e.ErrorMessage, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	return e, err
}

// FindByInput returns an existing entry with the same normalized input.
// client_request_id is intentionally not part of this lookup: a new request
// ID must not allow the same accounting input to be parsed again.
func (db *DB) FindByInput(ctx context.Context, inputType, inputText string, excludeID int64) (Entry, error) {
	var e Entry
	query := `SELECT id, source, client_request_id, input_type, input_text, state, version, reconcile_misses, draft_json, error_code, error_message, created_at, updated_at FROM entries WHERE input_type=? AND TRIM(input_text)=?`
	args := []any{inputType, inputText}
	if excludeID > 0 {
		query += ` AND id<>?`
		args = append(args, excludeID)
	}
	query += ` ORDER BY id ASC LIMIT 1`
	err := db.sql.QueryRowContext(ctx, query, args...).Scan(&e.ID, &e.Source, &e.ClientRequestID, &e.InputType, &e.InputText, &e.State, &e.Version, &e.ReconcileMisses, &e.DraftJSON, &e.ErrorCode, &e.ErrorMessage, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	return e, err
}

func (db *DB) List(ctx context.Context, state string, limit int) ([]Entry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT id, source, client_request_id, input_type, input_text, state, version, reconcile_misses, draft_json, error_code, error_message, created_at, updated_at FROM entries`
	args := []any{}
	if state != "" {
		query += " WHERE state=?"
		args = append(args, state)
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]Entry, 0)
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Source, &e.ClientRequestID, &e.InputType, &e.InputText, &e.State, &e.Version, &e.ReconcileMisses, &e.DraftJSON, &e.ErrorCode, &e.ErrorMessage, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func (db *DB) UpdateDraft(ctx context.Context, id int64, version int, draftJSON string) (Entry, error) {
	return db.UpdateDraftState(ctx, id, version, "", draftJSON)
}

func (db *DB) UpdateDraftState(ctx context.Context, id int64, version int, nextState, draftJSON string) (Entry, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	query := `UPDATE entries SET draft_json=?, version=version+1, updated_at=? WHERE id=? AND version=? AND state NOT IN ('queued','processing','committing','committed')`
	args := []any{draftJSON, now, id, version}
	if nextState != "" {
		query = `UPDATE entries SET draft_json=?, state=?, version=version+1, updated_at=? WHERE id=? AND version=? AND state NOT IN ('queued','processing','committing','committed')`
		args = []any{draftJSON, nextState, now, id, version}
	}
	result, err := db.sql.ExecContext(ctx, query, args...)
	if err != nil {
		return Entry{}, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return Entry{}, ErrConflict
	}
	return db.Get(ctx, id)
}

// CompleteProcessing stores the result of background parsing and moves an
// entry out of processing in one transaction.
func (db *DB) CompleteProcessing(ctx context.Context, id int64, draftJSON, nextState, errorCode, errorMessage string) (Entry, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return Entry{}, err
	}
	var fromState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM entries WHERE id=? AND state='processing'`, id).Scan(&fromState); err != nil {
		tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return Entry{}, ErrConflict
		}
		return Entry{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE entries SET draft_json=?, state=?, error_code=?, error_message=?, reconcile_misses=0, version=version+1, updated_at=? WHERE id=? AND state='processing'`, draftJSON, nextState, errorCode, errorMessage, now, id)
	if err != nil {
		tx.Rollback()
		return Entry{}, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		tx.Rollback()
		return Entry{}, ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO entry_events(entry_id, from_state, to_state, message, created_at) VALUES(?,?,?,?,?)`, id, fromState, nextState, errorMessage, now); err != nil {
		tx.Rollback()
		return Entry{}, err
	}
	if err := tx.Commit(); err != nil {
		return Entry{}, err
	}
	return db.Get(ctx, id)
}

// RecoverInterruptedEntries finalizes work abandoned by the previous process.
// Parsing can be retried, but a committing entry may already exist in Sui and
// must be reconciled instead of replayed. Drafts and state events are retained.
func (db *DB) RecoverInterruptedEntries(ctx context.Context) (int64, error) {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT id, state FROM entries WHERE state IN ('processing','committing') ORDER BY id`)
	if err != nil {
		return 0, err
	}
	type interruptedEntry struct {
		id    int64
		state string
	}
	entries := make([]interruptedEntry, 0)
	for rows.Next() {
		var entry interruptedEntry
		if err := rows.Scan(&entry.id, &entry.state); err != nil {
			rows.Close()
			return 0, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	var recovered int64
	for _, entry := range entries {
		nextState := "rejected"
		errorCode := "processing_interrupted"
		errorMessage := "服务重启导致解析中断，请重新解析"
		if entry.state == "committing" {
			nextState = "uncertain"
			errorCode = "sui_commit_interrupted"
			errorMessage = "服务重启导致提交中断，请先核对随手记流水，不要重新提交"
		}
		result, err := tx.ExecContext(ctx, `UPDATE entries SET state=?, error_code=?, error_message=?, reconcile_misses=0, version=version+1, updated_at=? WHERE id=? AND state=?`, nextState, errorCode, errorMessage, now, entry.id, entry.state)
		if err != nil {
			return 0, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if changed == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO entry_events(entry_id, from_state, to_state, message, created_at) VALUES(?,?,?,?,?)`, entry.id, entry.state, nextState, errorMessage, now); err != nil {
			return 0, err
		}
		recovered++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return recovered, nil
}

func (db *DB) ReparseDraft(ctx context.Context, id int64, version int, inputText, nextState, draftJSON string) (Entry, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return Entry{}, err
	}
	var fromState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM entries WHERE id=? AND version=? AND state NOT IN ('queued','processing','committing','committed')`, id, version).Scan(&fromState); err != nil {
		tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return Entry{}, ErrConflict
		}
		return Entry{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE entries SET input_text=?, draft_json=?, state=?, error_code='', error_message='', reconcile_misses=0, version=version+1, updated_at=? WHERE id=? AND version=? AND state NOT IN ('queued','processing','committing','committed')`, inputText, draftJSON, nextState, now, id, version); err != nil {
		tx.Rollback()
		return Entry{}, err
	}
	if fromState != nextState {
		if _, err := tx.ExecContext(ctx, `INSERT INTO entry_events(entry_id, from_state, to_state, message, created_at) VALUES(?,?,?,?,?)`, id, fromState, nextState, "重新解析后更新状态", now); err != nil {
			tx.Rollback()
			return Entry{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Entry{}, err
	}
	return db.Get(ctx, id)
}

func (db *DB) DeleteEntry(ctx context.Context, id int64) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM entry_events WHERE entry_id=?`, id); err != nil {
		tx.Rollback()
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM entries WHERE id=?`, id)
	if err != nil {
		tx.Rollback()
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		tx.Rollback()
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// ListManual returns entries that need a user decision or manual recovery.
func (db *DB) ListManual(ctx context.Context, limit int) ([]Entry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := db.sql.QueryContext(ctx, `SELECT id, source, client_request_id, input_type, input_text, state, version, reconcile_misses, draft_json, error_code, error_message, created_at, updated_at FROM entries WHERE state IN ('pending_submission','needs_confirmation','rejected','uncertain') ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]Entry, 0)
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Source, &e.ClientRequestID, &e.InputType, &e.InputText, &e.State, &e.Version, &e.ReconcileMisses, &e.DraftJSON, &e.ErrorCode, &e.ErrorMessage, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// CleanupCommitted deletes committed entries older than cutoff and reclaims
// SQLite free pages after the delete. Entry events are removed first because
// their foreign key does not use ON DELETE CASCADE.
func (db *DB) CleanupCommitted(ctx context.Context, cutoff time.Time) (int64, error) {
	if cutoff.IsZero() {
		if err := db.Vacuum(ctx); err != nil {
			return 0, err
		}
		return 0, nil
	}
	cutoffText := cutoff.UTC().Format(time.RFC3339Nano)
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM entry_events WHERE entry_id IN (SELECT id FROM entries WHERE state='committed' AND updated_at < ?)`, cutoffText); err != nil {
		tx.Rollback()
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM entries WHERE state='committed' AND updated_at < ?`, cutoffText)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	deleted, _ := result.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if err := db.Vacuum(ctx); err != nil {
		return deleted, err
	}
	return deleted, nil
}

func (db *DB) Vacuum(ctx context.Context) error {
	_, err := db.sql.ExecContext(ctx, `VACUUM`)
	return err
}

// RecordReconcileMiss records one valid reconciliation check that did not
// find exactly one matching upstream transaction. Two consecutive misses
// require manual confirmation instead of remaining indefinitely uncertain.
func (db *DB) RecordReconcileMiss(ctx context.Context, id int64) (Entry, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return Entry{}, err
	}
	var misses int
	if err := tx.QueryRowContext(ctx, `SELECT reconcile_misses FROM entries WHERE id=? AND state='uncertain'`, id).Scan(&misses); err != nil {
		tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return Entry{}, ErrConflict
		}
		return Entry{}, err
	}
	misses++
	nextState := "uncertain"
	message := fmt.Sprintf("第%d次核对未找到唯一的随手记流水", misses)
	if misses >= 2 {
		nextState = "needs_confirmation"
		message = "连续两次核对未找到唯一的随手记流水，请人工确认"
	}
	result, err := tx.ExecContext(ctx, `UPDATE entries SET state=?, reconcile_misses=?, error_code=?, error_message=?, version=version+1, updated_at=? WHERE id=? AND state='uncertain'`, nextState, misses, "reconcile_miss", message, now, id)
	if err != nil {
		tx.Rollback()
		return Entry{}, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		tx.Rollback()
		return Entry{}, ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO entry_events(entry_id, from_state, to_state, message, created_at) VALUES(?,?,?,?,?)`, id, "uncertain", nextState, message, now); err != nil {
		tx.Rollback()
		return Entry{}, err
	}
	if err := tx.Commit(); err != nil {
		return Entry{}, err
	}
	return db.Get(ctx, id)
}

func (db *DB) SetState(ctx context.Context, id int64, from, to, code, message string) (Entry, error) {
	// Successful transitions keep their explanation in the event history, not
	// in the entry's current error fields.
	errorCode, errorMessage := code, message
	if to == "committed" || to == "committing" || to == "processing" {
		errorCode, errorMessage = "", ""
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return Entry{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE entries SET state=?, error_code=?, error_message=?, reconcile_misses=CASE WHEN ?='committed' THEN 0 ELSE reconcile_misses END, version=version+1, updated_at=? WHERE id=? AND state=?`, to, errorCode, errorMessage, to, now, id, from)
	if err != nil {
		tx.Rollback()
		return Entry{}, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		tx.Rollback()
		return Entry{}, ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO entry_events(entry_id, from_state, to_state, message, created_at) VALUES(?,?,?,?,?)`, id, from, to, message, now); err != nil {
		tx.Rollback()
		return Entry{}, err
	}
	if err := tx.Commit(); err != nil {
		return Entry{}, err
	}
	return db.Get(ctx, id)
}

func (db *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := db.sql.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, key, value, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (db *DB) GetSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := db.sql.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (db *DB) SaveMetadata(ctx context.Context, bookID, payload string) error {
	_, err := db.sql.ExecContext(ctx, `INSERT INTO metadata_snapshots(book_id, payload_json, fetched_at) VALUES(?,?,?)`, bookID, payload, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (db *DB) LatestMetadata(ctx context.Context) (MetadataSnapshot, error) {
	var snapshot MetadataSnapshot
	err := db.sql.QueryRowContext(ctx, `SELECT book_id, payload_json, fetched_at FROM metadata_snapshots ORDER BY id DESC LIMIT 1`).Scan(&snapshot.BookID, &snapshot.Payload, &snapshot.FetchedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MetadataSnapshot{}, ErrNotFound
	}
	return snapshot, err
}

type ModelCatalogSnapshot struct {
	CacheKey  string
	Provider  string
	BaseURL   string
	Payload   string
	FetchedAt string
}

func (db *DB) SaveLLMModelCatalog(ctx context.Context, cacheKey, provider, baseURL, payload, fetchedAt string) error {
	_, err := db.sql.ExecContext(ctx, `INSERT INTO llm_model_catalog(cache_key, provider, base_url, payload_json, fetched_at) VALUES(?,?,?,?,?) ON CONFLICT(cache_key) DO UPDATE SET provider=excluded.provider, base_url=excluded.base_url, payload_json=excluded.payload_json, fetched_at=excluded.fetched_at`, cacheKey, provider, baseURL, payload, fetchedAt)
	return err
}

func (db *DB) LatestLLMModelCatalog(ctx context.Context, cacheKey string) (ModelCatalogSnapshot, error) {
	var snapshot ModelCatalogSnapshot
	err := db.sql.QueryRowContext(ctx, `SELECT cache_key, provider, base_url, payload_json, fetched_at FROM llm_model_catalog WHERE cache_key=?`, cacheKey).Scan(&snapshot.CacheKey, &snapshot.Provider, &snapshot.BaseURL, &snapshot.Payload, &snapshot.FetchedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelCatalogSnapshot{}, ErrNotFound
	}
	return snapshot, err
}

// SaveModelCatalog is retained for source compatibility with the original
// OpenRouter-only store API.
func (db *DB) SaveModelCatalog(ctx context.Context, payload, fetchedAt string) error {
	return db.SaveLLMModelCatalog(ctx, "openrouter|https://openrouter.ai/api/v1", "openrouter", "https://openrouter.ai/api/v1", payload, fetchedAt)
}

// LatestModelCatalog is retained for source compatibility with the original
// OpenRouter-only store API.
func (db *DB) LatestModelCatalog(ctx context.Context) (ModelCatalogSnapshot, error) {
	return db.LatestLLMModelCatalog(ctx, "openrouter|https://openrouter.ai/api/v1")
}

type BookCatalogSnapshot struct {
	Payload         string
	CurrentBookName string
	UpdatedAt       string
}

func (db *DB) SaveBookCatalog(ctx context.Context, payload, currentBookName, updatedAt string) error {
	_, err := db.sql.ExecContext(ctx, `INSERT INTO sui_book_catalog(id, payload_json, current_book_name, updated_at) VALUES(1,?,?,?) ON CONFLICT(id) DO UPDATE SET payload_json=excluded.payload_json, current_book_name=excluded.current_book_name, updated_at=excluded.updated_at`, payload, currentBookName, updatedAt)
	return err
}

func (db *DB) LatestBookCatalog(ctx context.Context) (BookCatalogSnapshot, error) {
	var snapshot BookCatalogSnapshot
	err := db.sql.QueryRowContext(ctx, `SELECT payload_json, current_book_name, updated_at FROM sui_book_catalog WHERE id=1`).Scan(&snapshot.Payload, &snapshot.CurrentBookName, &snapshot.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BookCatalogSnapshot{}, ErrNotFound
	}
	return snapshot, err
}

type TargetBookSnapshot struct {
	BookName  string
	UpdatedAt string
}

func (db *DB) SaveTargetBook(ctx context.Context, bookName string) error {
	_, err := db.sql.ExecContext(ctx, `INSERT INTO sui_target_book(id, book_name, updated_at) VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET book_name=excluded.book_name, updated_at=excluded.updated_at`, strings.TrimSpace(bookName), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (db *DB) LatestTargetBook(ctx context.Context) (TargetBookSnapshot, error) {
	var snapshot TargetBookSnapshot
	err := db.sql.QueryRowContext(ctx, `SELECT book_name, updated_at FROM sui_target_book WHERE id=1`).Scan(&snapshot.BookName, &snapshot.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TargetBookSnapshot{}, ErrNotFound
	}
	return snapshot, err
}
