package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"sui-bridge/internal/accounting"
	"sui-bridge/internal/store"
)

const backgroundProcessingTimeout = 10 * time.Minute
const statePersistenceTimeout = 5 * time.Second
const uncertainReconciliationInterval = 5 * time.Minute

type processingJob struct {
	id int64
}

func (s *Server) enqueueProcessing(id int64) {
	if id <= 0 {
		return
	}
	startWorker := false
	s.processingMu.Lock()
	if _, ok := s.processing[id]; ok {
		s.processingMu.Unlock()
		return
	}
	s.processing[id] = struct{}{}
	s.processingQueue = append(s.processingQueue, processingJob{id: id})
	if !s.processingRunning {
		s.processingRunning = true
		startWorker = true
	}
	s.processingMu.Unlock()

	if startWorker {
		go s.runProcessingQueue()
	}
}

// runProcessingQueue consumes entries in enqueue order. A single worker keeps
// parsing, metadata resolution, and automatic submission for one entry from
// overlapping with the next entry. This matters because one accounting entry
// can make several upstream requests, and Sui does not reliably tolerate
// interleaved write flows.
func (s *Server) runProcessingQueue() {
	for {
		s.processingMu.Lock()
		if len(s.processingQueue) == 0 {
			s.processingRunning = false
			s.processingMu.Unlock()
			return
		}
		job := s.processingQueue[0]
		s.processingQueue = s.processingQueue[1:]
		ctx, cancel := context.WithTimeout(context.Background(), backgroundProcessingTimeout)
		s.processingCancel[job.id] = cancel
		s.processingMu.Unlock()

		if err := s.processEntry(ctx, job.id); err != nil {
			s.logger.Error("background entry processing failed", "entry_id", job.id, "error", err)
		}
		cancel()

		s.processingMu.Lock()
		delete(s.processing, job.id)
		delete(s.processingCancel, job.id)
		s.processingMu.Unlock()
	}
}

func (s *Server) cancelProcessing(id int64) {
	s.processingMu.Lock()
	cancel := s.processingCancel[id]
	s.processingMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Server) enqueueQueuedEntries() {
	entries, err := s.db.List(context.Background(), accounting.StateQueued, 200)
	if err != nil {
		s.logger.Error("load queued entries failed", "error", err)
		return
	}
	// List returns newest first for the web UI. Reverse the startup batch so
	// recovery preserves the original submission order.
	for i := len(entries) - 1; i >= 0; i-- {
		s.enqueueProcessing(entries[i].ID)
	}
}

func (s *Server) recoverInterruptedEntries(ctx context.Context) error {
	recovered, err := s.db.RecoverInterruptedEntries(ctx)
	if err != nil {
		return err
	}
	if recovered > 0 {
		s.logger.Warn("recovered interrupted entries", "count", recovered)
	}
	return nil
}

