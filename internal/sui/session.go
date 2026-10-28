package sui

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"sui-copilot/internal/security"
)

const loginBaseURL = "https://login.sui.com"

// The legacy web endpoint expects a normal browser-style User-Agent. Keep it
// fixed for protocol compatibility; never rotate it as a way to evade risk
// controls.
const httpUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

var (
	ErrLoginFailed           = errors.New("sui login failed")
	ErrSessionNotConfigured  = errors.New("sui session is not configured")
	ErrSessionStoreCorrupted = errors.New("sui session store is corrupted")
)

// Session keeps the browser-like cookie jar used by the lightweight HTTP
// login flow. Passwords are never part of Session and are only held by the
// caller while Login is running.
type Session struct {
	Jar *PersistentJar

	path        string
	key         []byte
	mu          sync.Mutex
	requestMu   sync.Mutex
	nextRequest time.Time
}

// storedCookie is the minimal cookie representation needed to restore a
// cookie jar across process restarts. URL records the origin for host-only
// cookies; Domain is preserved for domain cookies shared by Sui subdomains.
type storedCookie struct {
	URL        string        `json:"url"`
	Name       string        `json:"name"`
	Value      string        `json:"value"`
	Path       string        `json:"path,omitempty"`
	Domain     string        `json:"domain,omitempty"`
	Expires    time.Time     `json:"expires,omitempty"`
	Raw        string        `json:"raw,omitempty"`
	RawExpires string        `json:"rawExpires,omitempty"`
	MaxAge     int           `json:"maxAge,omitempty"`
	Secure     bool          `json:"secure,omitempty"`
	HttpOnly   bool          `json:"httpOnly,omitempty"`
	SameSite   http.SameSite `json:"sameSite,omitempty"`
}

// PersistentJar delegates cookie matching to net/http's standard cookie jar
// while recording received cookies for encrypted persistence.
type PersistentJar struct {
	jar *cookiejar.Jar

	mu      sync.Mutex
	cookies map[string]storedCookie
	dirty   bool
}

func NewPersistentJar() (*PersistentJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &PersistentJar{jar: jar, cookies: make(map[string]storedCookie)}, nil
}

func (j *PersistentJar) Cookies(u *url.URL) []*http.Cookie {
	if j == nil || j.jar == nil {
		return nil
	}
	return j.jar.Cookies(u)
}

func (j *PersistentJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if j == nil || j.jar == nil || u == nil {
		return
	}
	j.jar.SetCookies(u, cookies)

	now := time.Now()
	host := strings.ToLower(u.Hostname())
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, cookie := range cookies {
		if cookie == nil || cookie.Name == "" {
			continue
		}
		domain := strings.ToLower(strings.TrimSpace(cookie.Domain))
		if domain == "" {
			domain = host
		}
		key := cookieKey(domain, cookie.Path, cookie.Name)
		if cookie.MaxAge < 0 || (!cookie.Expires.IsZero() && cookie.Expires.Before(now)) {
			delete(j.cookies, key)
			j.dirty = true
			continue
		}
		j.cookies[key] = storedCookie{
			URL:        u.Scheme + "://" + u.Host,
			Name:       cookie.Name,
			Value:      cookie.Value,
			Path:       cookie.Path,
			Domain:     cookie.Domain,
			Expires:    cookie.Expires,
			Raw:        cookie.Raw,
			RawExpires: cookie.RawExpires,
			MaxAge:     cookie.MaxAge,
			Secure:     cookie.Secure,
			HttpOnly:   cookie.HttpOnly,
			SameSite:   cookie.SameSite,
		}
		j.dirty = true
	}
}

func cookieKey(domain, path, name string) string {
	return domain + "\x00" + path + "\x00" + name
}

func (j *PersistentJar) snapshot() []storedCookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	result := make([]storedCookie, 0, len(j.cookies))
	for _, cookie := range j.cookies {
		result = append(result, cookie)
	}
	return result
}

func (j *PersistentJar) markClean() {
	j.mu.Lock()
	j.dirty = false
	j.mu.Unlock()
}

func (j *PersistentJar) isDirty() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.dirty
}

// NewSession creates a new encrypted session store. It does not perform a
// network request.
func NewSession(sessionPath, keyPath string) (*Session, error) {
	if strings.TrimSpace(sessionPath) == "" || strings.TrimSpace(keyPath) == "" {
		return nil, ErrSessionNotConfigured
	}
	key, err := security.LoadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	jar, err := NewPersistentJar()
	if err != nil {
		return nil, err
	}
	return &Session{Jar: jar, path: sessionPath, key: key}, nil
}

