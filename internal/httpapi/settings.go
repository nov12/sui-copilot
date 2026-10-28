package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"sui-bridge/internal/accounting"
	"sui-bridge/internal/config"
	"sui-bridge/internal/llm"
	"sui-bridge/internal/store"
	"sui-bridge/internal/sui"
)

var publicSettingKeys = map[string]bool{
	"sui_book_name":          true,
	"cleanup_retention_days": true,
	"llm_provider":           true,
	"llm_base_url":           true,
	"llm_model":              true,
	"llm_prompt":             true,
	"openrouter_model":       true,
	"openrouter_prompt":      true,
}

var secretSettingKeys = map[string]bool{
	"llm_api_key":        true,
	"openrouter_api_key": true,
}

var errCategoryMetadataNotSynced = errors.New("category metadata has not been synced")
var errCategoryMetadataBookMismatch = errors.New("category metadata belongs to another book")
var errAccountNotFound = errors.New("account was not found in current book")

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	settings, secrets := s.settingsResponse()
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings, "secrets": secrets})
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var values map[string]string
	if err := decodeJSON(r, &values); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "请求格式不正确")
		return
	}
	for key := range values {
		if !publicSettingKeys[key] && !secretSettingKeys[key] {
			writeError(w, http.StatusBadRequest, "invalid_setting", "包含不支持的设置项")
			return
		}
	}

	cfg := s.runtimeConfig()
	if value, ok := values["cleanup_retention_days"]; ok {
		retentionDays, err := parseCleanupRetentionDays(value)
		if err != nil {
			writeError(w, http.StatusBadRequest, "cleanup_retention_invalid", "超期清理时限必须是 1、2、7、15、30、90 天或永久")
			return
		}
		cfg.Cleanup.RetentionDays = &retentionDays
	}
	llmConfig := cfg.EffectiveLLM()
	oldProvider := strings.ToLower(strings.TrimSpace(llmConfig.Provider))
	if value, ok := firstSetting(values, "llm_provider"); ok {
		llmConfig.Provider = strings.ToLower(strings.TrimSpace(value))
	}
	providerChanged := oldProvider != strings.ToLower(strings.TrimSpace(llmConfig.Provider))
	if value, ok := firstSetting(values, "llm_base_url"); ok {
		llmConfig.BaseURL = strings.TrimSpace(value)
	}
	modelSet := false
	if value, ok := firstSetting(values, "llm_model", "openrouter_model"); ok {
		llmConfig.Model = strings.TrimSpace(value)
		modelSet = true
	}
	if value, ok := firstSetting(values, "llm_prompt", "openrouter_prompt"); ok {
		llmConfig.SupplementPrompt = strings.TrimSpace(value)
	}
	apiKey, apiKeySet := firstSetting(values, "llm_api_key", "openrouter_api_key")
	if providerChanged && (!apiKeySet || strings.TrimSpace(apiKey) == "") {
		writeError(w, http.StatusBadRequest, "llm_api_key_required", "切换模型服务商时必须输入新的 API Key")
		return
	}
	if apiKeySet && strings.TrimSpace(apiKey) != "" {
		llmConfig.APIKey = strings.TrimSpace(apiKey)
	}
	if providerChanged && !modelSet {
		llmConfig.Model = config.DefaultModel(llmConfig.Provider)
	}
	if strings.ToLower(strings.TrimSpace(llmConfig.Provider)) != config.ProviderCustom {
		llmConfig.BaseURL = ""
	}
	if err := llmConfig.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "llm_config_invalid", "模型服务配置无效："+err.Error())
		return
	}
	cfg.LLM = llmConfig
	cfg.OpenRouter = config.OpenRouterConfig{}
	for key, value := range values {
		value = strings.TrimSpace(value)
		switch key {
		case "sui_book_name":
			cfg.Sui.BookName = value
		}
	}
	if err := s.persistConfig(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "config_write_error", "写入配置文件失败")
		return
	}
	if _, ok := values["sui_book_name"]; ok {
		if err := s.db.SaveTargetBook(r.Context(), cfg.Sui.BookName); err != nil {
			writeError(w, http.StatusInternalServerError, "store_error", "保存目标账簿失败")
			return
		}
	}
	s.setRuntimeConfig(cfg)
	settings, secrets := s.settingsResponse()
	writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "settings": settings, "secrets": secrets})
}

