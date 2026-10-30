package httpapi

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"sui-copilot/internal/accounting"
	"sui-copilot/internal/config"
	"sui-copilot/internal/llm"
	"sui-copilot/internal/store"
	"sui-copilot/internal/sui"
)

//go:embed static/* static/dist
var staticFS embed.FS

type Server struct {
	db         *store.DB
	logger     *slog.Logger
	mux        *http.ServeMux
	config     config.Config
	configMu   sync.RWMutex
	configPath string
	debug      bool
	httpClient *http.Client

	suiSessionMu      sync.Mutex
	suiSession        *sui.Session
	suiSessionPath    string
	suiSessionKeyPath string

	processingMu      sync.Mutex
	processing        map[int64]struct{}
	processingCancel  map[int64]context.CancelFunc
	processingQueue   []processingJob
	processingRunning bool
	reconcileMu       sync.Mutex
	entryMu           sync.Mutex
	suiWriteMu        sync.Mutex
}

// Handler returns the configured HTTP handler. It is useful for embedding the
// service behind a reverse proxy and for httptest-based API tests.
func (s *Server) Handler() http.Handler { return s.logging(s.mux) }

func NewServer(db *store.DB, logger *slog.Logger, runtimeConfigs ...config.Config) *Server {
	runtimeConfig := config.Default()
	if len(runtimeConfigs) > 0 {
		runtimeConfig = runtimeConfigs[0]
	}
	return newServer(db, logger, runtimeConfig, "", false)
}

// NewServerWithSecretKey is kept as a source-compatible constructor for
// embedders from the earlier settings implementation. Configuration is now
// stored in the TOML file, so the key argument is intentionally ignored.
func NewServerWithSecretKey(db *store.DB, logger *slog.Logger, runtimeConfig config.Config, _ []byte) *Server {
	return newServer(db, logger, runtimeConfig, "", false)
}

// NewServerWithConfigPath constructs a server that persists ordinary web
// settings back to the TOML file used at startup. Secrets are also written to
// this owner-only configuration file when explicitly submitted by the user;
// they are never written to the web API response.
func NewServerWithConfigPath(db *store.DB, logger *slog.Logger, runtimeConfig config.Config, configPath string) *Server {
	return newServer(db, logger, runtimeConfig, configPath, false)
}

func NewServerWithConfigPathAndDebug(db *store.DB, logger *slog.Logger, runtimeConfig config.Config, configPath string, debug bool) *Server {
	return newServer(db, logger, runtimeConfig, configPath, debug)
}

