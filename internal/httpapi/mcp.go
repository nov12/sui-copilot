package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"sui-copilot/internal/buildinfo"
	"sui-copilot/internal/store"
)

type entryStatusRequest struct {
	EntryID int64 `json:"entryId" jsonschema:"submit_entry 返回的本地记账记录 ID。"`
}

type bookMetadataResult struct {
	BookID            string   `json:"bookId"`
	BookName          string   `json:"bookName"`
	Now               string   `json:"now" jsonschema:"当前北京时间，格式为 YYYY-MM-DD HH:mm。"`
	ExpenseCategories []string `json:"expenseCategories"`
	IncomeCategories  []string `json:"incomeCategories"`
	Accounts          []string `json:"accounts"`
	Stores            []string `json:"stores"`
	Projects          []string `json:"projects"`
	Members           []string `json:"members"`
}

func (s *Server) mcpHandler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "sui-copilot", Version: buildinfo.Version}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_book_metadata",
		Title:       "读取当前账簿资料",
		Description: "读取当前账簿 ID、可用于记账的二级分类、账户及其他可选字段。提交前用这些值填好账目。",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(false)},
	}, s.mcpBookMetadata)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "submit_entry",
		Title:       "提交结构化账目",
		Description: "提交已经解析好的账目，字段校验通过后将写入随手记。返回记录 ID，调用 get_entry_status 查询结果。网络重试时使用原请求 ID 和原参数。",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: new(false), IdempotentHint: true},
	}, s.mcpSubmitEntry)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_entry_status",
		Title:       "查询账目处理状态",
		Description: "查询提交后的本地状态。committed 表示已入账，needs_confirmation 需要人工确认，uncertain 需要核对随手记流水。",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(false)},
	}, s.mcpEntryStatus)

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless:           true,
		JSONResponse:        true,
		Logger:              s.logger,
		MaxRequestBodyBytes: integrationBodyLimit,
	})
	return integrationOriginProtection.Handler(handler)
}

func (s *Server) mcpBookMetadata(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, bookMetadataResult, error) {
	now := time.Now().In(time.FixedZone("Asia/Shanghai", 8*60*60))
	book, metadata, err := s.parseContext(ctx, now)
	if err != nil {
		status, code, message := submitErrorDetails(err)
		return nil, bookMetadataResult{}, &structuredEntryError{status, code, message}
	}
	return nil, bookMetadataResult{
		BookID: metadata.CurrentBook.ID, BookName: book.BookName, Now: book.Now,
		ExpenseCategories: book.ExpenseCategories, IncomeCategories: book.IncomeCategories,
		Accounts: book.Accounts, Stores: book.Stores, Projects: book.Projects, Members: book.Members,
	}, nil
}

func (s *Server) mcpSubmitEntry(ctx context.Context, _ *mcp.CallToolRequest, req structuredEntryRequest) (*mcp.CallToolResult, structuredEntryResponse, error) {
	result, err := s.queueStructuredEntry(ctx, req)
	if err != nil {
		return nil, structuredEntryResponse{}, err
	}
	return nil, result, nil
}

func (s *Server) mcpEntryStatus(ctx context.Context, _ *mcp.CallToolRequest, req entryStatusRequest) (*mcp.CallToolResult, entryStatus, error) {
	if req.EntryID <= 0 {
		return nil, entryStatus{}, errors.New("invalid_id: 记录 ID 不正确")
	}
	entry, err := s.db.Get(ctx, req.EntryID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && entry.BookID == "") {
		return nil, entryStatus{}, errors.New("not_found: 结构化记账记录不存在")
	}
	if err != nil {
		return nil, entryStatus{}, errors.New("store_error: 读取记录失败")
	}
	return nil, statusOfEntry(entry), nil
}