// LoadSession loads an existing encrypted session. os.ErrNotExist is
// returned when the user has not completed auth login yet.
func LoadSession(sessionPath, keyPath string) (*Session, error) {
	if strings.TrimSpace(sessionPath) == "" || strings.TrimSpace(keyPath) == "" {
		return nil, ErrSessionNotConfigured
	}
	ciphertext, err := os.ReadFile(sessionPath)
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	plaintext, err := security.Open(key, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("%w: decrypt session", ErrSessionStoreCorrupted)
	}
	var cookies []storedCookie
	if err := json.Unmarshal(plaintext, &cookies); err != nil {
		return nil, fmt.Errorf("%w: decode session", ErrSessionStoreCorrupted)
	}
	jar, err := NewPersistentJar()
	if err != nil {
		return nil, err
	}
	for _, stored := range cookies {
		u, err := url.Parse(stored.URL)
		if err != nil || u.Scheme == "" || u.Host == "" || stored.Name == "" {
			return nil, fmt.Errorf("%w: invalid cookie origin", ErrSessionStoreCorrupted)
		}
		cookie := &http.Cookie{
			Name:       stored.Name,
			Value:      stored.Value,
			Path:       stored.Path,
			Domain:     stored.Domain,
			Expires:    stored.Expires,
			Raw:        stored.Raw,
			RawExpires: stored.RawExpires,
			MaxAge:     stored.MaxAge,
			Secure:     stored.Secure,
			HttpOnly:   stored.HttpOnly,
			SameSite:   stored.SameSite,
		}
		jar.jar.SetCookies(u, []*http.Cookie{cookie})
		key := cookie.Domain
		if key == "" {
			key = u.Hostname()
		}
		jar.cookies[cookieKey(strings.ToLower(key), cookie.Path, cookie.Name)] = stored
	}
	jar.markClean()
	return &Session{Jar: jar, path: sessionPath, key: key}, nil
}

// Save encrypts the complete cookie snapshot with an owner-only key and
// atomically replaces the session file.
func (s *Session) Save() error {
	if s == nil || s.Jar == nil || len(s.key) != 32 {
		return ErrSessionNotConfigured
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	plaintext, err := json.Marshal(s.Jar.snapshot())
	if err != nil {
		return err
	}
	ciphertext, err := security.Seal(s.key, plaintext)
	if err != nil {
		return err
	}
	directory := filepath.Dir(s.path)
	file, err := os.CreateTemp(directory, ".sui-session-*")
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
		_ = file.Close()
		return err
	}
	if _, err := file.Write(ciphertext); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return err
	}
	keepTemporary = true
	s.Jar.markClean()
	return nil
}

func (s *Session) SaveIfDirty() error {
	if s == nil || s.Jar == nil || !s.Jar.isDirty() {
		return nil
	}
	return s.Save()
}