func newServer(db *store.DB, logger *slog.Logger, runtimeConfig config.Config, configPath string, debug bool) *Server {
	if target, err := db.LatestTargetBook(context.Background()); err == nil {
		if strings.TrimSpace(runtimeConfig.Sui.BookName) == "" {
			runtimeConfig.Sui.BookName = target.BookName
		}
	} else if errors.Is(err, store.ErrNotFound) && strings.TrimSpace(runtimeConfig.Sui.BookName) != "" {
		// Backfill the SQLite cache for configurations created before the cache
		// existed, without changing the TOML configuration source.
		_ = db.SaveTargetBook(context.Background(), runtimeConfig.Sui.BookName)
	}
	s := &Server{
		db:               db,
		logger:           logger,
		mux:              http.NewServeMux(),
		config:           runtimeConfig,
		configPath:       configPath,
		debug:            debug,
		httpClient:       &http.Client{Timeout: 120 * time.Second},
		processing:       make(map[int64]struct{}),
		processingCancel: make(map[int64]context.CancelFunc),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/v1/health", s.health)
	s.mux.HandleFunc("GET /api/v1/entries", s.listEntries)
	s.mux.HandleFunc("POST /api/v1/ingest", s.ingest)
	s.mux.HandleFunc("GET /api/v1/entries/{id}", s.getEntry)
	s.mux.HandleFunc("PATCH /api/v1/entries/{id}", s.updateEntry)
	s.mux.HandleFunc("POST /api/v1/entries/{id}/reparse", s.reparseEntry)
	s.mux.HandleFunc("POST /api/v1/entries/{id}/interrupt", s.interruptEntry)
	s.mux.HandleFunc("POST /api/v1/entries/{id}/submit-preview", s.submitPreview)
	s.mux.HandleFunc("POST /api/v1/entries/{id}/submit-preview/execute", s.executeSubmitPreview)
	s.mux.HandleFunc("DELETE /api/v1/entries/{id}", s.deleteEntry)
	s.mux.HandleFunc("POST /api/v1/entries/{id}/confirm", s.confirmEntry)
	s.mux.HandleFunc("POST /api/v1/entries/{id}/reconcile", s.reconcileEntry)
	s.mux.HandleFunc("POST /api/v1/entries/reconcile-uncertain", s.reconcileUncertain)
	s.mux.HandleFunc("POST /api/v1/entries/{id}/resolve", s.resolveEntry)
	s.mux.HandleFunc("GET /api/v1/metadata", s.metadata)
	s.mux.HandleFunc("POST /api/v1/metadata/refresh", s.refreshMetadata)
	s.mux.HandleFunc("POST /api/v1/connection/check", s.connectionCheck)
	s.mux.HandleFunc("POST /api/v1/processing/pause", s.pauseProcessing)
	s.mux.HandleFunc("POST /api/v1/processing/resume", s.resumeProcessing)
	s.mux.HandleFunc("GET /api/v1/settings", s.getSettings)
	s.mux.HandleFunc("PATCH /api/v1/settings", s.updateSettings)
	s.mux.HandleFunc("POST /api/v1/maintenance/cleanup", s.cleanupEntries)
	s.mux.HandleFunc("GET /api/v1/llm/models", s.llmModels)
	s.mux.HandleFunc("POST /api/v1/llm/models/sync", s.syncLLMModels)
	s.mux.HandleFunc("GET /api/v1/llm/prompt", s.llmPrompt)
	// Deprecated OpenRouter routes remain aliases for existing local clients.
	s.mux.HandleFunc("GET /api/v1/openrouter/models", s.openRouterModels)
	s.mux.HandleFunc("POST /api/v1/openrouter/models/sync", s.syncOpenRouterModels)
	s.mux.HandleFunc("GET /api/v1/openrouter/prompt", s.openRouterPrompt)
	s.mux.HandleFunc("GET /api/v1/debug", s.debugStatus)
	s.mux.HandleFunc("GET /api/v1/models", s.openRouterModels)
	s.mux.HandleFunc("GET /api/v1/sui/books", s.suiBooks)
	s.mux.HandleFunc("POST /api/v1/sui/books/sync", s.syncSuiBooks)
	s.mux.HandleFunc("POST /api/v1/sui/sync", s.syncSuiBook)
	if assets, err := fs.Sub(staticFS, "static/dist/assets"); err == nil {
		s.mux.Handle("GET /assets/", staticAssetHandler(assets))
	}
	s.mux.HandleFunc("GET /", s.index)
}

func staticAssetHandler(assets fs.FS) http.Handler {
	return http.StripPrefix("/assets/", http.FileServer(http.FS(assets)))
}

func (s *Server) Run(ctx context.Context, addr string) error {
	if err := s.recoverInterruptedEntries(ctx); err != nil {
		return fmt.Errorf("recover interrupted entries: %w", err)
	}
	s.enqueueQueuedEntries()
	go s.runDailyMaintenance(ctx)
	go s.runReconciliationLoop(ctx)
	// WriteTimeout is sized for the slowest legitimate handler: syncing book
	// metadata performs roughly nine rate-limited Sui requests (~15-20 s),
	// and confirm chains several 30 s-budgeted upstream calls. It is still a
	// hard bound so a stuck handler cannot hold a connection forever.
	server := &http.Server{Addr: addr, Handler: s.logging(s.mux), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()
	s.logger.Info("HTTP server listening", "addr", addr)
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		s.logger.Debug("http request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(started).String())
	})
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	path := "static/index.html"
	if _, err := fs.Stat(staticFS, "static/dist/index.html"); err == nil {
		path = "static/dist/index.html"
	}
	content, err := staticFS.ReadFile(path)
	if err != nil {
		http.Error(w, "frontend unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(content)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type ingestRequest struct {
	Source          string `json:"source"`
	InputType       string `json:"inputType"`
	Text            string `json:"text"`
	ClientRequestID string `json:"clientRequestId"`
}

var errLLMNotConfigured = errors.New("model service is not configured")

// Deprecated compatibility name for code that used the old OpenRouter-only
// implementation.
var errOpenRouterNotConfigured = errLLMNotConfigured

func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	var req ingestRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "请求格式不正确")
		return
	}
	if req.Source == "" {
		req.Source = "web"
	}
	if req.InputType == "" {
		req.InputType = "text"
	}
	inputText := strings.TrimSpace(req.Text)
	if inputText == "" {
		writeError(w, http.StatusBadRequest, "empty_input", "请输入记账内容")
		return
	}
	s.entryMu.Lock()
	defer s.entryMu.Unlock()
	if req.ClientRequestID != "" {
		existing, err := s.db.GetByIdempotency(r.Context(), req.Source, req.ClientRequestID)
		if err == nil {
			if existing.InputType != req.InputType || existing.InputText != inputText {
				writeError(w, http.StatusConflict, "idempotency_conflict", "clientRequestId 已用于其他内容")
				return
			}
			if existing.State == accounting.StateQueued {
				s.enqueueProcessing(existing.ID)
			}
			writeJSON(w, http.StatusOK, map[string]any{"entry": existing, "existing": true})
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusInternalServerError, "store_error", "读取记录失败")
			return
		}
	}
	if duplicate, err := s.db.FindByInput(r.Context(), req.InputType, inputText, 0); err == nil {
		writeDuplicateEntryError(w, duplicate)
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "store_error", "检查重复记录失败")
		return
	}
	if req.ClientRequestID == "" {
		req.ClientRequestID = newRequestID()
	}
	entry, existing, err := s.db.CreateEntryWithState(r.Context(), req.Source, req.ClientRequestID, req.InputType, inputText, accounting.StateQueued, `{}`)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "idempotency_conflict", "clientRequestId 已用于其他内容")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", "保存记录失败")
		return
	}
	if !existing {
		s.enqueueProcessing(entry.ID)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"entry": entry, "existing": existing})
}

