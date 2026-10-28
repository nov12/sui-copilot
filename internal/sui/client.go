package sui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrSessionExpired = errors.New("sui session expired")
var ErrBookMismatch = errors.New("sui current book does not match configured book name")
var ErrBookNameNotConfigured = errors.New("sui book name is not configured")
var ErrBookNotFound = errors.New("sui configured book was not found")
var ErrBookNameAmbiguous = errors.New("sui configured book name is ambiguous")
var ErrCategoryNotFound = errors.New("sui category was not found")
var ErrCategoryNotLeaf = errors.New("sui category is not a leaf category")
var ErrCategoryNameAmbiguous = errors.New("sui category name is ambiguous")
var ErrUncertain = errors.New("sui write result is uncertain")

// A stalled Sui request must release the single-entry queue well before its
// ten-minute processing deadline. This also bounds manual and debug requests.
const requestTimeout = 30 * time.Second

type Client struct {
	BaseURL     string
	BookName    string
	HTTP        *http.Client
	Jar         http.CookieJar
	Session     *Session
	SessionErr  error
	MinInterval time.Duration
	mu          sync.Mutex
	nextRequest time.Time
}

type Book struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Current bool   `json:"isCurrent"`
}
type Item struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	ParentID   string `json:"parentId,omitempty"`
	ParentName string `json:"parentName,omitempty"`
	Type       string `json:"type,omitempty"`
	Level      int    `json:"level,omitempty"`
	Leaf       bool   `json:"isLeaf,omitempty"`
}
type Metadata struct {
	CurrentBook Book   `json:"currentBook"`
	Books       []Book `json:"books"`
	Categories  struct {
		Expense []Item `json:"expense"`
		Income  []Item `json:"income"`
	} `json:"categories"`
	Stores    []Item    `json:"stores"`
	Accounts  []Item    `json:"accounts"`
	Projects  []Item    `json:"projects"`
	Members   []Item    `json:"members"`
	Currency  string    `json:"currency"`
	FetchedAt time.Time `json:"fetchedAt"`
}

type WriteInput struct {
	Type       string
	CategoryID string
	StoreID    string
	When       string
	ProjectID  string
	MemberID   string
	Memo       string
	AccountID  string
	Amount     string
}

type WriteResult struct {
	UpstreamID string
	Code       int64
	ErrorInfo  string
	Budget     string
	Price      string
}

type RequestPreview struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Payload string            `json:"payload"`
}

type RequestResult struct {
	StatusCode int               `json:"statusCode"`
	Status     string            `json:"status"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
}

type Transaction struct {
	ID                string `json:"tranId"`
	Type              string `json:"tranType"`
	Name              string `json:"tranName"`
	Amount            string `json:"itemAmount"`
	CategoryID        string `json:"categoryId"`
	CategoryName      string `json:"categoryName"`
	AccountID         string `json:"buyerAcountId"`
	AccountName       string `json:"buyerAcount"`
	SellerAccountID   string `json:"sellerAcountId"`
	SellerAccountName string `json:"sellerAcount"`
	Memo              string `json:"memo"`
	Date              string `json:"date"`
}

// UnmarshalJSON normalizes Sui's mixed string/number transaction fields. The
// legacy endpoint returns account and category IDs as numbers in some
// responses and as strings in others.
func (t *Transaction) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID                json.RawMessage `json:"tranId"`
		Type              json.RawMessage `json:"tranType"`
		Name              json.RawMessage `json:"tranName"`
		Amount            json.RawMessage `json:"itemAmount"`
		CategoryID        json.RawMessage `json:"categoryId"`
		CategoryName      json.RawMessage `json:"categoryName"`
		AccountID         json.RawMessage `json:"buyerAcountId"`
		AccountName       json.RawMessage `json:"buyerAcount"`
		SellerAccountID   json.RawMessage `json:"sellerAcountId"`
		SellerAccountName json.RawMessage `json:"sellerAcount"`
		Memo              json.RawMessage `json:"memo"`
		Date              json.RawMessage `json:"date"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*t = Transaction{
		ID:                scalarJSONText(raw.ID),
		Type:              scalarJSONText(raw.Type),
		Name:              scalarJSONText(raw.Name),
		Amount:            scalarJSONText(raw.Amount),
		CategoryID:        scalarJSONText(raw.CategoryID),
		CategoryName:      scalarJSONText(raw.CategoryName),
		AccountID:         scalarJSONText(raw.AccountID),
		AccountName:       scalarJSONText(raw.AccountName),
		SellerAccountID:   scalarJSONText(raw.SellerAccountID),
		SellerAccountName: scalarJSONText(raw.SellerAccountName),
		Memo:              scalarJSONText(raw.Memo),
		Date:              scalarJSONText(raw.Date),
	}
	return nil
}