func (s *Session) wait(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	s.requestMu.Lock()
	wait := time.Until(s.nextRequest)
	if wait <= 0 {
		s.nextRequest = time.Now().Add(interval)
		s.requestMu.Unlock()
		return nil
	}
	s.nextRequest = time.Now().Add(wait + interval)
	s.requestMu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type loginVccode struct {
	Vccode string `json:"vccode"`
	UID    string `json:"uid"`
}

type loginResponse struct {
	Status string `json:"status"`
}

// Login performs the same lightweight login handshake used by the current
// login.sui.com page. status=1 is the website's "keep me signed in" option.
// The password is hashed before transmission and is never stored.
func Login(ctx context.Context, baseURL, email, password, sessionPath, keyPath string) (Metadata, error) {
	email = strings.TrimSpace(email)
	password = strings.TrimSpace(password)
	if email == "" || password == "" {
		return Metadata{}, ErrLoginFailed
	}
	session, err := NewSession(sessionPath, keyPath)
	if err != nil {
		return Metadata{}, err
	}
	client := &http.Client{Jar: session.Jar, Timeout: 20 * time.Second}
	return loginWithClient(ctx, client, session, baseURL, email, password)
}

func loginWithClient(ctx context.Context, client *http.Client, session *Session, baseURL, email, password string) (Metadata, error) {
	vccode := loginVccode{}
	if err := getLoginJSON(ctx, client, loginBaseURL+"/login.do", url.Values{"opt": {"vccode"}}, &vccode); err != nil {
		return Metadata{}, err
	}
	if vccode.Vccode == "" || vccode.UID == "" {
		return Metadata{}, ErrLoginFailed
	}

	hash := loginPasswordHash(email, password, vccode.Vccode)
	response := loginResponse{}
	if err := getLoginJSON(ctx, client, loginBaseURL+"/login.do", url.Values{
		"email":    {email},
		"status":   {"1"},
		"password": {hash},
		"uid":      {vccode.UID},
	}, &response); err != nil {
		return Metadata{}, err
	}
	if response.Status != "ok" {
		return Metadata{}, ErrLoginFailed
	}

	// The legacy SSO endpoint selects its destination from the authenticated
	// server-side session. The established lightweight client calls auth.do
	// without adding a second returnUrl query; keep that wire format here.
	authURL := loginBaseURL + "/auth.do"
	referer := loginBaseURL + "/login.jsp?returnUrl=" + url.QueryEscape(strings.TrimRight(baseURL, "/")+"/sso/redirect.do")
	if err := visit(ctx, client, authURL, referer); err != nil {
		return Metadata{}, err
	}

	metadata, err := listBooksWithClient(ctx, client, baseURL)
	if err != nil {
		return Metadata{}, err
	}
	if _, err := getWithClient(ctx, client, baseURL, "/tally/new.do"); err != nil {
		return Metadata{}, err
	}
	if err := session.Save(); err != nil {
		return Metadata{}, err
	}
	return metadata, nil
}

func loginPasswordHash(email, password, vccode string) string {
	passwordHash := sha1Hex(password)
	passwordHash = sha1Hex(email + passwordHash)
	return sha1Hex(passwordHash + vccode)
}

func sha1Hex(value string) string {
	sum := sha1.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}

func getLoginJSON(ctx context.Context, client *http.Client, endpoint string, query url.Values, target any) error {
	if query == nil {
		query = make(url.Values)
	}
	data, _, err := requestURL(ctx, client, endpoint+"?"+query.Encode())
	if err != nil {
		return err
	}
	text := strings.TrimSpace(string(data))
	if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
		if err := json.Unmarshal([]byte(text), target); err != nil {
			return fmt.Errorf("%w: decode JSON response", ErrLoginFailed)
		}
		return nil
	}
	start := strings.IndexByte(text, '(')
	end := strings.LastIndexByte(text, ')')
	if start < 1 || end <= start {
		return fmt.Errorf("%w: invalid JSONP response", ErrLoginFailed)
	}
	if err := json.Unmarshal([]byte(text[start+1:end]), target); err != nil {
		return fmt.Errorf("%w: decode JSONP response", ErrLoginFailed)
	}
	return nil
}

func visit(ctx context.Context, client *http.Client, endpoint, referer string) error {
	method := http.MethodGet
	currentURL := endpoint
	form := url.Values(nil)
	currentReferer := referer
	for jump := 0; jump <= 5; jump++ {
		headers := make(http.Header)
		if currentReferer != "" {
			headers.Set("Referer", currentReferer)
		}
		headers.Set("User-Agent", httpUserAgent)
		data, resp, err := requestMethod(ctx, client, method, currentURL, headers, form)
		if err != nil {
			return err
		}
		if resp == nil || resp.Request == nil {
			return errors.New("sui auth response has no request URL")
		}
		nextMethod, nextURL, nextForm, ok, err := parseAuthForm(data, resp.Request.URL)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		method = nextMethod
		currentURL = nextURL
		form = nextForm
		currentReferer = resp.Request.URL.String()
	}
	return errors.New("sui auth redirect limit exceeded")
}

// completeReportSSO completes the browser's auto-submitted SSO form when a
// short-lived web session expires but the long-lived login cookie still works.
// It only accepts the known login endpoint and the configured HTTPS SSO callback or report
// landing page. It never resends the original business request.
func completeReportSSO(ctx context.Context, client *http.Client, baseURL string, data []byte, resp *http.Response) ([]byte, *http.Response, error) {
	if resp.Request.URL.Hostname() != "login.sui.com" {
		return data, resp, nil
	}
	origin := resp.Request.URL
	if origin.Scheme != "https" || origin.Host != "login.sui.com" || origin.Path != "/auth.do" || resp.StatusCode != http.StatusOK {
		return nil, resp, ErrSessionExpired
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, resp, ErrSessionExpired
	}
	headers := make(http.Header)
	headers.Set("User-Agent", httpUserAgent)
	for jump := 0; jump < 5; jump++ {
		method, action, form, ok, err := parseAuthForm(data, resp.Request.URL)
		if err != nil {
			return nil, resp, ErrSessionExpired
		}
		if !ok {
			if jump == 0 || bytesLooksLikeLogin(data) {
				return nil, resp, ErrSessionExpired
			}
			// SSO can end with a JavaScript landing page; read the known report.
			return requestURLWithHeaders(ctx, client, strings.TrimRight(baseURL, "/")+"/report_index.do", headers)
		}
		target, err := url.Parse(action)
		if err != nil || target.Scheme != "https" || target.Host != base.Host || (target.Path != "/report_index.do" && target.Path != "/sso/redirect.do") || target.User != nil || method != http.MethodPost {
			return nil, resp, ErrSessionExpired
		}
		headers.Set("Referer", resp.Request.URL.String())
		data, resp, err = requestMethod(ctx, client, method, action, headers, form)
		if err != nil {
			return nil, resp, err
		}
		if resp.Request.URL.Host != base.Host {
			return nil, resp, ErrSessionExpired
		}
	}
	return nil, resp, ErrSessionExpired
}