func (s *Server) parseWithAI(ctx context.Context, text string) (accounting.Draft, error) {
	now := time.Now().In(time.FixedZone("Asia/Shanghai", 8*60*60))
	parseContext, metadata, err := s.parseContext(ctx, now)
	if err != nil {
		return accounting.Draft{}, err
	}
	client := s.llmClient()
	if strings.TrimSpace(client.APIKey) == "" {
		return accounting.Draft{}, errLLMNotConfigured
	}
	draft, err := client.Parse(ctx, text, parseContext)
	if err != nil {
		return accounting.Draft{}, err
	}
	return normalizeDraft(draft, text, parseContext, metadata), nil
}

func normalizeDraft(draft accounting.Draft, inputText string, contextData llm.ParseContext, metadata sui.Metadata) accounting.Draft {
	if amount := extractExplicitAmount(inputText); amount != "" {
		draft.Amount = amount
	}
	draft.CategoryName = strings.TrimSpace(draft.CategoryName)
	if category, err := sui.ResolveLeafCategory(metadata, draft.Type, "", draft.CategoryName); err == nil {
		draft.CategoryName = categoryDisplayName(category)
	}
	draft.AccountName = normalizeKnownName(draft.AccountName, contextData.Accounts)
	if draft.AccountName == "" {
		draft.AccountName = contextData.DefaultAccount
	}
	draft.StoreName = inferStoreName(draft.StoreName, inputText, contextData.Stores)
	draft.MemberName = inferMemberName(draft.MemberName, inputText, contextData.Members)
	draft.StoreName = normalizeKnownName(draft.StoreName, contextData.Stores)
	draft.ProjectName = normalizeKnownName(draft.ProjectName, contextData.Projects)
	draft.MemberName = normalizeKnownName(draft.MemberName, contextData.Members)
	if len(accounting.MissingConfirmation(draft)) > 0 {
		draft.NeedsConfirm = true
	}
	if _, err := sui.ResolveLeafCategory(metadata, draft.Type, "", draft.CategoryName); err != nil {
		draft.NeedsConfirm = true
	}
	if err := accounting.Validate(draft, time.FixedZone("Asia/Shanghai", 8*60*60)); err != nil {
		draft.NeedsConfirm = true
	}
	return draft
}

var explicitAmountPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?:¥|￥|人民币|RMB|CNY)\s*([0-9]+(?:\.[0-9]+)?)`),
	regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\s*(?:元|块钱|块|人民币)`),
	regexp.MustCompile(`(?:花了|花费|支付|付款|付了|共计|合计|总共|消费了|用了)\s*(?:约|大约)?\s*([0-9]+(?:\.[0-9]+)?)`),
}

var explicitDecimalAmountPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,2})?$`)

// nonAmountUnitPattern matches a digit run directly followed by a non-amount
// unit (time spans, counts, weights). "花了3小时"/"用了2次" then no longer
// override the model amount.
var nonAmountUnitPattern = regexp.MustCompile(`[0-9]+\s*(?:个?小时|个?半小时|分钟|秒钟|天|日|周|个?月|年|次|趟|遍|斤|公斤|克|千克|吨|个|只|条|件|瓶|杯|盒|包|袋|箱|台|部|辆|张|份|场|门|课|页|篇|字|首|幅|双|对|组|套|打|件套)`)

// yuanFragmentPattern matches the colloquial "N块M" form ("4块5" = 4.5 元).
// Its digits are not standalone amounts and would be misread as 4.
var yuanFragmentPattern = regexp.MustCompile(`[0-9]+\s*块\s*[0-9]`)

// extractExplicitAmount is a narrow local fallback for amounts explicitly
// written in the input. Currency markers and amount verbs make this safe from
// confusing quantities, product model numbers, or dates with the amount.
// Matches for non-amount quantities and fragmented colloquial amounts are
// discarded instead of overriding the model's value.
func extractExplicitAmount(inputText string) string {
	candidates := make([]string, 0, 2)
	for _, pattern := range explicitAmountPatterns {
		for _, match := range pattern.FindAllStringSubmatch(inputText, -1) {
			if len(match) == 2 && explicitDecimalAmountPattern.MatchString(match[1]) {
				candidates = append(candidates, match[1])
			}
		}
	}
	if len(candidates) != 1 {
		// Zero matches mean no explicit amount; more than one means the input
		// is ambiguous about which number is the amount, so keep the model's.
		return ""
	}
	candidate := candidates[0]
	if nonAmountUnitPattern.MatchString(inputText) || yuanFragmentPattern.MatchString(inputText) {
		return ""
	}
	return candidate
}

func inferStoreName(value, inputText string, options []string) string {
	mentioned := mentionedKnownNames(inputText, options)
	if len(mentioned) == 1 {
		return mentioned[0]
	}
	if containsMealText(inputText) && !containsTakeawaySignal(inputText) {
		for _, option := range options {
			if strings.TrimSpace(option) == "堂食" {
				return "堂食"
			}
		}
	}
	return value
}

func inferMemberName(value, inputText string, options []string) string {
	mentioned := mentionedKnownNames(inputText, options)
	if len(mentioned) == 1 {
		return mentioned[0]
	}
	if len(mentioned) == 0 && containsFirstPersonReference(inputText) {
		for _, option := range options {
			if strings.TrimSpace(option) == "本人" {
				return "本人"
			}
		}
	}
	// Self requires an explicit reference; omitted subjects are not evidence.
	if strings.TrimSpace(value) == "本人" && !containsFirstPersonReference(inputText) {
		return ""
	}
	return value
}

func mentionedKnownNames(inputText string, options []string) []string {
	mentioned := make([]string, 0, len(options))
	seen := make(map[string]struct{}, len(options))
	for _, option := range options {
		name := strings.TrimSpace(option)
		if name == "" || !strings.Contains(inputText, name) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		mentioned = append(mentioned, name)
	}
	return mentioned
}

func containsFirstPersonReference(inputText string) bool {
	return strings.Contains(inputText, "本人") || (strings.Contains(inputText, "我") && !strings.Contains(inputText, "我们"))
}

func containsMealText(inputText string) bool {
	for _, word := range []string{"早餐", "早饭", "午餐", "午饭", "晚餐", "晚饭", "吃饭", "用餐"} {
		if strings.Contains(inputText, word) {
			return true
		}
	}
	return false
}

func containsTakeawaySignal(inputText string) bool {
	for _, word := range []string{"外卖", "打包", "配送", "送餐", "点餐到家", "外带"} {
		if strings.Contains(inputText, word) {
			return true
		}
	}
	return false
}

func categoryDisplayName(category sui.Item) string {
	name := strings.TrimSpace(category.Name)
	if parent := strings.TrimSpace(category.ParentName); parent != "" {
		return parent + " / " + name
	}
	return name
}

func normalizeKnownName(value string, options []string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for _, option := range options {
		if value == strings.TrimSpace(option) {
			return strings.TrimSpace(option)
		}
	}
	return ""
}

func (s *Server) writeParseError(w http.ResponseWriter, err error, action string) {
	status, code, message := parseErrorDetails(err, action)
	writeError(w, status, code, message)
}

func parseErrorDetails(err error, action string) (int, string, string) {
	switch {
	case errors.Is(err, errCategoryMetadataNotSynced):
		return http.StatusUnprocessableEntity, "category_metadata_not_synced", "请先同步当前账簿分类，再" + action
	case errors.Is(err, errCategoryMetadataBookMismatch):
		return http.StatusUnprocessableEntity, "category_metadata_book_mismatch", "分类资料不属于当前目标账簿，请重新同步分类"
	case errors.Is(err, errLLMNotConfigured):
		return http.StatusBadRequest, "llm_not_configured", "请先配置模型服务 API Key，" + action + "时才会执行 AI 解析"
	default:
		var configErr *llm.ConfigError
		if errors.As(err, &configErr) {
			return http.StatusBadRequest, "llm_config_invalid", "模型服务配置无效：" + configErr.Error()
		}
		var parseErr *llm.ParseError
		if errors.As(err, &parseErr) {
			return http.StatusBadGateway, "llm_parse_failed", "AI 解析失败：" + parseErr.SafeMessage()
		}
		return http.StatusBadGateway, "llm_parse_failed", "AI 解析失败：解析服务内部错误，请查看服务日志"
	}
}

func (s *Server) parseContext(ctx context.Context, now time.Time) (llm.ParseContext, sui.Metadata, error) {
	snapshot, err := s.db.LatestMetadata(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return llm.ParseContext{}, sui.Metadata{}, errCategoryMetadataNotSynced
	}
	if err != nil {
		return llm.ParseContext{}, sui.Metadata{}, err
	}
	var metadata sui.Metadata
	if err := json.Unmarshal([]byte(snapshot.Payload), &metadata); err != nil {
		return llm.ParseContext{}, sui.Metadata{}, errCategoryMetadataNotSynced
	}
	configuredBook := strings.TrimSpace(s.runtimeConfig().Sui.BookName)
	if configuredBook == "" || strings.TrimSpace(metadata.CurrentBook.Name) != configuredBook {
		return llm.ParseContext{}, sui.Metadata{}, errCategoryMetadataBookMismatch
	}
	defaultAccount := ""
	if len(metadata.Accounts) > 0 {
		defaultAccount = strings.TrimSpace(metadata.Accounts[0].Name)
	}
	return llm.ParseContext{
		BookName:          metadata.CurrentBook.Name,
		ExpenseCategories: promptCategoryNames(metadata.Categories.Expense),
		IncomeCategories:  promptCategoryNames(metadata.Categories.Income),
		Accounts:          promptItemNames(metadata.Accounts),
		Stores:            promptItemNames(metadata.Stores),
		Projects:          promptItemNames(metadata.Projects),
		Members:           promptItemNames(metadata.Members),
		DefaultAccount:    defaultAccount,
		Now:               now.Format("2006-01-02 15:04"),
	}, metadata, nil
}

func promptCategoryNames(items []sui.Item) []string {
	result := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if !(item.Leaf || item.Level == 2 || item.ParentID != "") {
			continue
		}
		name := strings.TrimSpace(item.Name)
		if name == "" {
			continue
		}
		if parent := strings.TrimSpace(item.ParentName); parent != "" {
			name = parent + " / " + name
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return result
}

func promptItemNames(items []sui.Item) []string {
	result := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return result
}

func (s *Server) listEntries(w http.ResponseWriter, r *http.Request) {
	var entries []store.Entry
	var err error
	if r.URL.Query().Get("view") == "manual" {
		entries, err = s.db.ListManual(r.Context(), 50)
	} else {
		entries, err = s.db.List(r.Context(), r.URL.Query().Get("state"), 50)
	}
	if err != nil {
		writeError(w, 500, "store_error", "读取记录失败")
		return
	}
	writeJSON(w, 200, map[string]any{"entries": entries})
}

func (s *Server) getEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, 400, "invalid_id", "记录 ID 不正确")
		return
	}
	e, err := s.db.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, "not_found", "记录不存在")
		return
	}
	if err != nil {
		writeError(w, 500, "store_error", "读取记录失败")
		return
	}
	writeJSON(w, 200, map[string]any{"entry": e})
}

func (s *Server) deleteEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_id", "记录 ID 不正确")
		return
	}
	if err := s.db.DeleteEntry(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "记录不存在")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", "删除记录失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "id": id})
}

func (s *Server) reparseEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_id", "记录 ID 不正确")
		return
	}
	e, err := s.db.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "记录不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", "读取记录失败")
		return
	}
	if !isEditableEntryState(e.State) {
		if e.State == accounting.StateUncertain {
			writeError(w, http.StatusConflict, "invalid_state", "待核对记录的随手记写入结果未知，可能已入账；请先核对或人工处理，不能重新解析")
			return
		}
		writeError(w, http.StatusConflict, "invalid_state", "当前状态不能重新解析")
		return
	}
	var request struct {
		Text string `json:"text"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "请求格式不正确")
		return
	}
	inputText := strings.TrimSpace(request.Text)
	if inputText == "" {
		writeError(w, http.StatusBadRequest, "empty_input", "请输入要重新解析的内容")
		return
	}
	s.entryMu.Lock()
	defer s.entryMu.Unlock()
	if duplicate, err := s.db.FindByInput(r.Context(), e.InputType, inputText, e.ID); err == nil {
		writeDuplicateEntryError(w, duplicate)
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "store_error", "检查重复记录失败")
		return
	}
	updated, err := s.db.ReparseDraft(r.Context(), id, e.Version, inputText, accounting.StateQueued, `{}`)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "version_conflict", "记录已被更新")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", "更新解析草稿失败")
		return
	}
	s.enqueueProcessing(updated.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"entry": updated})
}

