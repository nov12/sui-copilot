package sui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoginWithClientUsesKeepSignedInFlowAndPersistsJar(t *testing.T) {
	sessionPath := t.TempDir() + "/sui-session.bin"
	keyPath := t.TempDir() + "/sui-session.key"
	session, err := NewSession(sessionPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}

	const email = "user@example.com"
	const password = "secret"
	const vccode = "vccode"
	const expectedPasswordHash = "6312b48eb4a27ff1f4629985ffadb45097cb8ec3"
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := ""
		header := make(http.Header)
		status := http.StatusOK
		switch r.URL.Path {
		case "/login.do":
			if r.URL.Query().Get("opt") == "vccode" {
				body = `{"vccode":"vccode","uid":"uid"}`
				header.Add("Set-Cookie", "login=1; Domain=login.sui.com; Path=/")
				break
			}
			if r.URL.Query().Get("status") != "1" || r.URL.Query().Get("email") != email || r.URL.Query().Get("password") != expectedPasswordHash || r.URL.Query().Get("uid") != "uid" {
				t.Fatalf("unexpected login query: %s", r.URL.RawQuery)
			}
			body = `{"status":"ok"}`
			header.Add("Set-Cookie", "session=secret; Domain=.sui.com; Path=/; HttpOnly")
		case "/auth.do":
			if r.URL.RawQuery != "" {
				t.Fatalf("unexpected auth query: %q", r.URL.RawQuery)
			}
			body = `<html><body onload="document.forms[0].submit();"><form method="POST" action="/auth-complete"><input type="hidden" name="token" value="abc"></form></body></html>`
		case "/auth-complete":
			if r.Method != http.MethodPost || r.FormValue("token") != "abc" {
				t.Fatalf("unexpected auth completion request: method=%s token=%q", r.Method, r.FormValue("token"))
			}
			header.Add("Set-Cookie", "session=secret; Domain=.sui.com; Path=/; HttpOnly")
			body = `authenticated`
		case "/sso/redirect.do":
		case "/report_index.do":
			if !strings.Contains(r.Header.Get("Cookie"), "session=secret") {
				t.Fatalf("session cookie was not sent to report page: %q", r.Header.Get("Cookie"))
			}
			body = `<li title='自己账本' data-bookId="1" class='current'>自己账本</li>`
		case "/tally/new.do":
			body = `<html><body>tally form</body></html>`
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: status, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: header, Request: r}, nil
	})
	client := &http.Client{Jar: session.Jar, Transport: transport}
	metadata, err := loginWithClient(context.Background(), client, session, "https://www.sui.com", email, password)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.CurrentBook.ID != "1" || metadata.CurrentBook.Name != "自己账本" {
		t.Fatalf("unexpected metadata: %+v", metadata)
	}

	ciphertext, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), "secret") || strings.Contains(string(ciphertext), password) {
		t.Fatal("session file contains plaintext session data")
	}
	loaded, err := LoadSession(sessionPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	urlValue := mustParseURL(t, "https://www.sui.com/report_index.do")
	cookies := loaded.Jar.Cookies(urlValue)
	if len(cookies) != 1 || cookies[0].Name != "session" || cookies[0].Value != "secret" {
		t.Fatalf("unexpected restored cookies: %+v", cookies)
	}
}

func TestLoginPasswordHashMatchesWebsiteAlgorithm(t *testing.T) {
	if actual := loginPasswordHash("user@example.com", "secret", "vccode"); actual != "6312b48eb4a27ff1f4629985ffadb45097cb8ec3" {
		t.Fatalf("unexpected password hash: %s", actual)
	}
}

func TestGetLoginJSONAlsoAcceptsLegacyJSONP(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(`suiBridgeCallback({"status":"ok"})`)), Header: make(http.Header), Request: r}, nil
	})}
	var response loginResponse
	if err := getLoginJSON(context.Background(), client, "https://login.sui.com/login.do", nil, &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "ok" {
		t.Fatalf("unexpected JSONP response: %+v", response)
	}
}