func (c *Client) Get(ctx context.Context, path string, form url.Values) (result []byte, resultErr error) {
	if c.Jar == nil {
		if c.SessionErr != nil {
			return nil, c.SessionErr
		}
		return nil, ErrSessionNotConfigured
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://www.sui.com"
	}
	if c.MinInterval <= 0 {
		c.MinInterval = time.Second
	}
	if c.Session != nil {
		if err := c.Session.wait(ctx, c.MinInterval); err != nil {
			return nil, err
		}
	} else {
		c.mu.Lock()
		wait := time.Until(c.nextRequest)
		if wait > 0 {
			c.mu.Unlock()
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-timer.C:
			}
		} else {
			c.mu.Unlock()
		}
		c.mu.Lock()
		c.nextRequest = time.Now().Add(c.MinInterval)
		c.mu.Unlock()
	}
	method := http.MethodGet
	var body io.Reader
	if form != nil {
		method = http.MethodPost
		body = strings.NewReader(form.Encode())
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", httpUserAgent)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	if c.Jar != nil {
		clientCopy := *client
		clientCopy.Jar = c.Jar
		client = &clientCopy
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := c.Session.SaveIfDirty(); err != nil && resultErr == nil {
			resultErr = fmt.Errorf("save sui session: %w", err)
		}
	}()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	closeErr := resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	// Only the book landing page may complete SSO. Other GET endpoints can
	// mutate state (book switching), and accounting POSTs must never replay.
	if path == "/report_index.do" && form == nil {
		data, resp, err = completeReportSSO(ctx, client, base, data, resp)
		if err != nil {
			return nil, err
		}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.Request.URL.Hostname() == "login.sui.com" || bytesLooksLikeLogin(data) {
		return nil, ErrSessionExpired
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("sui returned %s", resp.Status)
	}
	return data, nil
}

func (c *Client) CheckBook(ctx context.Context) (Metadata, error) {
	metadata, err := c.ListBooks(ctx)
	if err != nil {
		return Metadata{}, err
	}
	if err := ValidateBook(metadata, c.BookName); err != nil {
		return metadata, err
	}
	return metadata, nil
}

func (c *Client) ListBooks(ctx context.Context) (Metadata, error) {
	data, err := c.Get(ctx, "/report_index.do", nil)
	if err != nil {
		return Metadata{}, err
	}
	metadata := DeduplicateMetadata(ParseBooks(data))
	if metadata.CurrentBook.ID == "" {
		return Metadata{}, errors.New("sui current book was not found")
	}
	metadata.FetchedAt = time.Now().UTC()
	return metadata, nil
}

func ValidateBook(metadata Metadata, bookName string) error {
	book, err := FindBookByName(metadata, bookName)
	if err != nil {
		return err
	}
	if metadata.CurrentBook.ID != book.ID {
		return ErrBookMismatch
	}
	return nil
}

func FindBookByName(metadata Metadata, bookName string) (Book, error) {
	bookName = strings.TrimSpace(bookName)
	if bookName == "" {
		return Book{}, ErrBookNameNotConfigured
	}
	var matches []Book
	for _, book := range metadata.Books {
		if strings.TrimSpace(book.Name) == bookName {
			matches = append(matches, book)
		}
	}
	switch len(matches) {
	case 0:
		return Book{}, ErrBookNotFound
	case 1:
		return matches[0], nil
	default:
		return Book{}, ErrBookNameAmbiguous
	}
}

