package sui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"
)

func TestParseBooks(t *testing.T) {
	metadata := ParseBooks([]byte(`<ul class="s-accountbook-scroll" style="top: 0px"><li title='自己账本' data-bookId="1" class='current'><span class="b-wrap"><span class="no-char-num">自己账本</span></span></li><li title='自己账本' data-bookId="1" class=''><span class="b-wrap"><span class="no-char-num">自己账本</span></span></li><li title='工作' data-bookId="2" class=''><span class="b-wrap"><span class="no-char-num">工作</span></span></li></ul><script>ssjBookSwitch_param.currentBookId = "1"</script>`))
	if metadata.CurrentBook.ID != "1" || len(metadata.Books) != 2 || metadata.Books[1].Name != "工作" {
		t.Fatalf("unexpected metadata: %+v", metadata)
	}
}

func TestParseCategoryItems(t *testing.T) {
	items := ParseCategoryItems([]byte(`<div id="category100" class="lb-row"><ul><li class="li-level1" title="日常">日常</li></ul></div><div id="category123" class="lb-row"><ul><li class="li-level2" title="食品零食">食品零食</li><li class="li-tj">176689.40</li></ul></div><div id="category123" class="lb-row"><ul><li class="li-level2" title="食品零食">食品零食</li></ul></div>`), "expense")
	if len(items) != 2 || items[0].Level != 1 || items[1].ID != "123" || items[1].Name != "食品零食" || items[1].ParentID != "100" || items[1].ParentName != "日常" || items[1].Level != 2 || items[1].Type != "expense" || !items[1].Leaf || strings.Contains(items[1].Name, "176689.40") {
		t.Fatalf("unexpected category items: %+v", items)
	}
}

func TestParseCategoryItemsFindsNestedIncomeChildren(t *testing.T) {
	items := ParseCategoryItems([]byte(`<div id="category100" class="lb-row">
<ul><li class="li-level1" title="工资收入">工资收入</li></ul>
<div id="category101" class="lb-row"><ul><li class="li-level2" title="月薪">月薪</li></ul></div>
<div id="category102" class="lb-row"><ul><li class="li-level2" title="年终奖">年终奖</li></ul></div>
</div>`), "income")
	if len(items) != 3 {
		t.Fatalf("expected parent and two children, got %+v", items)
	}
	if items[0].Name != "工资收入" || items[0].Level != 1 || items[0].Leaf {
		t.Fatalf("unexpected income parent: %+v", items[0])
	}
	for i, expected := range []struct {
		id   string
		name string
	}{
		{id: "101", name: "月薪"},
		{id: "102", name: "年终奖"},
	} {
		item := items[i+1]
		if item.ID != expected.id || item.Name != expected.name || item.ParentID != "100" || item.ParentName != "工资收入" || item.Level != 2 || !item.Leaf || item.Type != "income" {
			t.Fatalf("unexpected income child: %+v", item)
		}
	}
}

func TestParseCategoryItemsSkipsEmptyLevel1Placeholder(t *testing.T) {
	items := ParseCategoryItems([]byte(`<div id="category200" class="list-msg"><ul>
<li class="li-level1">工作收入</li><li class="li-level2"></li>
</ul></div>
<div id="listBox200" class="list-box hidden">
<div id="category201" class="lb-row"><ul><li class="li-level1"></li><li class="li-level2">工资收入</li></ul></div>
<div id="category202" class="lb-row"><ul><li class="li-level1"></li><li class="li-level2">奖金收入</li></ul></div>
<div id="category203" class="lb-row"><ul><li class="li-level1" title="投资收入"></li><li class="li-level2"><span title="投资收入"></span>投资收入</li></ul></div>
</div>`), "income")
	if len(items) != 4 {
		t.Fatalf("expected one parent and three children, got %+v", items)
	}
	if items[0].ID != "200" || items[0].Name != "工作收入" || items[0].Level != 1 || items[0].Leaf {
		t.Fatalf("unexpected income parent: %+v", items[0])
	}
	for i, expected := range []string{"工资收入", "奖金收入", "投资收入"} {
		item := items[i+1]
		if item.Name != expected || item.Level != 2 || !item.Leaf || item.ParentID != "200" || item.ParentName != "工作收入" {
			t.Fatalf("unexpected income child: %+v", item)
		}
	}
}

