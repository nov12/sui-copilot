package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server  ServerConfig  `toml:"server"`
	Logging LoggingConfig `toml:"logging"`
	Sui     SuiConfig     `toml:"sui"`
	LLM     LLMConfig     `toml:"llm"`
	Cleanup CleanupConfig `toml:"cleanup"`
	// OpenRouter is retained for decoding configurations written before the
	// provider-neutral LLM settings were introduced. It is never written by
	// the current configuration writer.
	OpenRouter OpenRouterConfig `toml:"openrouter"`
}

type ServerConfig struct {
	ListenAddr string `toml:"listen_addr"`
	DataDir    string `toml:"data_dir"`
}

type LoggingConfig struct {
	Level string `toml:"level"`
}

type SuiConfig struct {
	BaseURL        string `toml:"base_url"`
	BookName       string `toml:"book_name"`
	SessionFile    string `toml:"session_file"`
	SessionKeyFile string `toml:"session_key_file"`
}

// CleanupConfig controls the automatic retention of successfully committed
// local entries. A nil value keeps the default for configurations assembled by
// embedders that do not know about this section yet.
type CleanupConfig struct {
	RetentionDays *int `toml:"retention_days"`
}

type OpenRouterConfig struct {
	APIKey           string `toml:"api_key"`
	Model            string `toml:"model"`
	SupplementPrompt string `toml:"prompt"`
}

const (
	ProviderOpenAI     = "openai"
	ProviderOpenRouter = "openrouter"
	ProviderCustom     = "custom"

	DefaultOpenAIBaseURL        = "https://api.openai.com/v1"
	DefaultOpenRouterBaseURL    = "https://openrouter.ai/api/v1"
	DefaultOpenAIModel          = "gpt-4o-mini"
	DefaultOpenRouterModel      = "openai/gpt-4o-mini"
	DefaultCleanupRetentionDays = 7
)

// IsValidCleanupRetentionDays reports whether days is one of the retention
// values exposed by the web settings. Zero means entries are kept forever.
func IsValidCleanupRetentionDays(days int) bool {
	switch days {
	case 0, 1, 2, 7, 15, 30, 90:
		return true
	default:
		return false
	}
}

type LLMConfig struct {
	Provider         string `toml:"provider"`
	BaseURL          string `toml:"base_url"`
	APIKey           string `toml:"api_key"`
	Model            string `toml:"model"`
	SupplementPrompt string `toml:"prompt"`
}

func Default() Config {
	retentionDays := DefaultCleanupRetentionDays
	return Config{
		Server:  ServerConfig{ListenAddr: "127.0.0.1:8080", DataDir: "./data"},
		Logging: LoggingConfig{Level: "info"},
		Sui:     SuiConfig{BaseURL: "https://www.sui.com"},
		LLM:     LLMConfig{Provider: ProviderOpenRouter, Model: DefaultOpenRouterModel},
		Cleanup: CleanupConfig{RetentionDays: &retentionDays},
	}
}

// EffectiveCleanupRetentionDays returns the configured retention period or
// the default when the value is absent or invalid. Zero is the explicit
// permanent-retention value.
func (cfg Config) EffectiveCleanupRetentionDays() int {
	if cfg.Cleanup.RetentionDays == nil || !IsValidCleanupRetentionDays(*cfg.Cleanup.RetentionDays) {
		return DefaultCleanupRetentionDays
	}
	return *cfg.Cleanup.RetentionDays
}

// Load reads TOML and then applies environment variable overrides. A missing
// file is returned to the caller so the binary can distinguish an optional
// default file from a malformed configuration.
func Load(path string) (Config, error) {
	cfg := Default()
	metadata, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return cfg, err
	}
	if !metadata.IsDefined("llm") && metadata.IsDefined("openrouter") {
		cfg.LLM = LLMConfig{
			Provider:         ProviderOpenRouter,
			APIKey:           cfg.OpenRouter.APIKey,
			Model:            cfg.OpenRouter.Model,
			SupplementPrompt: cfg.OpenRouter.SupplementPrompt,
		}
	}
	if metadata.IsDefined("llm") {
		if !metadata.IsDefined("llm", "model") {
			cfg.LLM.Model = ""
		}
		if !metadata.IsDefined("llm", "base_url") {
			cfg.LLM.BaseURL = ""
		}
	}
	cfg.OpenRouter = OpenRouterConfig{}
	Normalize(&cfg)
	ApplyEnv(&cfg)
	// Keep the legacy field populated for source-compatible readers. The
	// canonical writer and all runtime lookups use cfg.LLM.
	cfg.OpenRouter = OpenRouterConfig{APIKey: cfg.LLM.APIKey, Model: cfg.LLM.Model, SupplementPrompt: cfg.LLM.SupplementPrompt}
	return cfg, nil
}