// ResolveLeafCategory resolves a category name or qualified category path to
// one second-level category. IDs are selected locally from current metadata;
// they are not part of the model prompt.
func ResolveLeafCategory(metadata Metadata, typeName, categoryID, categoryName string) (Item, error) {
	categoryID = strings.TrimSpace(categoryID)
	categoryName = strings.TrimSpace(categoryName)
	var categories []Item
	switch typeName {
	case "expense":
		categories = metadata.Categories.Expense
	case "income":
		categories = metadata.Categories.Income
	default:
		return Item{}, ErrCategoryNotFound
	}
	if categoryID != "" {
		for _, item := range categories {
			if strings.TrimSpace(item.ID) != categoryID {
				continue
			}
			if !(item.Leaf || item.Level == 2 || item.ParentID != "") {
				return Item{}, ErrCategoryNotLeaf
			}
			if categoryName != "" && !categoryNameMatches(item, categoryName) {
				return Item{}, ErrCategoryNotFound
			}
			return item, nil
		}
		return Item{}, ErrCategoryNotFound
	}
	if categoryName == "" {
		return Item{}, ErrCategoryNotFound
	}
	found := false
	var match Item
	leafMatches := 0
	for _, item := range categories {
		if !categoryNameMatches(item, categoryName) {
			continue
		}
		found = true
		if item.Leaf || item.Level == 2 || item.ParentID != "" {
			leafMatches++
			match = item
		}
	}
	if !found {
		return Item{}, ErrCategoryNotFound
	}
	if leafMatches == 0 {
		return Item{}, ErrCategoryNotLeaf
	}
	if leafMatches > 1 {
		return Item{}, ErrCategoryNameAmbiguous
	}
	return match, nil
}

func categoryNameMatches(item Item, categoryName string) bool {
	categoryName = strings.TrimSpace(categoryName)
	if strings.TrimSpace(item.Name) == categoryName {
		return true
	}
	parent := strings.TrimSpace(item.ParentName)
	return parent != "" && parent+" / "+strings.TrimSpace(item.Name) == categoryName
}

// ValidateLeafCategory checks that a draft category resolves to exactly one
// second-level category in the supplied metadata. Parent categories are
// useful for display but are not valid targets for a ledger entry.
func ValidateLeafCategory(metadata Metadata, typeName, categoryName string) error {
	_, err := ResolveLeafCategory(metadata, typeName, "", categoryName)
	return err
}

// SyncMetadata selects the configured book when it is not already current,
// then fetches that book's expense and income category pages and other
// metadata. The selected book is resolved by name and the resulting IDs stay
// inside the local adapter.
func (c *Client) SyncMetadata(ctx context.Context) (Metadata, error) {
	metadata, err := c.SwitchBook(ctx, c.BookName)
	if err != nil {
		return metadata, err
	}
	for _, resource := range []struct {
		path   string
		target *[]Item
		parser func([]byte) []Item
	}{
		{path: "/category/budgetCategory.do", target: &metadata.Categories.Expense, parser: func(data []byte) []Item { return ParseCategoryItems(data, "expense") }},
		{path: "/category/incomeCategory.do", target: &metadata.Categories.Income, parser: func(data []byte) []Item { return ParseCategoryItems(data, "income") }},
		{path: "/category/storeCategory.do", target: &metadata.Stores, parser: func(data []byte) []Item { return ParseCategoryItems(data, "store") }},
		{path: "/account/account.do", target: &metadata.Accounts, parser: ParseAccountItems},
		{path: "/category/projectCategory.do", target: &metadata.Projects, parser: func(data []byte) []Item { return ParseCategoryItems(data, "project") }},
		{path: "/category/member.do", target: &metadata.Members, parser: func(data []byte) []Item { return ParseCategoryItems(data, "member") }},
	} {
		data, err := c.Get(ctx, resource.path, nil)
		if err != nil {
			return Metadata{}, err
		}
		*resource.target = DeduplicateItems(resource.parser(data))
	}
	metadata = DeduplicateMetadata(metadata)
	metadata.FetchedAt = time.Now().UTC()
	return metadata, nil
}

// SwitchBook selects a uniquely named book through Sui's existing book switch
// endpoint, then verifies that the session now reports that book as current.
// The caller supplies a name; the upstream ID never becomes configuration.
func (c *Client) SwitchBook(ctx context.Context, bookName string) (Metadata, error) {
	metadata, err := c.ListBooks(ctx)
	if err != nil {
		return Metadata{}, err
	}
	book, err := FindBookByName(metadata, bookName)
	if err != nil {
		return metadata, err
	}
	if metadata.CurrentBook.ID != book.ID {
		base := strings.TrimRight(c.BaseURL, "/")
		if base == "" {
			base = "https://www.sui.com"
		}
		returnURL := url.QueryEscape(base + "/category/budgetCategory.do")
		if _, err := c.Get(ctx, "/systemSet/book.do?opt=switch&switchId="+url.QueryEscape(book.ID)+"&return="+returnURL, nil); err != nil {
			return Metadata{}, err
		}
		metadata, err = c.ListBooks(ctx)
		if err != nil {
			return Metadata{}, err
		}
	}
	if err := ValidateBook(metadata, bookName); err != nil {
		return metadata, err
	}
	return metadata, nil
}