func TestParseAuthFormAcceptsUnquotedHTMLAttributes(t *testing.T) {
	data := []byte(`<html><body onload="document.forms[0].submit()"><form action="https://www.sui.com/report_index.do" method=POST><input type=hidden name="su" value="token"><input type=hidden name="iv" value="nonce"></form></body></html>`)
	baseURL := mustParseURL(t, "https://login.sui.com/auth.do")

	method, action, form, ok, err := parseAuthForm(data, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || method != http.MethodPost || action != "https://www.sui.com/report_index.do" {
		t.Fatalf("unexpected auth form: ok=%t method=%q action=%q", ok, method, action)
	}
	if form.Get("su") != "token" || form.Get("iv") != "nonce" {
		t.Fatalf("unexpected auth form fields: %v", form)
	}
}

func TestLoadSessionRejectsMissingFile(t *testing.T) {
	_, err := LoadSession(t.TempDir()+"/missing.bin", t.TempDir()+"/missing.key")
	if !os.IsNotExist(err) {
		t.Fatalf("expected missing session error, got %v", err)
	}
}

func TestClientUsesSessionJarAndSavesUpdatedCookies(t *testing.T) {
	sessionPath := t.TempDir() + "/sui-session.bin"
	keyPath := t.TempDir() + "/sui-session.key"
	session, err := NewSession(sessionPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.Header.Get("Cookie"), "session=secret") {
			t.Fatalf("session cookie was not sent: %q", r.Header.Get("Cookie"))
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header), Request: r}, nil
	})
	session.Jar.SetCookies(mustParseURL(t, "https://www.sui.com/report_index.do"), []*http.Cookie{{Name: "session", Value: "secret", Path: "/", Domain: ".sui.com"}})
	client := Client{BaseURL: "https://www.sui.com", Jar: session.Jar, Session: session, HTTP: &http.Client{Transport: transport}, MinInterval: time.Nanosecond}
	if _, err := client.Get(context.Background(), "/report_index.do", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("session was not saved: %v", err)
	}
}

func mustParseURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestClientRestoresExpiredWebSessionFromSSO(t *testing.T) {
	sessionPath := t.TempDir() + "/session.bin"
	keyPath := t.TempDir() + "/session.key"
	session, err := NewSession(sessionPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	session.Jar.SetCookies(mustParseURL(t, loginBaseURL), []*http.Cookie{{Name: "remember", Value: "long-lived", Path: "/"}})
	reports, exchanges := 0, 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		header := make(http.Header)
		body := ""
		status := http.StatusOK
		switch r.URL.Host + r.URL.Path {
		case "www.sui.com/report_index.do":
			if r.Method == http.MethodPost {
				exchanges++
				if r.FormValue("su") != "token" || r.Header.Get("User-Agent") != httpUserAgent {
					t.Fatal("invalid SSO exchange")
				}
				header.Add("Set-Cookie", "web=fresh; Path=/; HttpOnly")
				body = `<li title='自己账本' data-bookId="1" class='current'>自己账本</li>`
			} else {
				reports++
				if c, err := r.Cookie("web"); err == nil && c.Value == "fresh" {
					body = `<li title='自己账本' data-bookId="1" class='current'>自己账本</li>`
				} else {
					status = 302
					header.Set("Location", loginBaseURL+"/auth.do")
				}
			}
		case "login.sui.com/auth.do":
			if _, err := r.Cookie("remember"); err != nil {
				t.Fatal("long-lived cookie missing")
			}
			body = `<body onload="document.forms[0].submit()"><form method=POST action="https://www.sui.com/report_index.do"><input name="su" value="token"></form></body>`
		default:
			t.Fatalf("unexpected endpoint: %s%s", r.URL.Host, r.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	c := Client{BaseURL: "https://www.sui.com", BookName: "自己账本", Jar: session.Jar, Session: session, HTTP: &http.Client{Transport: transport}, MinInterval: time.Nanosecond}
	if _, err := c.CheckBook(context.Background()); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSession(sessionPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	c.Jar = loaded.Jar
	c.Session = loaded
	if _, err := c.CheckBook(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reports != 3 || exchanges != 1 {
		t.Fatalf("reports=%d exchanges=%d", reports, exchanges)
	}
	c.BookName = "other"
	if _, err := c.CheckBook(context.Background()); !errors.Is(err, ErrBookNotFound) {
		t.Fatalf("book validation bypassed: %v", err)
	}
}

func TestClientSSORecoveryBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, path, landing, action string
		form                        url.Values
		loginForm                   bool
	}{
		{name: "expired remember cookie", path: "/report_index.do", landing: "/auth.do", loginForm: true},
		{name: "login page", path: "/report_index.do", landing: "/login.jsp"},
		{name: "untrusted form", path: "/report_index.do", landing: "/auth.do", action: "https://other.example/report_index.do"},
		{name: "wrong target", path: "/report_index.do", landing: "/auth.do", action: "https://www.sui.com/tally/payout.rmi"},
		{name: "accounting write", path: "/tally/payout.rmi", landing: "/auth.do", form: url.Values{"price": {"1"}}},
		{name: "book switch GET", path: "/systemSet/book.do?opt=switch", landing: "/auth.do"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jar, _ := NewPersistentJar()
			requests := 0
			h := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				if requests > 1 {
					t.Fatal("unexpected replay or SSO exchange")
				}
				action := tc.action
				if action == "" {
					action = "https://www.sui.com/report_index.do"
				}
				body := `<body onload="document.forms[0].submit()"><form method=POST action="` + action + `"><input name="su" value="token"></form></body>`
				if tc.loginForm {
					body = `<input name="login">`
				}
				final := r.Clone(r.Context())
				final.URL = mustParseURL(t, loginBaseURL+tc.landing)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: final}, nil
			})}
			c := Client{BaseURL: "https://www.sui.com", Jar: jar, HTTP: h, MinInterval: time.Nanosecond}
			if _, err := c.Get(context.Background(), tc.path, tc.form); !errors.Is(err, ErrSessionExpired) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestReportSSOCompletesBothAutoSubmittedForms(t *testing.T) {
	jar, _ := NewPersistentJar()
	calls := 0
	c := Client{BaseURL: "https://www.sui.com", Jar: jar, MinInterval: time.Nanosecond}
	c.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		header := make(http.Header)
		status := 200
		body := ""
		switch calls {
		case 1:
			final := r.Clone(r.Context())
			final.URL = mustParseURL(t, loginBaseURL+"/auth.do")
			r = final
			body = `<body onload="document.forms[0].submit()"><form method=POST action="https://www.sui.com/sso/redirect.do"><input name="su" value="token"></form></body>`
		case 2:
			if r.Method != "POST" || r.URL.Path != "/sso/redirect.do" {
				t.Fatal("unexpected SSO callback")
			}
			body = `<body onload="document.forms[0].submit()"><form method=POST action="https://www.sui.com/report_index.do"><input name="su" value="token"></form></body>`
		case 3, 4:
			if (calls == 3 && r.Method != "POST") || (calls == 4 && r.Method != "GET") || r.URL.Path != "/report_index.do" {
				t.Fatal("unexpected report redirect")
			}
			body = `<li title='自己账本' data-bookId="1" class='current'>自己账本</li>`
		default:
			t.Fatal("SSO loop")
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	if _, err := c.ListBooks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestReportSSOLoopIsBounded(t *testing.T) {
	jar, _ := NewPersistentJar()
	calls := 0
	c := Client{BaseURL: "https://www.sui.com", Jar: jar, MinInterval: time.Nanosecond}
	c.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls > 6 {
			t.Fatal("SSO loop not bounded")
		}
		if calls == 1 {
			r = r.Clone(r.Context())
			r.URL = mustParseURL(t, loginBaseURL+"/auth.do")
		}
		body := `<body onload="document.forms[0].submit()"><form method=POST action="https://www.sui.com/sso/redirect.do"><input name="su" value="token"></form></body>`
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	if _, err := c.ListBooks(context.Background()); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("got %v", err)
	}
}

func TestClientReportsSessionSaveFailure(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSession(dir+"/missing/session.bin", dir+"/key")
	if err != nil {
		t.Fatal(err)
	}
	c := Client{BaseURL: "https://www.sui.com", Jar: s.Jar, Session: s, MinInterval: time.Nanosecond}
	c.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		h := make(http.Header)
		h.Add("Set-Cookie", "web=fresh; Path=/")
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
	})}
	if _, err := c.Get(context.Background(), "/report_index.do", nil); err == nil || !strings.Contains(err.Error(), "save sui session") {
		t.Fatalf("save failure hidden: %v", err)
	}
}
