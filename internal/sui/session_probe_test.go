package sui

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

// Opt-in remote diagnostic: never prints cookie values or response bodies.
func TestRemoteSessionProbe(t *testing.T) {
	dir := os.Getenv("SUI_PROBE_DIR")
	if dir == "" {
		t.Skip("remote diagnostic disabled")
	}
	s, err := LoadSession(dir+"/sui-session.bin", dir+"/sui-session.key")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Jar.snapshot() {
		t.Logf("cookie name=%s domain=%s path=%s expires=%s maxAge=%d", c.Name, c.Domain, c.Path, c.Expires.Format(time.RFC3339), c.MaxAge)
	}
	// Explicitly simulate loss of only the web session, keeping login cookies.
	if os.Getenv("SUI_PROBE_EXPIRE_WEB") == "1" {
		for _, c := range s.Jar.snapshot() {
			u := mustParseURL(t, c.URL)
			if u.Hostname() == "www.sui.com" && c.Domain == "" {
				s.Jar.SetCookies(u, []*http.Cookie{{Name: c.Name, Path: c.Path, MaxAge: -1}})
				t.Logf("simulated web session expiry: %s", c.Name)
			}
		}
	}
	h := &http.Client{Jar: s.Jar, Timeout: 20 * time.Second, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err == nil {
			t.Logf("HTTP %s %s%s -> %d", r.Method, r.URL.Host, r.URL.Path, resp.StatusCode)
			if (r.URL.Hostname() == "login.sui.com" && r.URL.Path == "/auth.do") || r.URL.Path == "/sso/redirect.do" {
				body, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				resp.Body = io.NopCloser(bytes.NewReader(body))
				if readErr != nil {
					t.Fatal(readErr)
				}
				method, action, _, ok, parseErr := parseAuthForm(body, r.URL)
				target, _ := url.Parse(action)
				t.Logf("SSO form found=%t method=%s target=%s://%s%s loginForm=%t parseErr=%v", ok, method, target.Scheme, target.Host, target.Path, bytesLooksLikeLogin(body), parseErr)
			}
		}
		return resp, err
	})}
	ctx := context.Background()
	c := Client{BaseURL: "https://www.sui.com", Jar: s.Jar, Session: s, HTTP: h, MinInterval: time.Nanosecond}
	m, err := c.ListBooks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("bridge read: bookFound=%t", m.CurrentBook.ID != "")
	loaded, err := LoadSession(dir+"/sui-session.bin", dir+"/sui-session.key")
	if err != nil {
		t.Fatal(err)
	}
	c.Jar = loaded.Jar
	c.Session = loaded
	if _, err := c.ListBooks(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("reload and read succeeded")
}