func TestParseAccountItemsFromAccountPage(t *testing.T) {
	items := ParseAccountItems([]byte(`<div id="l-list"><a id="ll-1"><span class="ll-name">错误候选</span></a></div><script>var accountNameJson = {"22074919196":"总账","2121295030":"应付款项","2121295031":"应收款项","2121295032":"公司报销"};</script>`))
	if len(items) != 4 {
		t.Fatalf("expected four accounts, got %+v", items)
	}
	for i, expected := range []struct {
		id   string
		name string
	}{
		{id: "22074919196", name: "总账"},
		{id: "2121295030", name: "应付款项"},
		{id: "2121295031", name: "应收款项"},
		{id: "2121295032", name: "公司报销"},
	} {
		if items[i].ID != expected.id || items[i].Name != expected.name || items[i].Type != "account" || !items[i].Leaf {
			t.Fatalf("unexpected account: %+v", items[i])
		}
	}
}

func TestParseAccountItemsRequiresAccountNameJSON(t *testing.T) {
	items := ParseAccountItems([]byte(`<a id="ll-1"><span class="ll-name">错误候选</span></a>`))
	if len(items) != 0 {
		t.Fatalf("unexpected account candidates without accountNameJson: %+v", items)
	}
}

func TestParseAccountItemsFromAccountNameJSON(t *testing.T) {
	items := ParseAccountItems([]byte(`var accountNameJson = {
"22074919196": "总账",
"2121295030": "应付款项",
"2121295031": "应收款项",
"2121295032": "公司报销"
};`))
	if len(items) != 4 {
		t.Fatalf("unexpected account count: %+v", items)
	}
	for index, expected := range []struct{ id, name string }{
		{"22074919196", "总账"}, {"2121295030", "应付款项"}, {"2121295031", "应收款项"}, {"2121295032", "公司报销"},
	} {
		if items[index].ID != expected.id || items[index].Name != expected.name || items[index].Type != "account" || !items[index].Leaf {
			t.Fatalf("unexpected account at %d: %+v", index, items[index])
		}
	}
}

func TestValidateLeafCategory(t *testing.T) {
	metadata := Metadata{}
	metadata.Categories.Income = []Item{
		{ID: "100", Name: "工资收入", Type: "income", Level: 1},
		{ID: "101", Name: "月薪", Type: "income", Level: 2, ParentID: "100", ParentName: "工资收入", Leaf: true},
	}
	if err := ValidateLeafCategory(metadata, "income", "月薪"); err != nil {
		t.Fatalf("expected leaf category to pass: %v", err)
	}
	if err := ValidateLeafCategory(metadata, "income", "工资收入"); !errors.Is(err, ErrCategoryNotLeaf) {
		t.Fatalf("expected parent category rejection, got %v", err)
	}
	if err := ValidateLeafCategory(metadata, "income", "不存在"); !errors.Is(err, ErrCategoryNotFound) {
		t.Fatalf("expected missing category rejection, got %v", err)
	}
}

func TestBytesLooksLikeLoginIgnoresNormalLogoutLink(t *testing.T) {
	if bytesLooksLikeLogin([]byte(`<script>var logoutURL = "https://login.sui.com/"</script>`)) {
		t.Fatal("normal page logout link was classified as login page")
	}
	if !bytesLooksLikeLogin([]byte(`<form><input type="text" name="login"></form>`)) {
		t.Fatal("login form was not classified as login page")
	}
}

func TestWriteDoesNotRetryAndParsesSuccess(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `<li data-bookId="1" class="current">自己账本</li>`
		if r.URL.Path == "/tally/payout.rmi" {
			body = `{"id":12345}`
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})
	client := testClient(t, "https://sui.test", "自己账本", transport)
	result, err := client.Write(context.Background(), WriteInput{Type: "expense", CategoryID: "2", AccountID: "3", When: "2026-09-09 12:00", Amount: "35.00"})
	if err != nil || result.UpstreamID != "12345" {
		t.Fatalf("unexpected write result: %+v err=%v", result, err)
	}
}