// Normalize applies the provider defaults and converts the legacy in-memory
// OpenRouter representation used by older embedders and tests.
func Normalize(cfg *Config) {
	if cfg == nil {
		return
	}
	if isZeroLLM(cfg.LLM) && !isZeroOpenRouter(cfg.OpenRouter) {
		cfg.LLM = LLMConfig{
			Provider:         ProviderOpenRouter,
			APIKey:           cfg.OpenRouter.APIKey,
			Model:            cfg.OpenRouter.Model,
			SupplementPrompt: cfg.OpenRouter.SupplementPrompt,
		}
	}
	if cfg.LLM.Provider == "" {
		cfg.LLM.Provider = ProviderOpenRouter
	}
	if cfg.LLM.Model == "" {
		cfg.LLM.Model = DefaultModel(cfg.LLM.Provider)
	}
	if cfg.LLM.Provider != ProviderCustom {
		cfg.LLM.BaseURL = ""
	}
	cfg.OpenRouter = OpenRouterConfig{}
}

// EffectiveLLM returns the provider-neutral settings without modifying cfg.
// It keeps source compatibility for callers that still construct Config with
// the old OpenRouter field.
func (cfg Config) EffectiveLLM() LLMConfig {
	copy := cfg
	Normalize(&copy)
	return copy.LLM
}

func DefaultModel(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case ProviderOpenAI:
		return DefaultOpenAIModel
	case ProviderOpenRouter, "":
		return DefaultOpenRouterModel
	default:
		return ""
	}
}

// Validate checks settings that can be validated without contacting a model
// service. Credentials are intentionally optional so the service can start
// before a key is entered in the web UI.
func (cfg LLMConfig) Validate() error {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	switch provider {
	case ProviderOpenAI, ProviderOpenRouter:
	case ProviderCustom:
		base := strings.TrimSpace(cfg.BaseURL)
		parsed, err := url.Parse(base)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("custom LLM base URL must be an absolute http(s) URL without credentials, query, or fragment")
		}
	default:
		return fmt.Errorf("unsupported LLM provider %q", cfg.Provider)
	}
	if provider == ProviderCustom && strings.TrimSpace(cfg.Model) == "" {
		return fmt.Errorf("custom LLM model is required")
	}
	return nil
}

func isZeroLLM(cfg LLMConfig) bool {
	return strings.TrimSpace(cfg.Provider) == "" && strings.TrimSpace(cfg.BaseURL) == "" && strings.TrimSpace(cfg.APIKey) == "" && strings.TrimSpace(cfg.Model) == "" && strings.TrimSpace(cfg.SupplementPrompt) == ""
}

func isZeroOpenRouter(cfg OpenRouterConfig) bool {
	return strings.TrimSpace(cfg.APIKey) == "" && strings.TrimSpace(cfg.Model) == "" && strings.TrimSpace(cfg.SupplementPrompt) == ""
}

func ApplyEnv(cfg *Config) {
	if cfg == nil {
		return
	}
	Normalize(cfg)
	setenv("SUI_COPILOT_LISTEN_ADDR", &cfg.Server.ListenAddr)
	setenv("SUI_COPILOT_DATA_DIR", &cfg.Server.DataDir)
	setenv("SUI_COPILOT_LOG_LEVEL", &cfg.Logging.Level)
	setenv("SUI_BASE_URL", &cfg.Sui.BaseURL)
	setenv("SUI_BOOK_NAME", &cfg.Sui.BookName)
	setenv("SUI_SESSION_FILE", &cfg.Sui.SessionFile)
	setenv("SUI_SESSION_KEY_FILE", &cfg.Sui.SessionKeyFile)
	canonicalAPIKeySet := strings.TrimSpace(os.Getenv("LLM_API_KEY")) != ""
	canonicalModelSet := strings.TrimSpace(os.Getenv("LLM_MODEL")) != ""
	setenv("LLM_PROVIDER", &cfg.LLM.Provider)
	setenv("LLM_BASE_URL", &cfg.LLM.BaseURL)
	setenv("LLM_API_KEY", &cfg.LLM.APIKey)
	setenv("LLM_MODEL", &cfg.LLM.Model)
	setenv("LLM_PROMPT", &cfg.LLM.SupplementPrompt)
	if cfg.LLM.Provider == ProviderOpenRouter {
		if !canonicalAPIKeySet {
			setenv("OPENROUTER_API_KEY", &cfg.LLM.APIKey)
		}
		if !canonicalModelSet {
			setenv("OPENROUTER_MODEL", &cfg.LLM.Model)
		}
	}
	if cfg.LLM.Provider == ProviderOpenAI {
		if !canonicalAPIKeySet {
			setenv("OPENAI_API_KEY", &cfg.LLM.APIKey)
		}
		if !canonicalModelSet {
			setenv("OPENAI_MODEL", &cfg.LLM.Model)
		}
	}
}

func setenv(name string, target *string) {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		*target = value
	}
}