func (s *Server) settingsResponse() (map[string]string, map[string]bool) {
	cfg := s.runtimeConfig()
	llmConfig := cfg.EffectiveLLM()
	provider := strings.ToLower(strings.TrimSpace(llmConfig.Provider))
	return map[string]string{
		"sui_book_name":          cfg.Sui.BookName,
		"cleanup_retention_days": strconv.Itoa(cfg.EffectiveCleanupRetentionDays()),
		"llm_provider":           provider,
		"llm_base_url":           llmConfig.BaseURL,
		"llm_model":              llmConfig.Model,
		"llm_prompt":             llmConfig.SupplementPrompt,
		// Deprecated aliases retained for older local clients.
		"openrouter_model":  llmConfig.Model,
		"openrouter_prompt": llmConfig.SupplementPrompt,
	}, map[string]bool{
		"llm_api_key":        strings.TrimSpace(llmConfig.APIKey) != "",
		"openrouter_api_key": provider == config.ProviderOpenRouter && strings.TrimSpace(llmConfig.APIKey) != "",
	}
}

func parseCleanupRetentionDays(value string) (int, error) {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "permanent") || value == "永久" {
		return 0, nil
	}
	days, err := strconv.Atoi(value)
	if err != nil || !config.IsValidCleanupRetentionDays(days) {
		return 0, errors.New("invalid cleanup retention")
	}
	return days, nil
}

func firstSetting(values map[string]string, keys ...string) (string, bool) {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value, true
		}
	}
	return "", false
}

func (s *Server) runtimeConfig() config.Config {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.config
}

func (s *Server) setRuntimeConfig(cfg config.Config) {
	s.configMu.Lock()
	s.config = cfg
	s.configMu.Unlock()
}

func (s *Server) persistConfig(cfg config.Config) error {
	if s.configPath == "" {
		return nil
	}
	file, err := os.CreateTemp(filepath.Dir(s.configPath), ".sui-bridge-config-*")
	if err != nil {
		return err
	}
	temporaryPath := file.Name()
	keepTemporary := false
	defer func() {
		if !keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	// Encode a canonical shape so saving a legacy configuration also performs
	// the one-way migration from [openrouter] to [llm].
	canonical := struct {
		Server  config.ServerConfig  `toml:"server"`
		Logging config.LoggingConfig `toml:"logging"`
		Sui     config.SuiConfig     `toml:"sui"`
		LLM     config.LLMConfig     `toml:"llm"`
		Cleanup config.CleanupConfig `toml:"cleanup"`
	}{
		Server:  cfg.Server,
		Logging: cfg.Logging,
		Sui:     cfg.Sui,
		LLM:     cfg.EffectiveLLM(),
		Cleanup: cfg.Cleanup,
	}
	if err := toml.NewEncoder(file).Encode(canonical); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, s.configPath); err != nil {
		return err
	}
	keepTemporary = true
	return nil
}

func (s *Server) llmModels(w http.ResponseWriter, r *http.Request) {
	client := s.llmClient()
	cacheKey, err := client.CacheKey()
	if err != nil {
		writeError(w, http.StatusBadRequest, "llm_config_invalid", "模型服务配置无效："+err.Error())
		return
	}
	snapshot, err := s.db.LatestLLMModelCatalog(r.Context(), cacheKey)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "not_synced", "models": []llm.Model{}, "fetchedAt": ""})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", "读取模型列表失败")
		return
	}
	var models []llm.Model
	if err := json.Unmarshal([]byte(snapshot.Payload), &models); err != nil {
		writeError(w, http.StatusInternalServerError, "catalog_error", "模型缓存损坏")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "cached", "models": models, "fetchedAt": snapshot.FetchedAt})
}