func TestRequestsHaveBoundedDeadlines(t *testing.T) {
	for _, preview := range []bool{false, true} {
		name := "normal request"
		if preview {
			name = "debug preview"
		}
		t.Run(name, func(t *testing.T) {
			client := testClient(t, "https://sui.test", "自己账本", roundTripFunc(func(r *http.Request) (*http.Response, error) {
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 30*time.Second {
					t.Fatalf("request has no bounded upstream deadline: deadline=%v present=%v", deadline, ok)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":12345}`)), Header: make(http.Header), Request: r}, nil
			}))
			var err error
			if preview {
				_, err = client.ExecuteRequestPreview(context.Background(), RequestPreview{Method: http.MethodPost, URL: "https://sui.test/tally/payout.rmi"})
			} else {
				_, err = client.Get(context.Background(), "/report_index.do", nil)
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWriteDeadlineDoesNotRetryOrOverrideCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	callerDeadline, _ := ctx.Deadline()
	writes := 0
	client := testClient(t, "https://sui.test", "自己账本", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || !deadline.Equal(callerDeadline) {
			t.Fatalf("caller deadline changed: got=%v want=%v", deadline, callerDeadline)
		}
		if r.URL.Path == "/tally/payout.rmi" {
			writes++
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		body := `<li data-bookId="1" class="current">自己账本</li>`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	}))
	_, err := client.Write(ctx, WriteInput{Type: "expense", Amount: "35"})
	if !errors.Is(err, ErrUncertain) || writes != 1 {
		t.Fatalf("timed-out write was not kept uncertain without retry: err=%v writes=%d", err, writes)
	}
}

func TestWriteParsesNestedSuccessIDAndPrice(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `<li data-bookId="1" class="current">自己账本</li>`
		if r.URL.Path == "/tally/payout.rmi" {
			r.ParseForm()
			if _, ok := r.PostForm["debt"]; ok {
				t.Fatalf("ordinary payout payload contains debt: %q", r.PostForm.Encode())
			}
			if r.PostForm.Get("price") != "1234.5" {
				t.Fatalf("unexpected payout price: %q", r.PostForm.Get("price"))
			}
			body = `{"id":{"id":21693739113481},"budget":0,"price":1234.5}`
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})
	client := testClient(t, "https://sui.test", "自己账本", transport)
	result, err := client.Write(context.Background(), WriteInput{Type: "expense", CategoryID: "2", AccountID: "3", When: "2026-09-10 12:00", Amount: "1234.5"})
	if err != nil {
		t.Fatalf("nested success response was rejected: %v", err)
	}
	if result.UpstreamID != "21693739113481" || result.Budget != "0" || result.Price != "1234.5" {
		t.Fatalf("unexpected nested write result: %+v", result)
	}
}

func TestWriteResponseFormatsAndFailureBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, body, id string
		wantErr        error
	}{
		{"observed object literal", `{id:{id:21693739113481},price:0.01}`, "21693739113481", nil},
		{"whitespace", `{ id : { id : "21693739113481" }, budget : 0, price : 0.01 }`, "21693739113481", nil},
		{"legacy literal", `{id:12345}`, "12345", nil},
		{"session expired", `{id:-1}`, "", ErrSessionExpired},
		{"zero", `{id:0}`, "", ErrUncertain},
		{"unknown negative", `{id:-2}`, "", ErrUncertain},
		{"missing", `{price:0.01}`, "", ErrUncertain},
		{"truncated", `{id:{id:12345}`, "", ErrUncertain},
		{"script", `{id:{id:12345}};alert(1)`, "", ErrUncertain},
		{"expression", `{id:12345+1}`, "", ErrUncertain},
		{"null", `{id:null}`, "", ErrUncertain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			client := testClient(t, "https://sui.test", "自己账本", roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := `<li data-bookId="1" class="current">自己账本</li>`
				if r.URL.Path == "/tally/payout.rmi" {
					writes++
					body = tc.body
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
			}))
			result, err := client.Write(context.Background(), WriteInput{Type: "expense"})
			if !errors.Is(err, tc.wantErr) || result.UpstreamID != tc.id || writes != 1 {
				t.Fatalf("result=%+v err=%v writes=%d", result, err, writes)
			}
		})
	}
	const info = `分类错误,price:12,{id:99},包含"引号"`
	result, err := parseWriteResult([]byte(`{id:-999,errorInfo:"分类错误,price:12,{id:99},包含\"引号\""}`))
	if err != nil || result.ID != -999 || result.ErrorInfo != info {
		t.Fatalf("rejection text changed: %+v err=%v", result, err)
	}
}

func TestWriteRequestOmitsDebtForOrdinaryTransactions(t *testing.T) {
	_, form := writeRequest(WriteInput{Type: "expense", CategoryID: "2", AccountID: "3", Amount: "35"})
	if _, ok := form["debt"]; ok {
		t.Fatalf("ordinary transaction form must not contain debt: %q", form.Encode())
	}
}