func (s *Server) interruptEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_id", "记录 ID 不正确")
		return
	}
	e, err := s.db.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "记录不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", "读取记录失败")
		return
	}
	if e.State != accounting.StateProcessing {
		writeError(w, http.StatusConflict, "invalid_state", "只有解析中的记录可以中断")
		return
	}
	updated, err := s.db.SetState(r.Context(), id, accounting.StateProcessing, accounting.StateRejected, "processing_cancelled", "已手动中断解析，请重新解析")
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "invalid_state", "记录已完成解析或被其他请求更新")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", "中断解析失败")
		return
	}
	s.cancelProcessing(id)
	writeJSON(w, http.StatusOK, map[string]any{"entry": updated})
}

func (s *Server) submitPreview(w http.ResponseWriter, r *http.Request) {
	if !s.debug {
		http.NotFound(w, r)
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_id", "记录 ID 不正确")
		return
	}
	e, err := s.db.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "记录不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", "读取记录失败")
		return
	}
	if e.State != accounting.StatePendingSubmission && e.State != accounting.StateNeedsConfirmation && e.State != accounting.StateRejected {
		writeError(w, http.StatusConflict, "invalid_state", "当前状态不能提交预览")
		return
	}
	draft, err := accounting.DecodeDraft(e.DraftJSON)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "invalid_draft", "草稿损坏")
		return
	}
	draft, err = s.applyDefaultAccount(r.Context(), draft)
	if err != nil {
		s.writeSubmitError(w, err)
		return
	}
	if missing := accounting.MissingConfirmation(draft); len(missing) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"code": "needs_confirmation", "missing": missing, "entry": e})
		return
	}
	if err := accounting.Validate(draft, time.FixedZone("Asia/Shanghai", 8*60*60)); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_draft", err.Error())
		return
	}
	input, err := s.resolveWriteInput(r.Context(), draft)
	if err != nil {
		s.writeSubmitError(w, err)
		return
	}
	preview := s.suiClient().WritePreview(input)
	writeJSON(w, http.StatusOK, preview)
}

func (s *Server) executeSubmitPreview(w http.ResponseWriter, r *http.Request) {
	if !s.debug {
		http.NotFound(w, r)
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_id", "记录 ID 不正确")
		return
	}
	e, err := s.db.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "记录不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", "读取记录失败")
		return
	}
	if e.State != accounting.StatePendingSubmission && e.State != accounting.StateNeedsConfirmation && e.State != accounting.StateRejected {
		writeError(w, http.StatusConflict, "invalid_state", "当前状态不能直接提交")
		return
	}
	var preview sui.RequestPreview
	if err := decodeJSON(r, &preview); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "提交预览格式不正确")
		return
	}
	draft, err := accounting.DecodeDraft(e.DraftJSON)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "invalid_draft", "草稿损坏")
		return
	}
	if missing := accounting.MissingConfirmation(draft); len(missing) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"code": "needs_confirmation", "missing": missing, "entry": e})
		return
	}
	if err := accounting.Validate(draft, time.FixedZone("Asia/Shanghai", 8*60*60)); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_draft", err.Error())
		return
	}
	if err := validateDebugPreview(s.runtimeConfig().Sui.BaseURL, draft.Type, preview); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_preview", err.Error())
		return
	}
	result, err := s.suiClient().ExecuteRequestPreview(r.Context(), preview)
	if err != nil {
		writeError(w, http.StatusBadGateway, "submit_failed", "提交请求失败，未收到随手记返回内容")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func isEditableEntryState(state string) bool {
	switch state {
	case accounting.StateQueued, accounting.StateProcessing, accounting.StateCommitting, accounting.StateCommitted, accounting.StateUncertain:
		return false
	default:
		return true
	}
}

func validateDebugPreview(baseURL, typeName string, preview sui.RequestPreview) error {
	if preview.Method != http.MethodPost {
		return errors.New("method 必须是 POST")
	}
	configuredBase := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if configuredBase == "" {
		configuredBase = "https://www.sui.com"
	}
	configuredURL, err := url.Parse(configuredBase)
	if err != nil {
		return errors.New("随手记地址配置无效")
	}
	previewURL, err := url.Parse(preview.URL)
	if err != nil || previewURL.Scheme != configuredURL.Scheme || previewURL.Host != configuredURL.Host || previewURL.RawQuery != "" || previewURL.Fragment != "" {
		return errors.New("URL 必须指向当前随手记地址且不能带查询参数")
	}
	expectedPath := "/tally/payout.rmi"
	if typeName == "income" {
		expectedPath = "/tally/income.rmi"
	}
	if previewURL.Path != expectedPath {
		return errors.New("URL 路径与记录类型不匹配")
	}
	return nil
}

func (s *Server) debugStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": s.debug})
}