// Write performs one upstream create request. It intentionally never retries:
// a timeout or malformed response may still mean that Sui accepted the write.
func (c *Client) Write(ctx context.Context, input WriteInput) (WriteResult, error) {
	if input.Type != "expense" && input.Type != "income" {
		return WriteResult{}, errors.New("unsupported transaction type")
	}
	if _, err := c.CheckBook(ctx); err != nil {
		return WriteResult{}, err
	}
	path, form := writeRequest(input)
	data, err := c.Get(ctx, path, form)
	if err != nil {
		if errors.Is(err, ErrSessionExpired) {
			return WriteResult{}, err
		}
		return WriteResult{}, ErrUncertain
	}
	result, err := parseWriteResult(data)
	if err != nil {
		return WriteResult{}, ErrUncertain
	}
	switch result.ID {
	case -1:
		return WriteResult{}, ErrSessionExpired
	case -999:
		return result.WriteResult, fmt.Errorf("sui rejected write: %s", result.ErrorInfo)
	}
	if result.ID <= 0 {
		return result.WriteResult, ErrUncertain
	}
	return result.WriteResult, nil
}

type parsedWriteResult struct {
	WriteResult
	ID int64
}

// Sui also returns object literals with unquoted keys. Match quoted strings
// as whole tokens so key-like text inside error messages is never rewritten.
var writeResponseToken = regexp.MustCompile(`"(?:\\.|[^"\\])*"|[{,]\s*[A-Za-z_$][A-Za-z0-9_$]*\s*:`)

func normalizeWriteResponse(data []byte) []byte {
	return writeResponseToken.ReplaceAllFunc(data, func(token []byte) []byte {
		if token[0] == '"' {
			return token
		}
		key := strings.TrimSpace(string(token[1 : len(token)-1]))
		return []byte(string(token[:1]) + `"` + key + `":`)
	})
}

// parseWriteResult accepts numeric and nested IDs, including Sui's object
// literal response {id:{id:21693739113481},price:0.01}. Only unquoted keys
// are normalized; the result must still be valid JSON, never executable code.
// The nested ID is kept as a string because Sui IDs are larger than the range
// of some integer types used by clients.
func parseWriteResult(data []byte) (parsedWriteResult, error) {
	var response struct {
		ID        json.RawMessage `json:"id"`
		ErrorInfo string          `json:"errorInfo"`
		Budget    json.RawMessage `json:"budget"`
		Price     json.RawMessage `json:"price"`
	}
	if err := json.Unmarshal(normalizeWriteResponse(data), &response); err != nil || len(response.ID) == 0 {
		return parsedWriteResult{}, errors.New("invalid sui write response")
	}

	budget := scalarJSONText(response.Budget)
	price := scalarJSONText(response.Price)
	var numericID int64
	if err := json.Unmarshal(response.ID, &numericID); err == nil {
		return parsedWriteResult{
			WriteResult: WriteResult{UpstreamID: positiveIDOrEmpty(numericID), Code: numericID, ErrorInfo: response.ErrorInfo, Budget: budget, Price: price},
			ID:          numericID,
		}, nil
	}

	var nested struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(response.ID, &nested); err != nil || len(nested.ID) == 0 {
		return parsedWriteResult{}, errors.New("invalid sui write id")
	}
	upstreamID, err := positiveIDText(nested.ID)
	if err != nil {
		return parsedWriteResult{}, err
	}
	return parsedWriteResult{
		WriteResult: WriteResult{UpstreamID: upstreamID, ErrorInfo: response.ErrorInfo, Budget: budget, Price: price},
		ID:          1,
	}, nil
}

func positiveIDOrEmpty(id int64) string {
	if id <= 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

func positiveIDText(raw json.RawMessage) (string, error) {
	var numeric int64
	if err := json.Unmarshal(raw, &numeric); err == nil && numeric > 0 {
		return strconv.FormatInt(numeric, 10), nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		text = strings.TrimSpace(text)
		if text != "" {
			if numeric, err := strconv.ParseInt(text, 10, 64); err == nil && numeric > 0 {
				return strconv.FormatInt(numeric, 10), nil
			}
		}
	}
	return "", errors.New("invalid positive sui write id")
}

func scalarJSONText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(string(raw))
}

