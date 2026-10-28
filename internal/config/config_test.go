package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sui-bridge.toml")
	content := "[server]\nlisten_addr = '127.0.0.1:9090'\ndata_dir = './runtime'\n[logging]\nlevel = 'debug'\n[sui]\nbase_url = 'https://example.test'\nbook_name = '工作账本'\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.ListenAddr != "127.0.0.1:9090" || cfg.Server.DataDir != "./runtime" || cfg.Logging.Level != "debug" || cfg.Sui.BaseURL != "https://example.test" || cfg.Sui.BookName != "工作账本" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestDefaultLoggingLevel(t *testing.T) {
	if level := Default().Logging.Level; level != "info" {
		t.Fatalf("default logging level = %q, want info", level)
	}
}

func TestCleanupRetentionValues(t *testing.T) {
	if got := Default().EffectiveCleanupRetentionDays(); got != DefaultCleanupRetentionDays {
		t.Fatalf("default cleanup retention = %d, want %d", got, DefaultCleanupRetentionDays)
	}
	for _, want := range []int{0, 1, 2, 7, 15, 30, 90} {
		value := want
		cfg := Config{Cleanup: CleanupConfig{RetentionDays: &value}}
		if got := cfg.EffectiveCleanupRetentionDays(); got != want {
			t.Fatalf("cleanup retention = %d, want %d", got, want)
		}
		if !IsValidCleanupRetentionDays(want) {
			t.Fatalf("cleanup retention %d was rejected", want)
		}
	}
	invalid := 3
	if got := (Config{Cleanup: CleanupConfig{RetentionDays: &invalid}}).EffectiveCleanupRetentionDays(); got != DefaultCleanupRetentionDays {
		t.Fatalf("invalid cleanup retention = %d, want default %d", got, DefaultCleanupRetentionDays)
	}
}

func TestLoadLegacyOpenRouterConfigMigratesToLLM(t *testing.T) {
	for _, name := range []string{"LLM_PROVIDER", "LLM_BASE_URL", "LLM_API_KEY", "LLM_MODEL", "LLM_PROMPT", "OPENROUTER_API_KEY", "OPENROUTER_MODEL", "OPENAI_API_KEY", "OPENAI_MODEL"} {
		t.Setenv(name, "")
	}
	path := filepath.Join(t.TempDir(), "sui-bridge.toml")
	content := "[openrouter]\napi_key = 'legacy-key'\nmodel = 'legacy/model'\nprompt = 'legacy prompt'\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.Provider != ProviderOpenRouter || cfg.LLM.APIKey != "legacy-key" || cfg.LLM.Model != "legacy/model" || cfg.LLM.SupplementPrompt != "legacy prompt" {
		t.Fatalf("legacy config was not migrated: %+v", cfg.LLM)
	}
	if cfg.OpenRouter.APIKey != cfg.LLM.APIKey {
		t.Fatalf("legacy compatibility view was not populated: %+v", cfg.OpenRouter)
	}
}

func TestLoadCanonicalLLMConfigAndValidateCustomEndpoint(t *testing.T) {
	for _, name := range []string{"LLM_PROVIDER", "LLM_BASE_URL", "LLM_API_KEY", "LLM_MODEL", "LLM_PROMPT", "OPENROUTER_API_KEY", "OPENROUTER_MODEL", "OPENAI_API_KEY", "OPENAI_MODEL"} {
		t.Setenv(name, "")
	}
	path := filepath.Join(t.TempDir(), "sui-bridge.toml")
	content := "[llm]\nprovider = 'custom'\nbase_url = 'http://127.0.0.1:9000/v1/'\nmodel = 'local/model'\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.LLM.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.Provider != ProviderCustom || cfg.LLM.BaseURL != "http://127.0.0.1:9000/v1/" || cfg.LLM.Model != "local/model" {
		t.Fatalf("unexpected custom config: %+v", cfg.LLM)
	}
	if err := (LLMConfig{Provider: ProviderCustom, BaseURL: "https://example.test/v1", Model: ""}).Validate(); err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("empty custom model was accepted: %v", err)
	}
}

func TestCanonicalLLMEnvironmentOverridesLegacyEnvironment(t *testing.T) {
	for _, name := range []string{"OPENROUTER_API_KEY", "OPENROUTER_MODEL", "OPENAI_API_KEY", "OPENAI_MODEL"} {
		t.Setenv(name, "legacy-value")
	}
	t.Setenv("LLM_PROVIDER", ProviderOpenRouter)
	t.Setenv("LLM_API_KEY", "canonical-key")
	t.Setenv("LLM_MODEL", "canonical/model")
	cfg := Default()
	ApplyEnv(&cfg)
	if cfg.LLM.APIKey != "canonical-key" || cfg.LLM.Model != "canonical/model" {
		t.Fatalf("canonical environment did not win: %+v", cfg.LLM)
	}
}
