package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"sui-copilot/internal/accounting"
	"sui-copilot/internal/store"
	"sui-copilot/internal/sui"
)

var (
	errReconcileInvalidState = errors.New("reconcile entry is not uncertain")
	errReconcileInvalidDraft = errors.New("reconcile draft is invalid")
	errReconcileInvalidDate  = errors.New("reconcile date is invalid")
)

type reconcileLookupError struct {
	err error
}

func (e *reconcileLookupError) Error() string { return e.err.Error() }
func (e *reconcileLookupError) Unwrap() error { return e.err }

type reconcileOutcome struct {
	Entry   store.Entry
	Matches []sui.Transaction
}

func (s *Server) checkUncertainEntry(ctx context.Context, id int64) (reconcileOutcome, error) {
	entry, err := s.db.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return reconcileOutcome{}, store.ErrNotFound
	}
	if err != nil {
		return reconcileOutcome{}, err
	}
	if entry.State != accounting.StateUncertain {
		return reconcileOutcome{Entry: entry}, errReconcileInvalidState
	}
	draft, err := accounting.DecodeDraft(entry.DraftJSON)
	if err != nil {
		return reconcileOutcome{Entry: entry}, errReconcileInvalidDraft
	}
	input, err := s.resolveWriteInput(ctx, draft)
	if err != nil {
		return reconcileOutcome{Entry: entry}, err
	}
	when, err := time.ParseInLocation("2006-01-02 15:04", draft.Date, time.FixedZone("Asia/Shanghai", 8*60*60))
	if err != nil {
		return reconcileOutcome{Entry: entry}, errReconcileInvalidDate
	}
	transactions, err := s.suiClient().List(ctx, when.Format("2006.01.02"), when.AddDate(0, 0, 1).Format("2006.01.02"))
	if err != nil {
		return reconcileOutcome{Entry: entry}, &reconcileLookupError{err: err}
	}
	matches := make([]sui.Transaction, 0, 1)
	for _, transaction := range transactions {
		if transactionMatches(transaction, input) {
			matches = append(matches, transaction)
		}
	}
	if len(matches) != 1 {
		updated, err := s.db.RecordReconcileMiss(ctx, id)
		if err != nil {
			return reconcileOutcome{Entry: entry, Matches: matches}, err
		}
		return reconcileOutcome{Entry: updated, Matches: matches}, nil
	}
	updated, err := s.db.SetState(ctx, id, accounting.StateUncertain, accounting.StateCommitted, "sui_reconciled", "通过随手记流水核对确认已记账")
	if err != nil {
		return reconcileOutcome{Entry: entry, Matches: matches}, err
	}
	return reconcileOutcome{Entry: updated, Matches: matches}, nil
}

func (s *Server) reconcileUncertainEntries(ctx context.Context) reconcileStats {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	entries, err := s.db.List(ctx, accounting.StateUncertain, 200)
	if err != nil {
		s.logger.Error("load uncertain entries failed", "error", err)
		return reconcileStats{Errors: 1}
	}
	stats := reconcileStats{}
	for _, entry := range entries {
		outcome, err := s.checkUncertainEntry(ctx, entry.ID)
		if err != nil {
			stats.Errors++
			s.logger.Error("automatic reconciliation failed", "entry_id", entry.ID, "error", err)
			continue
		}
		stats.Checked++
		if len(outcome.Matches) == 1 && outcome.Entry.State == accounting.StateCommitted {
			stats.Committed++
		}
		if outcome.Entry.State == accounting.StateNeedsConfirmation {
			stats.NeedsConfirmation++
		}
	}
	return stats
}

type reconcileStats struct {
	Checked           int `json:"checked"`
	Committed         int `json:"committed"`
	NeedsConfirmation int `json:"needsConfirmation"`
	Errors            int `json:"errors"`
}

func (s *Server) reconcileUncertain(w http.ResponseWriter, r *http.Request) {
	stats := s.reconcileUncertainEntries(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"status": "checked", "checked": stats.Checked, "committed": stats.Committed, "needsConfirmation": stats.NeedsConfirmation, "errors": stats.Errors})
}
