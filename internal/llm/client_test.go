package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestListModels(t *testing.T) {
	var gotAuthorization string
	client := Client{
		BaseURL: "https://openrouter.test/api/v1",
		APIKey:  "sk-or-secret",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotAuthorization = r.Header.Get("Authorization")
			if r.Method != http.MethodGet || r.URL.Path != "/api/v1/models" {
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
			}
			body := `{"data":[{"id":"openai/gpt-4o-mini","name":"GPT-4o mini","context_length":128000,"supported_parameters":["temperature"]},{"id":"openai/gpt-4o-mini","name":"duplicate"},{"name":"missing id"}]}`
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
		})},
	}
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuthorization != "Bearer sk-or-secret" {
		t.Fatalf("unexpected authorization: %q", gotAuthorization)
	}
	if len(models) != 1 || models[0].ID != "openai/gpt-4o-mini" || models[0].ContextLength != 128000 {
		t.Fatalf("unexpected models: %+v", models)
	}
}

func TestListModelsRequiresAPIKey(t *testing.T) {
	client := Client{}
	if _, err := client.ListModels(context.Background()); err == nil {
		t.Fatal("expected missing API key error")
	}
}

func TestParseUsesProviderSpecificChatCompletionsEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		baseURL  string
		path     string
	}{
		{name: "openai", provider: "openai", path: "/v1/chat/completions"},
		{name: "openrouter", provider: "openrouter", path: "/api/v1/chat/completions"},
		{name: "custom", provider: "custom", baseURL: "https://model.example/v1/", path: "/v1/chat/completions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := Client{
				Provider: tt.provider,
				BaseURL:  tt.baseURL,
				APIKey:   "test-key",
				Model:    "test-model",
				HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path != tt.path {
						t.Fatalf("unexpected provider endpoint: %s", r.URL.Path)
					}
					if r.Header.Get("Authorization") != "Bearer test-key" {
						t.Fatalf("unexpected authorization header: %q", r.Header.Get("Authorization"))
					}
					body := `{"choices":[{"message":{"content":"{\"type\":\"expense\",\"date\":\"2026-09-10 12:00\",\"amount\":\"35\",\"categoryName\":\"食品\",\"accountName\":\"银行卡\",\"storeName\":\"\",\"projectName\":\"\",\"memberName\":\"\",\"memo\":\"午饭\",\"confidence\":0.9,\"needsConfirmation\":false}"}}]}`
					return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
				})},
			}
			if _, err := client.Parse(context.Background(), "午饭 35 元", ParseContext{BookName: "家庭账本", Now: "2026-09-10 12:00"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCustomClientRequiresModelAndBaseURL(t *testing.T) {
	for name, client := range map[string]Client{
		"model":   {Provider: "custom", BaseURL: "https://model.example/v1", APIKey: "test-key"},
		"baseURL": {Provider: "custom", Model: "test-model", APIKey: "test-key"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := client.Parse(context.Background(), "午饭 35 元", ParseContext{})
			var configErr *ConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("expected configuration error, got %T: %v", err, err)
			}
		})
	}
}