func TestListAcceptsNumericTransactionIDs(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"groups":[{"list":[{"tranId":21693739113481,"tranType":"payout","itemAmount":79.1,"categoryId":23551923833,"buyerAcountId":22074919196,"date":"2026-09-09 20:24"}]}]}`
		if r.URL.Path == "/report_index.do" {
			body = `<li title='自己账本' data-bookId="1" class='current'>自己账本</li>`
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})
	client := testClient(t, "https://sui.test", "自己账本", transport)
	transactions, err := client.List(context.Background(), "2026.09.09", "2026.09.10")
	if err != nil || len(transactions) != 1 {
		t.Fatalf("numeric transaction response was not parsed: %+v err=%v", transactions, err)
	}
	transaction := transactions[0]
	if transaction.ID != "21693739113481" || transaction.Amount != "79.1" || transaction.CategoryID != "23551923833" || transaction.AccountID != "22074919196" {
		t.Fatalf("unexpected normalized transaction: %+v", transaction)
	}
}

func TestCheckBookRejectsAmbiguousName(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `<li data-bookId="1" class="current">共同账本</li><li data-bookId="2">共同账本</li>`
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})
	client := testClient(t, "https://sui.test", "共同账本", transport)
	if _, err := client.CheckBook(context.Background()); !errors.Is(err, ErrBookNameAmbiguous) {
		t.Fatalf("expected ambiguous book name, got %v", err)
	}
}

func TestSyncMetadataFetchesCategoriesAfterBookCheck(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/report_index.do":
			body = `<li title='自己账本' data-bookId="1" class='current'><span>自己账本</span></li>`
		case "/category/budgetCategory.do":
			body = `<div id="category123"><ul><li class="li-level2" title="食品">食品</li></ul></div>`
		case "/category/incomeCategory.do":
			body = `<div id="category456"><ul><li class="li-level2" title="工资">工资</li></ul></div>`
		case "/category/storeCategory.do":
			body = `<div id="category789"><span title="餐馆">餐馆</span></div>`
		case "/account/account.do":
			body = `<script>var accountNameJson = {"1":"现金账户","2":"金融账户"};</script>`
		case "/category/projectCategory.do":
			body = `<div id="category791"><span title="旅行">旅行</span></div>`
		case "/category/member.do":
			body = `<div id="category792"><span title="我">我</span></div>`
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})
	client := testClient(t, "https://sui.test", "自己账本", transport)
	metadata, err := client.SyncMetadata(context.Background())
	if err != nil || len(metadata.Categories.Expense) != 1 || len(metadata.Categories.Income) != 1 || len(metadata.Accounts) != 2 || metadata.Accounts[0].Name != "现金账户" || metadata.Accounts[1].Name != "金融账户" {
		t.Fatalf("unexpected synced metadata: %+v err=%v", metadata, err)
	}
}

func TestSyncMetadataSwitchesToRequestedBook(t *testing.T) {
	reportCalls := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/report_index.do":
			reportCalls++
			if reportCalls == 1 {
				body = `<li title='自己账本' data-bookId="1" class='current'>自己账本</li><li title='旅游' data-bookId="2">旅游</li>`
			} else {
				body = `<li title='自己账本' data-bookId="1">自己账本</li><li title='旅游' data-bookId="2" class='current'>旅游</li>`
			}
		case "/systemSet/book.do":
			if r.URL.Query().Get("opt") != "switch" || r.URL.Query().Get("switchId") != "2" || r.URL.Query().Get("return") != "https://sui.test/category/budgetCategory.do" {
				return nil, errors.New("unexpected book switch request: " + r.URL.String())
			}
			body = "ok"
		case "/category/budgetCategory.do":
			body = `<div id="category123"><ul><li class="li-level2" title="食品">食品</li></ul></div>`
		case "/category/incomeCategory.do":
			body = `<div id="category456"><ul><li class="li-level2" title="工资">工资</li></ul></div>`
		case "/category/storeCategory.do":
			body = `<div id="category789"><span title="餐馆">餐馆</span></div>`
		case "/account/account.do":
			body = `<div id="l-list"><div class="l-list-in"><a id="ll-1" class="choose gg g-cashaccount-g"><span class="ll-info"><span class="ll-money" title="0.00">0.00</span><span class="ll-name">现金账户</span></span></a></div></div>`
		case "/category/projectCategory.do":
			body = `<div id="category791"><span title="旅行">旅行</span></div>`
		case "/category/member.do":
			body = `<div id="category792"><span title="我">我</span></div>`
		default:
			return nil, errors.New("unexpected request after book switch: " + r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})
	client := testClient(t, "https://sui.test", "旅游", transport)
	metadata, err := client.SyncMetadata(context.Background())
	if err != nil || metadata.CurrentBook.Name != "旅游" || len(metadata.Categories.Expense) != 1 {
		t.Fatalf("expected switched metadata, got %+v err=%v", metadata, err)
	}
	if reportCalls != 2 {
		t.Fatalf("expected two book list checks, got %d", reportCalls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testClient(t *testing.T, baseURL, bookName string, transport http.RoundTripper) Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return Client{BaseURL: baseURL, BookName: bookName, Jar: jar, HTTP: &http.Client{Transport: transport}, MinInterval: time.Nanosecond}
}