func (s *Server) writeSubmitError(w http.ResponseWriter, err error) {
	status, code, message := submitErrorDetails(err)
	writeError(w, status, code, message)
}

func submitErrorDetails(err error) (int, string, string) {
	switch {
	case errors.Is(err, errCategoryMetadataNotSynced):
		return http.StatusUnprocessableEntity, "category_metadata_not_synced", "请先同步当前账簿分类"
	case errors.Is(err, errCategoryMetadataBookMismatch):
		return http.StatusUnprocessableEntity, "category_metadata_book_mismatch", "分类资料不属于当前目标账簿，请重新同步分类"
	case errors.Is(err, sui.ErrCategoryNotFound):
		return http.StatusUnprocessableEntity, "category_not_found", "分类不是当前账簿中已同步的二级分类"
	case errors.Is(err, sui.ErrCategoryNotLeaf):
		return http.StatusUnprocessableEntity, "category_not_leaf", "记账必须选择二级分类，不能使用一级分类"
	case errors.Is(err, sui.ErrCategoryNameAmbiguous):
		return http.StatusUnprocessableEntity, "category_name_ambiguous", "分类路径对应多个二级分类，请重新选择"
	case errors.Is(err, errAccountNotFound):
		return http.StatusUnprocessableEntity, "account_not_found", "账户不是当前账簿中已同步的账户"
	default:
		return http.StatusInternalServerError, "metadata_error", "解析提交参数失败"
	}
}