func (c *Client) WritePreview(input WriteInput) RequestPreview {
	path, form := writeRequest(input)
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://www.sui.com"
	}
	headers := map[string]string{
		"Content-Type": "application/x-www-form-urlencoded; charset=UTF-8",
		"User-Agent":   httpUserAgent,
	}
	if c.Jar != nil {
		headers["Cookie"] = "[redacted]"
	}
	return RequestPreview{Method: http.MethodPost, URL: base + path, Headers: headers, Payload: form.Encode()}
}

func (c *Client) ExecuteRequestPreview(ctx context.Context, preview RequestPreview) (RequestResult, error) {
	if c.Jar == nil {
		if c.SessionErr != nil {
			return RequestResult{}, c.SessionErr
		}
		return RequestResult{}, ErrSessionNotConfigured
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, preview.Method, preview.URL, strings.NewReader(preview.Payload))
	if err != nil {
		return RequestResult{}, err
	}
	for name, value := range preview.Headers {
		if strings.EqualFold(name, "Cookie") || strings.EqualFold(name, "Authorization") {
			continue
		}
		req.Header.Set(name, value)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	clientCopy := *client
	clientCopy.Jar = c.Jar
	resp, err := clientCopy.Do(req)
	if err != nil {
		return RequestResult{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return RequestResult{}, err
	}
	headers := make(map[string]string)
	for name, values := range resp.Header {
		if strings.EqualFold(name, "Set-Cookie") || strings.EqualFold(name, "Cookie") || strings.EqualFold(name, "Authorization") {
			continue
		}
		if len(values) > 0 {
			headers[name] = values[0]
		}
	}
	return RequestResult{StatusCode: resp.StatusCode, Status: resp.Status, Headers: headers, Body: string(body)}, nil
}

func writeRequest(input WriteInput) (string, url.Values) {
	path := "/tally/payout.rmi"
	if input.Type == "income" {
		path = "/tally/income.rmi"
	}
	form := url.Values{
		"id":           {"0"},
		"category":     {input.CategoryID},
		"store":        {zero(input.StoreID)},
		"time":         {input.When},
		"project":      {zero(input.ProjectID)},
		"member":       {zero(input.MemberID)},
		"memo":         {input.Memo},
		"url":          {""},
		"out_account":  {"0"},
		"in_account":   {"0"},
		"debt_account": {""},
		"account":      {input.AccountID},
		"price":        {input.Amount},
		"price2":       {""},
	}
	return path, form
}

// List restores the session and verifies the target book before the read-only
// transaction query, just as Write does before a create request.
func (c *Client) List(ctx context.Context, beginDate, endDate string) ([]Transaction, error) {
	if _, err := c.CheckBook(ctx); err != nil {
		return nil, err
	}
	data, err := c.Get(ctx, "/tally/new.rmi", url.Values{"opt": {"list2"}, "beginDate": {beginDate}, "endDate": {endDate}, "cids": {"0"}, "bids": {"0"}, "sids": {"0"}, "pids": {"0"}, "memids": {"0"}, "order": {""}, "isDesc": {"0"}, "page": {"1"}, "note": {""}, "mids": {"0"}})
	if err != nil {
		return nil, err
	}
	var payload struct {
		Groups []struct {
			List []Transaction `json:"list"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode sui transactions: %w", err)
	}
	transactions := make([]Transaction, 0)
	for _, group := range payload.Groups {
		transactions = append(transactions, group.List...)
	}
	return transactions, nil
}

func zero(value string) string {
	if value == "" {
		return "0"
	}
	return value
}

func ParseBooks(data []byte) Metadata {
	text := string(data)
	result := Metadata{Currency: "CNY"}
	bookPattern := regexp.MustCompile(`(?is)<li\b([^>]*\bdata-bookid\s*=\s*["']([^"']+)["'][^>]*)>(.*?)</li\s*>`)
	for _, match := range bookPattern.FindAllStringSubmatch(text, -1) {
		name := attributeValue(match[1], "title")
		if name == "" {
			name = textContent(match[3])
		}
		book := Book{ID: strings.TrimSpace(match[2]), Name: name, Current: hasCurrentClass(match[1])}
		result.Books = append(result.Books, book)
		if book.Current && result.CurrentBook.ID == "" {
			result.CurrentBook = book
		}
	}
	if result.CurrentBook.ID == "" {
		current := regexp.MustCompile(`currentBookId\s*[:=]\s*["']([^"']+)["']`).FindStringSubmatch(text)
		if len(current) == 2 {
			result.CurrentBook.ID = current[1]
			for _, book := range result.Books {
				if book.ID == current[1] {
					result.CurrentBook = book
				}
			}
		}
	}
	return DeduplicateMetadata(result)
}

func DeduplicateMetadata(metadata Metadata) Metadata {
	metadata.Books = DeduplicateBooks(metadata.Books)
	metadata.Categories.Expense = DeduplicateItems(metadata.Categories.Expense)
	metadata.Categories.Income = DeduplicateItems(metadata.Categories.Income)
	metadata.Stores = DeduplicateItems(metadata.Stores)
	metadata.Accounts = DeduplicateItems(metadata.Accounts)
	metadata.Projects = DeduplicateItems(metadata.Projects)
	metadata.Members = DeduplicateItems(metadata.Members)
	if metadata.CurrentBook.ID != "" {
		for _, book := range metadata.Books {
			if book.ID == metadata.CurrentBook.ID {
				metadata.CurrentBook = book
				break
			}
		}
	}
	return metadata
}

func DeduplicateBooks(books []Book) []Book {
	result := make([]Book, 0, len(books))
	byID := make(map[string]int, len(books))
	for _, book := range books {
		book.ID = strings.TrimSpace(book.ID)
		if book.ID == "" {
			continue
		}
		if index, ok := byID[book.ID]; ok {
			if result[index].Name == "" {
				result[index].Name = book.Name
			}
			result[index].Current = result[index].Current || book.Current
			continue
		}
		byID[book.ID] = len(result)
		result = append(result, book)
	}
	return result
}

func DeduplicateItems(items []Item) []Item {
	result := make([]Item, 0, len(items))
	byID := make(map[string]int, len(items))
	for _, item := range items {
		item.ID = strings.TrimSpace(item.ID)
		if item.ID == "" {
			continue
		}
		if index, ok := byID[item.ID]; ok {
			if result[index].Name == "" {
				result[index].Name = item.Name
			}
			if result[index].ParentID == "" {
				result[index].ParentID = item.ParentID
			}
			if result[index].ParentName == "" {
				result[index].ParentName = item.ParentName
			}
			if result[index].Level == 0 {
				result[index].Level = item.Level
			}
			result[index].Leaf = result[index].Leaf || item.Leaf
			continue
		}
		byID[item.ID] = len(result)
		result = append(result, item)
	}
	return result
}

func ParseCategoryItems(data []byte, typeName string) []Item {
	// Income pages can nest child rows inside a parent row. A non-greedy div
	// regex would stop at the child's closing tag and never inspect the child
	// as a separate row. Find every category opening tag first, then match its
	// own closing div while accounting for nested divs.
	itemPattern := regexp.MustCompile(`(?is)<div\b([^>]*\bid\s*=\s*["']category(\d+)["'][^>]*)>`)
	divPattern := regexp.MustCompile(`(?is)</?div\b[^>]*>`)
	namePattern := regexp.MustCompile(`(?is)<li\b([^>]*\bclass\s*=\s*["'][^"']*li-level(?:1|2)[^"']*["'][^>]*)>(.*?)</li\s*>`)
	items := make([]Item, 0)
	seen := map[string]bool{}
	parentID, parentName := "", ""
	text := string(data)
	for _, match := range itemPattern.FindAllStringSubmatchIndex(text, -1) {
		id := strings.TrimSpace(text[match[4]:match[5]])
		if id == "" || seen[id] {
			continue
		}
		bodyEnd := matchingDivEnd(text, match[1], divPattern)
		body := text[match[1]:bodyEnd]
		name, level := categoryNameAndLevel(body, namePattern)
		if name == "" {
			name = attributeValue(body, "title")
		}
		if name == "" {
			continue
		}
		item := Item{ID: id, Name: name, Type: typeName, Level: categoryLevelNumber(level), Leaf: level == "level2"}
		if level == "level1" {
			parentID, parentName = id, name
		} else if level == "level2" {
			item.ParentID, item.ParentName = parentID, parentName
		}
		seen[id] = true
		items = append(items, item)
	}
	return DeduplicateItems(items)
}

func ParseAccountItems(data []byte) []Item {
	items, _ := parseAccountNameJSON(data)
	return items
}

func parseAccountNameJSON(data []byte) ([]Item, bool) {
	text := string(data)
	marker := regexp.MustCompile(`(?is)\baccountNameJson\s*[:=]\s*`)
	location := marker.FindStringIndex(text)
	if location == nil {
		return nil, false
	}
	start := strings.Index(text[location[1]:], "{")
	if start < 0 {
		return nil, true
	}
	start += location[1]
	end := jsonObjectEnd(text, start)
	if end < 0 {
		return nil, true
	}
	decoder := json.NewDecoder(strings.NewReader(text[start : end+1]))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, true
	}
	items := make([]Item, 0)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, true
		}
		id, ok := key.(string)
		if !ok {
			return nil, true
		}
		var name string
		if err := decoder.Decode(&name); err != nil {
			return nil, true
		}
		id, name = strings.TrimSpace(id), strings.TrimSpace(name)
		if id == "" || name == "" {
			continue
		}
		items = append(items, Item{ID: id, Name: name, Type: "account", Leaf: true})
	}
	if _, err := decoder.Token(); err != nil {
		return nil, true
	}
	return DeduplicateItems(items), true
}

func jsonObjectEnd(text string, start int) int {
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(text); index++ {
		char := text[index]
		if inString {
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == '"' {
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return index
			}
		}
	}
	return -1
}

func categoryNameAndLevel(body string, namePattern *regexp.Regexp) (string, string) {
	// A second-level row contains an empty li-level1 placeholder before the
	// actual li-level2 element. Prefer a category row with visible text; if a
	// page only supplies title attributes, retain the first named row as a
	// fallback. This also keeps a parent row ahead of nested child rows.
	fallbackName, fallbackLevel := "", ""
	for _, match := range namePattern.FindAllStringSubmatch(body, -1) {
		name := attributeValue(match[1], "title")
		content := textContent(match[2])
		if name == "" {
			name = content
		}
		if name == "" {
			continue
		}
		level := categoryLevel(match[1])
		if fallbackName == "" {
			fallbackName, fallbackLevel = name, level
		}
		if strings.TrimSpace(content) != "" {
			return name, level
		}
	}
	return fallbackName, fallbackLevel
}

func matchingDivEnd(text string, bodyStart int, divPattern *regexp.Regexp) int {
	depth := 1
	for _, match := range divPattern.FindAllStringIndex(text[bodyStart:], -1) {
		tag := strings.TrimSpace(text[bodyStart+match[0] : bodyStart+match[1]])
		if strings.HasPrefix(tag, "</") {
			depth--
			if depth == 0 {
				return bodyStart + match[0]
			}
			continue
		}
		if !strings.HasSuffix(strings.TrimSuffix(tag, ">"), "/") {
			depth++
		}
	}
	return len(text)
}

func categoryLevelNumber(level string) int {
	switch level {
	case "level1":
		return 1
	case "level2":
		return 2
	default:
		return 0
	}
}

func categoryLevel(attrs string) string {
	class := strings.ToLower(attributeValue(attrs, "class"))
	if strings.Contains(class, "li-level1") {
		return "level1"
	}
	if strings.Contains(class, "li-level2") {
		return "level2"
	}
	return ""
}

func attributeValue(attrs, name string) string {
	pattern := regexp.MustCompile(`(?is)\b` + regexp.QuoteMeta(name) + `\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>]+))`)
	match := pattern.FindStringSubmatch(attrs)
	if len(match) != 4 {
		return ""
	}
	for _, value := range match[1:] {
		if value != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func textContent(value string) string {
	value = regexp.MustCompile(`(?is)<[^>]+>`).ReplaceAllString(value, " ")
	return strings.Join(strings.Fields(html.UnescapeString(value)), " ")
}

func hasCurrentClass(attrs string) bool {
	return regexp.MustCompile(`(?is)\bclass\s*=\s*["'][^"']*\bcurrent\b[^"']*["']`).MatchString(attrs)
}

func bytesLooksLikeLogin(data []byte) bool {
	text := strings.ToLower(string(data))
	return regexp.MustCompile(`(?is)<input\b[^>]*\bname\s*=\s*["']?login["']?\b`).MatchString(text)
}