func (s *Server) syncLLMModels(w http.ResponseWriter, r *http.Request) {
	client := s.llmClient()
	if strings.TrimSpace(client.APIKey) == "" {
		writeError(w, http.StatusBadRequest, "llm_not_configured", "请先配置"+client.ProviderLabel()+" API Key，再同步模型列表")
		return
	}
	models, err := client.ListModels(r.Context())
	if err != nil {
		var configErr *llm.ConfigError
		if errors.As(err, &configErr) {
			writeError(w, http.StatusBadRequest, "llm_config_invalid", "模型服务配置无效："+configErr.Error())
			return
		}
		// Do not pass through upstream response bodies: they may contain
		// provider-specific diagnostics or request metadata.
		writeError(w, http.StatusBadGateway, "llm_models_unavailable", "同步"+client.ProviderLabel()+"模型列表失败")
		return
	}
	payload, err := json.Marshal(models)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "catalog_error", "编码模型列表失败")
		return
	}
	baseURL, err := client.ResolvedBaseURL()
	if err != nil {
		writeError(w, http.StatusBadRequest, "llm_config_invalid", "模型服务配置无效："+err.Error())
		return
	}
	cacheKey, err := client.CacheKey()
	if err != nil {
		writeError(w, http.StatusBadRequest, "llm_config_invalid", "模型服务配置无效："+err.Error())
		return
	}
	fetchedAt := time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.db.SaveLLMModelCatalog(r.Context(), cacheKey, client.ProviderName(), baseURL, string(payload), fetchedAt); err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", "保存模型列表失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "synced", "models": models, "fetchedAt": fetchedAt})
}

func (s *Server) llmPrompt(w http.ResponseWriter, r *http.Request) {
	contextData, _, err := s.parseContext(r.Context(), time.Now().In(time.FixedZone("Asia/Shanghai", 8*60*60)))
	if err != nil {
		s.writeParseError(w, err, "预览基础 prompt")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"prompt":      llm.BasePrompt(contextData),
		"generatedAt": time.Now().UTC(),
	})
}

func (s *Server) llmClient() *llm.Client {
	cfg := s.runtimeConfig()
	llmConfig := cfg.EffectiveLLM()
	return &llm.Client{Provider: llmConfig.Provider, BaseURL: llmConfig.BaseURL, APIKey: llmConfig.APIKey, Model: llmConfig.Model, SupplementPrompt: llmConfig.SupplementPrompt, HTTP: s.httpClient}
}

// openRouterClient is retained as a source-compatible helper for embedders;
// it now returns the currently selected provider client.
func (s *Server) openRouterClient() *llm.Client { return s.llmClient() }

func (s *Server) openRouterModels(w http.ResponseWriter, r *http.Request) {
	s.llmModels(w, r)
}

func (s *Server) syncOpenRouterModels(w http.ResponseWriter, r *http.Request) {
	s.syncLLMModels(w, r)
}

func (s *Server) openRouterPrompt(w http.ResponseWriter, r *http.Request) {
	s.llmPrompt(w, r)
}

func (s *Server) suiClient() *sui.Client {
	cfg := s.runtimeConfig()
	client := &sui.Client{BaseURL: cfg.Sui.BaseURL, BookName: cfg.Sui.BookName, HTTP: s.httpClient}
	if cfg.Sui.SessionFile != "" {
		session, err := s.loadSuiSession(cfg.Sui.SessionFile, cfg.Sui.SessionKeyFile)
		if err == nil {
			client.Jar = session.Jar
			client.Session = session
		} else {
			client.SessionErr = err
		}
	}
	return client
}

func (s *Server) loadSuiSession(sessionPath, keyPath string) (*sui.Session, error) {
	s.suiSessionMu.Lock()
	defer s.suiSessionMu.Unlock()
	if s.suiSession != nil && s.suiSessionPath == sessionPath && s.suiSessionKeyPath == keyPath {
		return s.suiSession, nil
	}
	session, err := sui.LoadSession(sessionPath, keyPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, sui.ErrSessionNotConfigured
	}
	if err != nil {
		return nil, sui.ErrSessionStoreCorrupted
	}
	s.suiSession = session
	s.suiSessionPath = sessionPath
	s.suiSessionKeyPath = keyPath
	return session, nil
}