func (s *Server) updateEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, 400, "invalid_id", "记录 ID 不正确")
		return
	}
	var req struct {
		Version int              `json:"version"`
		Draft   accounting.Draft `json:"draft"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Version < 1 {
		writeError(w, 400, "invalid_request", "请求格式不正确")
		return
	}
	if err := accounting.Validate(req.Draft, time.FixedZone("Asia/Shanghai", 8*60*60)); err != nil {
		writeError(w, 400, "invalid_draft", err.Error())
		return
	}
	e, err := s.db.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, "not_found", "记录不存在")
		return
	}
	if err != nil {
		writeError(w, 500, "store_error", "读取记录失败")
		return
	}
	nextState := ""
	if e.State != accounting.StatePendingSubmission && len(accounting.MissingConfirmation(req.Draft)) == 0 {
		nextState = accounting.StatePendingSubmission
	}
	raw, _ := accounting.EncodeDraft(req.Draft)
	e, err = s.db.UpdateDraftState(r.Context(), id, req.Version, nextState, raw)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, 409, "version_conflict", "记录已被更新")
		return
	}
	if err != nil {
		writeError(w, 500, "store_error", "更新记录失败")
		return
	}
	writeJSON(w, 200, map[string]any{"entry": e})
}

func (s *Server) confirmEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, 400, "invalid_id", "记录 ID 不正确")
		return
	}
	e, err := s.db.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, "not_found", "记录不存在")
		return
	}
	if err != nil {
		writeError(w, 500, "store_error", "读取记录失败")
		return
	}
	if e.State != accounting.StatePendingSubmission && e.State != accounting.StateNeedsConfirmation && e.State != accounting.StateRejected {
		writeError(w, 409, "invalid_state", "当前状态不能确认")
		return
	}
	if s.debug {
		writeError(w, http.StatusConflict, "debug_preview_only", "Debug 模式下禁止直接提交，请先查看提交预览")
		return
	}
	draft, err := accounting.DecodeDraft(e.DraftJSON)
	if err != nil {
		writeError(w, 500, "invalid_draft", "草稿损坏")
		return
	}
	draft, err = s.applyDefaultAccount(r.Context(), draft)
	if err != nil {
		s.writeSubmitError(w, err)
		return
	}
	if missing := accounting.MissingConfirmation(draft); len(missing) > 0 {
		writeJSON(w, 422, map[string]any{"code": "needs_confirmation", "missing": missing, "entry": e})
		return
	}
	if err := accounting.Validate(draft, time.FixedZone("Asia/Shanghai", 8*60*60)); err != nil {
		writeError(w, 422, "invalid_draft", err.Error())
		return
	}
	if err := s.validateDraftCategory(r.Context(), draft); err != nil {
		switch {
		case errors.Is(err, errCategoryMetadataNotSynced):
			writeError(w, http.StatusUnprocessableEntity, "category_metadata_not_synced", "请先同步当前账簿分类")
		case errors.Is(err, errCategoryMetadataBookMismatch):
			writeError(w, http.StatusUnprocessableEntity, "category_metadata_book_mismatch", "分类资料不属于当前目标账簿，请重新同步分类")
		case errors.Is(err, sui.ErrCategoryNotFound):
			writeError(w, http.StatusUnprocessableEntity, "category_not_found", "分类不是当前账簿中已同步的二级分类")
		case errors.Is(err, sui.ErrCategoryNotLeaf):
			writeError(w, http.StatusUnprocessableEntity, "category_not_leaf", "记账必须选择二级分类，不能使用一级分类")
		case errors.Is(err, sui.ErrCategoryNameAmbiguous):
			writeError(w, http.StatusUnprocessableEntity, "category_name_ambiguous", "分类名称对应多个二级分类，请使用唯一的二级分类")
		default:
			writeError(w, http.StatusInternalServerError, "metadata_error", "读取分类资料失败")
		}
		return
	}
	input, err := s.resolveWriteInput(r.Context(), draft)
	if err != nil {
		s.writeSubmitError(w, err)
		return
	}
	updated, result, err := s.submitResolvedEntry(r.Context(), e, input)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "invalid_state", "记录已被其他请求处理")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", "提交失败且本地状态保存失败")
		return
	}
	if updated.State == accounting.StateCommitted {
		writeJSON(w, http.StatusOK, map[string]any{"entry": updated, "result": publicWriteResult(result)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": updated})
}

func (s *Server) submitResolvedEntry(ctx context.Context, entry store.Entry, input sui.WriteInput) (store.Entry, sui.WriteResult, error) {
	s.suiWriteMu.Lock()
	defer s.suiWriteMu.Unlock()

	committing, err := s.db.SetState(ctx, entry.ID, entry.State, accounting.StateCommitting, "sui_commit_started", "正在提交到随手记")
	if err != nil {
		return store.Entry{}, sui.WriteResult{}, err
	}

	result, writeErr := s.suiClient().Write(ctx, input)
	// The upstream operation may have exhausted its deadline, or a manual
	// caller may have disconnected after Sui accepted the write. Persist the
	// known outcome independently so the entry cannot remain committing just
	// because the request context was cancelled. Never replay the write.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statePersistenceTimeout)
	defer cancel()
	if writeErr == nil {
		updated, err := s.db.SetState(saveCtx, committing.ID, accounting.StateCommitting, accounting.StateCommitted, "sui_committed", "随手记已记账")
		if err != nil {
			return store.Entry{}, result, err
		}
		return updated, result, nil
	}

	nextState := accounting.StateRejected
	code := "sui_rejected"
	message := strings.TrimSpace(result.ErrorInfo)
	if message == "" {
		message = strings.TrimSpace(writeErr.Error())
	}
	if errors.Is(writeErr, sui.ErrUncertain) {
		nextState = accounting.StateUncertain
		code = "sui_uncertain"
		message = "提交结果不确定，请先核对随手记流水，不要自动重试"
	} else if errors.Is(writeErr, sui.ErrSessionExpired) {
		code = "sui_session_expired"
		message = "随手记会话已失效，请重新登录后人工处理"
	}
	updated, err := s.db.SetState(saveCtx, committing.ID, accounting.StateCommitting, nextState, code, message)
	if err != nil {
		return store.Entry{}, result, err
	}
	return updated, result, nil
}

func publicWriteResult(result sui.WriteResult) map[string]string {
	return map[string]string{
		"id":     result.UpstreamID,
		"budget": result.Budget,
		"price":  result.Price,
	}
}

func (s *Server) reconcileEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, 400, "invalid_id", "记录 ID 不正确")
		return
	}
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	outcome, err := s.checkUncertainEntry(r.Context(), id)
	if err != nil {
		s.writeReconcileError(w, id, err)
		return
	}
	if len(outcome.Matches) != 1 {
		message := "没有找到唯一匹配的随手记流水，记录仍为待核对"
		if len(outcome.Matches) > 1 {
			message = "找到多条匹配的随手记流水，记录仍为待核对"
		}
		if outcome.Entry.State == accounting.StateNeedsConfirmation {
			message = "连续两次核对未找到唯一的随手记流水，请人工确认"
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": outcome.Entry.State, "message": message, "matches": len(outcome.Matches), "entry": outcome.Entry})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "committed", "entry": outcome.Entry, "transaction": outcome.Matches[0]})
}

func (s *Server) writeReconcileError(w http.ResponseWriter, id int64, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "记录不存在")
		return
	}
	if errors.Is(err, errReconcileInvalidState) {
		writeError(w, http.StatusConflict, "invalid_state", "只有不确定记录可以核对")
		return
	}
	if errors.Is(err, errReconcileInvalidDraft) {
		writeError(w, http.StatusInternalServerError, "invalid_draft", "草稿损坏")
		return
	}
	if errors.Is(err, errReconcileInvalidDate) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_draft", "记录日期不正确")
		return
	}
	var lookupErr *reconcileLookupError
	if errors.As(err, &lookupErr) {
		s.logger.Error("reconcile transaction lookup failed", "entry_id", id, "error", lookupErr.err)
		status, code, message := suiError(lookupErr.err)
		writeError(w, status, code, message)
		return
	}
	s.writeSubmitError(w, err)
}

func transactionMatches(transaction sui.Transaction, input sui.WriteInput) bool {
	if !transactionTypeMatches(transaction, input.Type) {
		return false
	}
	if strings.TrimSpace(transaction.CategoryID) != strings.TrimSpace(input.CategoryID) || !transactionDateMatches(transaction.Date, input.When) {
		return false
	}
	if strings.TrimSpace(transaction.AccountID) != strings.TrimSpace(input.AccountID) && strings.TrimSpace(transaction.SellerAccountID) != strings.TrimSpace(input.AccountID) {
		return false
	}
	if !sameAmount(transaction.Amount, input.Amount) {
		return false
	}
	return strings.TrimSpace(input.Memo) == "" || strings.TrimSpace(transaction.Memo) == strings.TrimSpace(input.Memo)
}

func transactionTypeMatches(transaction sui.Transaction, typeName string) bool {
	value := strings.ToLower(strings.TrimSpace(transaction.Type + " " + transaction.Name))
	switch typeName {
	case "expense":
		return strings.Contains(value, "payout") || strings.Contains(value, "expense") || strings.Contains(value, "支出")
	case "income":
		return strings.Contains(value, "income") || strings.Contains(value, "收入")
	default:
		return false
	}
}

func transactionDateMatches(actual, expected string) bool {
	actual = strings.TrimSpace(actual)
	expected = strings.TrimSpace(expected)
	if actual == expected {
		return true
	}
	var serialized struct {
		DayOfMonth int `json:"date"`
		Hours      int `json:"hours"`
		Minutes    int `json:"minutes"`
		Month      int `json:"month"`
		Year       int `json:"year"`
	}
	if err := json.Unmarshal([]byte(actual), &serialized); err != nil || serialized.DayOfMonth < 1 || serialized.Month < 0 || serialized.Year < 0 {
		return false
	}
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	expectedTime, err := time.ParseInLocation("2006-01-02 15:04", expected, location)
	if err != nil {
		return false
	}
	return time.Date(serialized.Year+1900, time.Month(serialized.Month+1), serialized.DayOfMonth, serialized.Hours, serialized.Minutes, 0, 0, location).Equal(expectedTime)
}

func sameAmount(left, right string) bool {
	leftValue, leftErr := strconv.ParseFloat(strings.TrimSpace(left), 64)
	rightValue, rightErr := strconv.ParseFloat(strings.TrimSpace(right), 64)
	return leftErr == nil && rightErr == nil && leftValue == rightValue
}

func (s *Server) resolveEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, 400, "invalid_id", "记录 ID 不正确")
		return
	}
	var req struct {
		Result string `json:"result"`
		Note   string `json:"note"`
	}
	if err := decodeJSON(r, &req); err != nil || (req.Result != "committed" && req.Result != "rejected") {
		writeError(w, 400, "invalid_resolution", "result 必须是 committed 或 rejected")
		return
	}
	e, err := s.db.Get(r.Context(), id)
	if err != nil {
		writeError(w, 404, "not_found", "记录不存在")
		return
	}
	if e.State != accounting.StateUncertain && e.State != accounting.StateRejected {
		writeError(w, 409, "invalid_state", "当前状态不能人工处理")
		return
	}
	updated, err := s.db.SetState(r.Context(), id, e.State, req.Result, "manual_resolution", strings.TrimSpace(req.Note))
	if err != nil {
		writeError(w, 409, "invalid_state", "状态已被更新")
		return
	}
	writeJSON(w, 200, map[string]any{"entry": updated})
}

func (s *Server) metadata(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.db.LatestMetadata(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, 200, map[string]any{"status": "not_configured", "metadata": nil})
		return
	}
	if err != nil {
		writeError(w, 500, "store_error", "读取元数据失败")
		return
	}
	var payload any
	if json.Unmarshal([]byte(snapshot.Payload), &payload) != nil {
		payload = nil
	}
	writeJSON(w, 200, map[string]any{"status": "cached", "bookId": snapshot.BookID, "fetchedAt": snapshot.FetchedAt, "metadata": payload})
}

func (s *Server) refreshMetadata(w http.ResponseWriter, r *http.Request) {
	s.refreshSuiMetadata(w, r)
}
func (s *Server) connectionCheck(w http.ResponseWriter, r *http.Request) {
	s.checkSuiConnection(w, r)
}
func (s *Server) pauseProcessing(w http.ResponseWriter, r *http.Request) {
	if err := s.db.SetSetting(r.Context(), "processing_paused", "true"); err != nil {
		writeError(w, 500, "store_error", "保存处理状态失败")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "paused"})
}
func (s *Server) resumeProcessing(w http.ResponseWriter, r *http.Request) {
	if err := s.db.SetSetting(r.Context(), "processing_paused", "false"); err != nil {
		writeError(w, 500, "store_error", "保存处理状态失败")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "running"})
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

func writeDuplicateEntryError(w http.ResponseWriter, entry store.Entry) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"code":    "duplicate_entry",
		"message": fmt.Sprintf("数据库中已存在相同记录（记录 #%d），已拒绝再次解析", entry.ID),
		"entryId": entry.ID,
	})
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}
func newRequestID() string { return strconv.FormatInt(time.Now().UnixNano(), 36) }