func getWithClient(ctx context.Context, client *http.Client, baseURL, path string) ([]byte, error) {
	data, resp, err := requestURL(ctx, client, strings.TrimRight(baseURL, "/")+path)
	if err != nil {
		return nil, err
	}
	if resp != nil && resp.Request != nil && strings.Contains(resp.Request.URL.Host, "login.sui.com") {
		return nil, fmt.Errorf("%w: report page ended at %s%s", ErrSessionExpired, resp.Request.URL.Host, resp.Request.URL.Path)
	}
	if bytesLooksLikeLogin(data) {
		return nil, fmt.Errorf("%w: report page returned login form", ErrSessionExpired)
	}
	return data, nil
}

func requestURL(ctx context.Context, client *http.Client, endpoint string) ([]byte, *http.Response, error) {
	return requestURLWithHeaders(ctx, client, endpoint, nil)
}

func requestURLWithHeaders(ctx context.Context, client *http.Client, endpoint string, headers http.Header) ([]byte, *http.Response, error) {
	return requestMethod(ctx, client, http.MethodGet, endpoint, headers, nil)
}

func requestMethod(ctx context.Context, client *http.Client, method, endpoint string, headers http.Header, form url.Values) ([]byte, *http.Response, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, nil, err
	}
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	closeErr := resp.Body.Close()
	if err != nil {
		return nil, resp, err
	}
	if closeErr != nil {
		return nil, resp, closeErr
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, resp, ErrSessionExpired
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp, fmt.Errorf("sui returned %s", resp.Status)
	}
	return data, resp, nil
}

func parseAuthForm(data []byte, baseURL *url.URL) (method, action string, form url.Values, ok bool, err error) {
	text := string(data)
	onloadPattern := regexp.MustCompile(`(?is)<body\b[^>]*\bonload\s*=\s*["']\s*document\.forms\[0\]\.submit\(\)\s*;?\s*["'][^>]*>`)
	if !onloadPattern.MatchString(text) {
		return "", "", nil, false, nil
	}
	formPattern := regexp.MustCompile(`(?is)<form\b([^>]*)>(.*?)</form\s*>`)
	match := formPattern.FindStringSubmatch(text)
	if len(match) != 3 {
		return "", "", nil, false, fmt.Errorf("%w: auth form missing", ErrSessionExpired)
	}
	method = strings.ToUpper(attributeValue(match[1], "method"))
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodPost {
		return "", "", nil, false, fmt.Errorf("%w: unsupported auth form method", ErrSessionExpired)
	}
	action = attributeValue(match[1], "action")
	if action == "" {
		action = baseURL.String()
	} else {
		resolved, resolveErr := baseURL.Parse(action)
		if resolveErr != nil {
			return "", "", nil, false, fmt.Errorf("%w: invalid auth form action", ErrSessionExpired)
		}
		action = resolved.String()
	}
	form = make(url.Values)
	inputPattern := regexp.MustCompile(`(?is)<input\b([^>]*)>`)
	for _, input := range inputPattern.FindAllStringSubmatch(match[2], -1) {
		name := attributeValue(input[1], "name")
		if name != "" {
			form.Set(name, attributeValue(input[1], "value"))
		}
	}
	if method == http.MethodGet && len(form) > 0 {
		separator := "?"
		if strings.Contains(action, "?") {
			separator = "&"
		}
		action += separator + form.Encode()
		form = nil
	}
	return method, action, form, true, nil
}

func listBooksWithClient(ctx context.Context, client *http.Client, baseURL string) (Metadata, error) {
	data, err := getWithClient(ctx, client, baseURL, "/report_index.do")
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