func (s *Server) suiBooks(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.db.LatestBookCatalog(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "not_synced", "currentBookName": "", "books": []map[string]any{}})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", "读取账簿缓存失败")
		return
	}
	var books []sui.Book
	if err := json.Unmarshal([]byte(snapshot.Payload), &books); err != nil {
		writeError(w, http.StatusInternalServerError, "catalog_error", "账簿缓存损坏")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "cached",
		"currentBookName": snapshot.CurrentBookName,
		"books":           publicBooks(books),
		"updatedAt":       snapshot.UpdatedAt,
	})
}

func (s *Server) syncSuiBooks(w http.ResponseWriter, r *http.Request) {
	metadata, err := s.suiClient().ListBooks(r.Context())
	if err != nil {
		status, code, message := suiError(err)
		writeError(w, status, code, message)
		return
	}
	payload, err := json.Marshal(metadata.Books)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "metadata_error", "保存账簿资料失败")
		return
	}
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.db.SaveBookCatalog(r.Context(), string(payload), metadata.CurrentBook.Name, updatedAt); err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", "保存账簿资料失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "synced",
		"currentBookName": metadata.CurrentBook.Name,
		"books":           publicBooks(metadata.Books),
		"updatedAt":       updatedAt,
	})
}

func (s *Server) checkSuiConnection(w http.ResponseWriter, r *http.Request) {
	client := s.suiClient()
	metadata, err := client.ListBooks(r.Context())
	if err != nil {
		status, code, message := suiError(err)
		writeError(w, status, code, message)
		return
	}
	if strings.TrimSpace(client.BookName) == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":          "book_selection_required",
			"currentBookName": metadata.CurrentBook.Name,
			"books":           publicBooks(metadata.Books),
		})
		return
	}
	if err := sui.ValidateBook(metadata, client.BookName); err != nil {
		status, code, message := suiError(err)
		writeError(w, status, code, message)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "ok",
		"currentBookName": metadata.CurrentBook.Name,
		"books":           publicBooks(metadata.Books),
	})
}

func (s *Server) refreshSuiMetadata(w http.ResponseWriter, r *http.Request) {
	metadata, err := s.syncAndSaveMetadata(r.Context(), s.suiClient())
	if err != nil {
		status, code, message := suiError(err)
		writeError(w, status, code, message)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "refreshed", "metadata": metadata})
}

