package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"sui-copilot/internal/accounting"
	"sui-copilot/internal/store"
)

const integrationBodyLimit = 1 << 20

var integrationOriginProtection = http.NewCrossOriginProtection()

// Structured submissions carry accounting fields. Model confidence and warnings
// are produced by the text parser and are not part of this request.
type structuredEntryRequest struct {
	ClientRequestID string          `json:"clientRequestId"`
	BookID          string          `json:"bookId"`
	Draft           structuredDraft `json:"draft"`
}

type structuredDraft struct {
	Type              string `json:"type"`
	Date              string `json:"date"`
	Amount            string `json:"amount"`
	CategoryName      string `json:"categoryName"`
	AccountName       string `json:"accountName"`
	StoreName         string `json:"storeName,omitempty"`
	ProjectName       string `json:"projectName,omitempty"`
	MemberName        string `json:"memberName,omitempty"`
	Memo              string `json:"memo,omitempty"`
	NeedsConfirmation bool   `json:"needsConfirmation,omitempty"`
}

type entryStatus struct {
	EntryID      int64  `json:"entryId"`
	State        string `json:"state"`
	ErrorCode    string `json:"errorCode,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

func statusOfEntry(entry store.Entry) entryStatus {
	return entryStatus{EntryID: entry.ID, State: entry.State, ErrorCode: entry.ErrorCode, ErrorMessage: entry.ErrorMessage}
}

type structuredEntryResponse struct {
	entryStatus
	Existing bool `json:"existing"`
}

type structuredEntryError struct {
	status  int
	code    string
	message string
}

func (e *structuredEntryError) Error() string { return e.code + ": " + e.message }

func (s *Server) createStructuredEntry(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if integrationOriginProtection.Check(r) != nil {
		writeError(w, http.StatusForbidden, "invalid_origin", "跨站请求不能提交记账")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "invalid_content_type", "记账请求必须使用 JSON")
		return
	}
	var req structuredEntryRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, integrationBodyLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || decoder.Decode(new(any)) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_json", "请求格式不正确")
		return
	}

	result, submitErr := s.queueStructuredEntry(r.Context(), req)
	if submitErr != nil {
		writeError(w, submitErr.status, submitErr.code, submitErr.message)
		return
	}
	status := http.StatusAccepted
	if result.Existing {
		status = http.StatusOK
	}
	w.Header().Set("Location", fmt.Sprintf("/api/v1/entries/%d", result.EntryID))
	writeJSON(w, status, result)
}

func (s *Server) queueStructuredEntry(ctx context.Context, req structuredEntryRequest) (structuredEntryResponse, *structuredEntryError) {
	requestID := strings.TrimSpace(req.ClientRequestID)
	bookID := strings.TrimSpace(req.BookID)
	if requestID == "" || bookID == "" {
		return structuredEntryResponse{}, &structuredEntryError{http.StatusBadRequest, "invalid_request", "必须提供 clientRequestId 和 bookId"}
	}
	draft := accounting.Draft{
		Type:         strings.TrimSpace(req.Draft.Type),
		Date:         strings.TrimSpace(req.Draft.Date),
		Amount:       strings.TrimSpace(req.Draft.Amount),
		CategoryName: strings.TrimSpace(req.Draft.CategoryName),
		AccountName:  strings.TrimSpace(req.Draft.AccountName),
		StoreName:    strings.TrimSpace(req.Draft.StoreName),
		ProjectName:  strings.TrimSpace(req.Draft.ProjectName),
		MemberName:   strings.TrimSpace(req.Draft.MemberName),
		Memo:         req.Draft.Memo,
		NeedsConfirm: req.Draft.NeedsConfirmation,
	}
	if len(accounting.MissingConfirmation(draft)) > 0 {
		return structuredEntryResponse{}, &structuredEntryError{http.StatusBadRequest, "invalid_draft", "必须提供日期、金额、二级分类和账户"}
	}
	if err := accounting.Validate(draft, time.FixedZone("Asia/Shanghai", 8*60*60)); err != nil {
		return structuredEntryResponse{}, &structuredEntryError{http.StatusBadRequest, "invalid_draft", err.Error()}
	}
	raw, err := accounting.EncodeDraft(draft)
	if err != nil {
		return structuredEntryResponse{}, &structuredEntryError{http.StatusInternalServerError, "draft_encode_failed", "保存草稿失败"}
	}
	summary := draft.CategoryName + " " + draft.Amount
	if memo := strings.TrimSpace(draft.Memo); memo != "" {
		summary += " · " + memo
	}

	s.entryMu.Lock()
	defer s.entryMu.Unlock()
	entry, existing, err := s.db.CreateStructuredEntry(ctx, requestID, bookID, summary, raw)
	if errors.Is(err, store.ErrConflict) {
		return structuredEntryResponse{}, &structuredEntryError{http.StatusConflict, "idempotency_conflict", "clientRequestId 已用于其他内容"}
	}
	if err != nil {
		return structuredEntryResponse{}, &structuredEntryError{http.StatusInternalServerError, "store_error", "保存结构化记账失败"}
	}
	if entry.State == accounting.StateQueued {
		s.enqueueProcessing(entry.ID)
	}
	return structuredEntryResponse{entryStatus: statusOfEntry(entry), Existing: existing}, nil
}