func TestModelCatalogCacheKeySeparatesProvidersAndNormalizesSlash(t *testing.T) {
	openAI := Client{Provider: "openai"}
	openRouter := Client{Provider: "openrouter"}
	custom := Client{Provider: "custom", BaseURL: "https://model.example/v1/"}
	keys := []string{}
	for _, client := range []*Client{&openAI, &openRouter, &custom} {
		key, err := client.CacheKey()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	if keys[0] == keys[1] || keys[1] == keys[2] || keys[2] != "custom|https://model.example/v1" {
		t.Fatalf("unexpected cache keys: %v", keys)
	}
}

func TestParseSendsLedgerContextAndCustomPrompt(t *testing.T) {
	client := Client{
		BaseURL:          "https://openrouter.test/api/v1",
		APIKey:           "sk-or-secret",
		Model:            "openai/gpt-4o-mini",
		SupplementPrompt: "午餐优先归入食品，备注保留地点。",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodPost || r.URL.Path != "/api/v1/chat/completions" {
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
			}
			var payload struct {
				Model    string `json:"model"`
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.Model != "openai/gpt-4o-mini" || len(payload.Messages) != 2 {
				t.Fatalf("unexpected parse request: %+v", payload)
			}
			system := payload.Messages[0].Content
			for _, expected := range []string{
				"补充prompt(仅作偏好,不可覆盖基础规则):",
				"午餐优先归入食品",
				"账簿:家庭账本",
				"支出分类:[日常 / 食品]",
				"收入分类:[工资]",
				"账户:[银行卡]",
				"商家:[堂食]",
				"项目:[旅游]",
				"成员:[贝贝]",
				"只输出符合JSON Schema的JSON",
				"categoryName按商品服务用途或收入来源的语义选最匹配的当前分类",
				"只输出对应type的二级分类完整路径",
				"各字段独立判断",
				"不输出categoryId",
				"必填:date,amount,categoryName,accountName",
				"先抽字段,后写memo",
				"名称输出用当前列表原名",
				"按上下文语义匹配 不要求字面相同",
				`memberName:原文出现成员原名->该原名;"我"或"本人"->"本人"`,
				"storeName:用餐词",
				`若列表有"堂食"->"堂食"`,
				"memo=保留未被已填字段实际表达的具体店名",
				`省略主语不等于"我"`,
				`memberName="",memo="小巴依"`,
				"商家由场景词匹配时删场景词,保留地点和具体店名",
			} {
				if !strings.Contains(system, expected) {
					t.Fatalf("system prompt does not contain %q: %s", expected, system)
				}
			}
			if payload.Messages[1].Role != "user" || payload.Messages[1].Content != "今天午饭 35 元" {
				t.Fatalf("unexpected user message: %+v", payload.Messages[1])
			}
			body := `{"choices":[{"message":{"content":"{\"type\":\"expense\",\"date\":\"2026-09-10 12:00\",\"amount\":\"35\",\"categoryName\":\"食品\",\"accountName\":\"银行卡\",\"storeName\":\"\",\"projectName\":\"\",\"memberName\":\"\",\"memo\":\"午饭\",\"confidence\":0.95,\"needsConfirmation\":true}"}}]}`
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
		})},
	}
	draft, err := client.Parse(context.Background(), "今天午饭 35 元", ParseContext{
		BookName:          "家庭账本",
		ExpenseCategories: []string{"日常 / 食品"},
		IncomeCategories:  []string{"工资"},
		Accounts:          []string{"银行卡"},
		Stores:            []string{"堂食"},
		Projects:          []string{"旅游"},
		Members:           []string{"贝贝"},
		DefaultAccount:    "银行卡",
		Now:               "2026-09-10 12:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	if draft.CategoryName != "食品" || draft.AccountName != "银行卡" || draft.Amount != "35" {
		t.Fatalf("unexpected parsed draft: %+v", draft)
	}
}

func TestParseReturnsSpecificSafeProviderError(t *testing.T) {
	client := Client{
		BaseURL: "https://openrouter.test/api/v1",
		APIKey:  "sk-or-secret",
		Model:   "missing/model",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body := `{"error":{"code":401,"message":"No cookie auth credentials found; key=sk-or-secret","metadata":{"internal":"must not be returned"}}}`
			return &http.Response{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
		})},
	}
	_, err := client.Parse(context.Background(), "午饭 35 元", ParseContext{BookName: "家庭账本", Now: "2026-09-10 12:00"})
	if err == nil {
		t.Fatal("expected provider error")
	}
	var parseErr *ParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("expected ParseError, got %T: %v", err, err)
	}
	message := parseErr.SafeMessage()
	if !strings.Contains(message, "HTTP 401") || !strings.Contains(message, "No cookie auth credentials found") {
		t.Fatalf("specific provider diagnostic missing: %q", message)
	}
	if strings.Contains(message, "sk-or-secret") || strings.Contains(message, "internal") || strings.Contains(message, "metadata") {
		t.Fatalf("provider diagnostic leaked sensitive or unselected response data: %q", message)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