func (s *Server) syncSuiBook(w http.ResponseWriter, r *http.Request) {
	var request struct {
		BookName string `json:"bookName"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "请求格式不正确")
		return
	}
	client := s.suiClient()
	if strings.TrimSpace(request.BookName) != "" {
		client.BookName = strings.TrimSpace(request.BookName)
	}
	metadata, err := s.syncAndSaveMetadata(r.Context(), client)
	if err != nil {
		status, code, message := suiError(err)
		writeError(w, status, code, message)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "synced", "metadata": metadata})
}

func (s *Server) syncAndSaveMetadata(ctx context.Context, client *sui.Client) (sui.Metadata, error) {
	metadata, err := client.SyncMetadata(ctx)
	if err != nil {
		return sui.Metadata{}, err
	}
	payload, err := json.Marshal(metadata)
	if err != nil {
		return sui.Metadata{}, err
	}
	if err := s.db.SaveMetadata(ctx, metadata.CurrentBook.ID, string(payload)); err != nil {
		return sui.Metadata{}, err
	}
	return metadata, nil
}

func (s *Server) validateDraftCategory(ctx context.Context, draft accounting.Draft) error {
	snapshot, err := s.db.LatestMetadata(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return errCategoryMetadataNotSynced
	}
	if err != nil {
		return err
	}
	var metadata sui.Metadata
	if err := json.Unmarshal([]byte(snapshot.Payload), &metadata); err != nil {
		return errCategoryMetadataNotSynced
	}
	configuredBook := strings.TrimSpace(s.runtimeConfig().Sui.BookName)
	if configuredBook == "" || strings.TrimSpace(metadata.CurrentBook.Name) != configuredBook {
		return errCategoryMetadataBookMismatch
	}
	_, err = sui.ResolveLeafCategory(metadata, draft.Type, "", draft.CategoryName)
	return err
}

func (s *Server) resolveWriteInput(ctx context.Context, draft accounting.Draft) (sui.WriteInput, error) {
	_, metadata, err := s.parseContext(ctx, time.Now().In(time.FixedZone("Asia/Shanghai", 8*60*60)))
	if err != nil {
		return sui.WriteInput{}, err
	}
	category, err := sui.ResolveLeafCategory(metadata, draft.Type, "", draft.CategoryName)
	if err != nil {
		return sui.WriteInput{}, err
	}
	accountName := strings.TrimSpace(draft.AccountName)
	if accountName == "" && len(metadata.Accounts) > 0 {
		accountName = strings.TrimSpace(metadata.Accounts[0].Name)
	}
	accountID := metadataItemID(metadata.Accounts, accountName)
	if accountID == "" {
		return sui.WriteInput{}, errAccountNotFound
	}
	return sui.WriteInput{
		Type:       draft.Type,
		CategoryID: category.ID,
		StoreID:    metadataItemID(metadata.Stores, draft.StoreName),
		When:       draft.Date,
		ProjectID:  metadataItemID(metadata.Projects, draft.ProjectName),
		MemberID:   metadataItemID(metadata.Members, draft.MemberName),
		Memo:       draft.Memo,
		AccountID:  accountID,
		Amount:     draft.Amount,
	}, nil
}

func (s *Server) applyDefaultAccount(ctx context.Context, draft accounting.Draft) (accounting.Draft, error) {
	if strings.TrimSpace(draft.AccountName) != "" {
		return draft, nil
	}
	contextData, _, err := s.parseContext(ctx, time.Now().In(time.FixedZone("Asia/Shanghai", 8*60*60)))
	if err != nil {
		return accounting.Draft{}, err
	}
	draft.AccountName = contextData.DefaultAccount
	return draft, nil
}

func metadataItemID(items []sui.Item, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	for _, item := range items {
		if strings.TrimSpace(item.Name) == name {
			return strings.TrimSpace(item.ID)
		}
	}
	return ""
}

func publicBooks(books []sui.Book) []map[string]any {
	result := make([]map[string]any, 0, len(books))
	for _, book := range books {
		result = append(result, map[string]any{"name": book.Name, "current": book.Current})
	}
	return result
}

func suiError(err error) (int, string, string) {
	switch {
	case errors.Is(err, sui.ErrSessionExpired):
		return http.StatusUnauthorized, "sui_session_expired", "随手记会话已失效，请重新执行 sui-bridge auth login"
	case errors.Is(err, sui.ErrSessionNotConfigured):
		return http.StatusBadRequest, "sui_session_not_configured", "请先执行 sui-bridge auth login"
	case errors.Is(err, sui.ErrSessionStoreCorrupted):
		return http.StatusBadRequest, "sui_session_corrupted", "随手记登录会话不可用，请重新执行 sui-bridge auth login"
	case errors.Is(err, sui.ErrBookNameNotConfigured):
		return http.StatusBadRequest, "book_name_not_configured", "请先设置目标账簿名称"
	case errors.Is(err, sui.ErrBookNotFound):
		return http.StatusUnprocessableEntity, "book_not_found", "找不到配置的账簿名称"
	case errors.Is(err, sui.ErrBookNameAmbiguous):
		return http.StatusUnprocessableEntity, "book_name_ambiguous", "账簿名称不唯一，请在随手记中改成唯一名称"
	case errors.Is(err, sui.ErrBookMismatch):
		return http.StatusUnprocessableEntity, "book_not_current", "配置的账簿不是当前会话账簿"
	default:
		return http.StatusBadGateway, "sui_unavailable", "随手记连接失败"
	}
}