func (s *Server) processEntry(ctx context.Context, id int64) error {
	entry, err := s.db.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if entry.State != accounting.StateQueued {
		return nil
	}
	entry, err = s.db.SetState(ctx, id, accounting.StateQueued, accounting.StateProcessing, "parse_started", "后台解析中")
	if err != nil {
		return err
	}

	draft, err := s.parseWithAI(ctx, entry.InputText)
	// Parsing may return after cancellation. Local state transitions still
	// need a short opportunity to finish, without extending upstream work.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statePersistenceTimeout)
	defer cancel()
	if err != nil {
		if ctx.Err() != nil {
			// A manual interrupt changes the database state before cancelling the
			// request. If the timeout itself fires, also avoid leaving the entry
			// permanently in processing.
			code, message := "processing_timeout", "解析超时，请重新解析"
			if errors.Is(ctx.Err(), context.Canceled) {
				code, message = "processing_cancelled", "已手动中断解析，请重新解析"
			}
			_, stateErr := s.db.SetState(saveCtx, id, accounting.StateProcessing, accounting.StateRejected, code, message)
			if stateErr != nil && !errors.Is(stateErr, store.ErrConflict) {
				return stateErr
			}
			return nil
		}
		code, message := backgroundParseError(err)
		_, completeErr := s.db.CompleteProcessing(saveCtx, id, `{}`, accounting.StateRejected, code, message)
		if completeErr != nil {
			return fmt.Errorf("save parse failure: %w", completeErr)
		}
		return nil
	}
	raw, err := accounting.EncodeDraft(draft)
	if err != nil {
		_, completeErr := s.db.CompleteProcessing(saveCtx, id, `{}`, accounting.StateRejected, "draft_encode_failed", "保存解析草稿失败")
		if completeErr != nil {
			return fmt.Errorf("save draft encoding failure: %w", completeErr)
		}
		return nil
	}

	if missing := accounting.MissingConfirmation(draft); draft.NeedsConfirm || len(missing) > 0 {
		message := "解析结果需要人工确认"
		if len(missing) > 0 {
			message = "解析结果缺少必要字段，请人工补充后确认"
		}
		_, err = s.db.CompleteProcessing(saveCtx, id, raw, accounting.StateNeedsConfirmation, "needs_confirmation", message)
		return err
	}
	if err := accounting.Validate(draft, time.FixedZone("Asia/Shanghai", 8*60*60)); err != nil {
		_, completeErr := s.db.CompleteProcessing(saveCtx, id, raw, accounting.StateNeedsConfirmation, "invalid_draft", err.Error())
		return completeErr
	}
	if err := s.validateDraftCategory(saveCtx, draft); err != nil {
		_, code, message := submitErrorDetails(err)
		_, completeErr := s.db.CompleteProcessing(saveCtx, id, raw, accounting.StateNeedsConfirmation, code, message)
		return completeErr
	}
	input, err := s.resolveWriteInput(saveCtx, draft)
	if err != nil {
		_, code, message := submitErrorDetails(err)
		_, completeErr := s.db.CompleteProcessing(saveCtx, id, raw, accounting.StateNeedsConfirmation, code, message)
		return completeErr
	}
	entry, err = s.db.CompleteProcessing(saveCtx, id, raw, accounting.StatePendingSubmission, "", "解析完成，准备自动提交")
	if err != nil {
		return err
	}
	if s.debug {
		return nil
	}
	_, _, err = s.submitResolvedEntry(ctx, entry, input)
	return err
}

func backgroundParseError(err error) (string, string) {
	_, code, message := parseErrorDetails(err, "后台解析")
	return code, message
}

func (s *Server) runReconciliationLoop(ctx context.Context) {
	ticker := time.NewTicker(uncertainReconciliationInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileUncertainEntries(ctx)
		}
	}
}

func (s *Server) runDailyMaintenance(ctx context.Context) {
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	timer := time.NewTimer(time.Until(nextMaintenanceAt(time.Now(), location)))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.cleanupCommitted(ctx, location)
			timer.Reset(time.Until(nextMaintenanceAt(time.Now(), location)))
		}
	}
}

func nextMaintenanceAt(now time.Time, location *time.Location) time.Time {
	current := now.In(location)
	next := time.Date(current.Year(), current.Month(), current.Day(), 2, 0, 0, 0, location)
	if !next.After(current) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

func (s *Server) cleanupCommitted(ctx context.Context, location *time.Location) (int64, error) {
	retentionDays := s.runtimeConfig().EffectiveCleanupRetentionDays()
	var cutoff time.Time
	if retentionDays > 0 {
		cutoff = time.Now().In(location).Add(-time.Duration(retentionDays) * 24 * time.Hour)
	}
	deleted, err := s.db.CleanupCommitted(ctx, cutoff)
	if err != nil {
		s.logger.Error("committed-entry maintenance failed", "error", err, "deleted", deleted)
		return deleted, err
	}
	s.logger.Info("committed-entry maintenance completed", "deleted", deleted, "retention_days", retentionDays, "vacuum", true)
	return deleted, nil
}

func (s *Server) cleanupEntries(w http.ResponseWriter, r *http.Request) {
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	deleted, err := s.cleanupCommitted(r.Context(), location)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cleanup_error", "清理记录失败，SQLite VACUUM 未完成")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "cleaned",
		"deleted":       deleted,
		"retentionDays": s.runtimeConfig().EffectiveCleanupRetentionDays(),
		"vacuum":        true,
	})
}
