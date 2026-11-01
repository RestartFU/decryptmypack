package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/restartfu/decryptmypack/internal/adapters/filesystem"
	"github.com/restartfu/decryptmypack/internal/webaccounts"
	"golang.org/x/oauth2"
)

type accountTestAuthenticator struct{}

func (accountTestAuthenticator) Begin(context.Context) (*oauth2.DeviceAuthResponse, error) {
	return &oauth2.DeviceAuthResponse{DeviceCode: "device", UserCode: "WXYZ-1234", VerificationURI: "https://microsoft.com/link", Expiry: time.Now().Add(time.Minute)}, nil
}

func (accountTestAuthenticator) Complete(context.Context, *oauth2.DeviceAuthResponse) (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: "visitor-access", RefreshToken: "visitor-refresh", Expiry: time.Now().Add(time.Hour)}, nil
}

func (accountTestAuthenticator) AccountName(context.Context, *oauth2.Token) (string, error) {
	return "AlexMines42", nil
}

func (accountTestAuthenticator) Refresh(token *oauth2.Token) oauth2.TokenSource {
	return oauth2.StaticTokenSource(token)
}

// accountTestDownloader records which credential each download used.
type accountTestDownloader struct {
	publicTestDownloader
	usedAccount chan string
}

func (d *accountTestDownloader) DownloadWithTokenSource(source oauth2.TokenSource, host, resolvedIP string, port int, progress func(string)) (io.Reader, error) {
	token, err := source.Token()
	if err != nil {
		return nil, err
	}
	d.usedAccount <- token.AccessToken
	return d.publicTestDownloader.DownloadFrom(host, resolvedIP, port, progress)
}

func (d *accountTestDownloader) DownloadFrom(host, resolvedIP string, port int, progress func(string)) (io.Reader, error) {
	d.usedAccount <- "service"
	return d.publicTestDownloader.DownloadFrom(host, resolvedIP, port, progress)
}

func newAccountTestHandler(t *testing.T) (http.Handler, *accountTestDownloader) {
	t.Helper()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Flush() })
	accounts, err := webaccounts.Open(t.TempDir(), bytes.Repeat([]byte{3}, 32), accountTestAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	downloader := &accountTestDownloader{
		publicTestDownloader: publicTestDownloader{body: testOuterArchive(t, map[string][]byte{"Pack.zip": []byte("pack")})},
		usedAccount:          make(chan string, 4),
	}
	handler := New(store, downloader, WithWebAccounts(accounts), withTargetResolver(func(string) (string, error) {
		return "8.8.8.8", nil
	})).Handler()
	return handler, downloader
}

// accountCall sends a same-origin account API request carrying the visitor cookie when one is given.
func accountCall(handler http.Handler, method, target string, cookie *http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://decryptmypack.test"+target, nil)
	request.Header.Set(accountHeader, "1")
	request.Header.Set("Origin", "http://decryptmypack.test")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// signIn runs a whole device login through the HTTP API and returns the visitor cookie and account ID.
func signIn(t *testing.T, handler http.Handler) (*http.Cookie, string) {
	t.Helper()
	response := accountCall(handler, http.MethodPost, "/api/accounts/login", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("begin login status = %d: %s", response.Code, response.Body)
	}
	var cookie *http.Cookie
	for _, candidate := range response.Result().Cookies() {
		if candidate.Name == visitorCookie {
			cookie = candidate
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || !cookie.Secure {
		t.Fatalf("visitor cookie = %+v", cookie)
	}
	var challenge webaccounts.LoginChallenge
	if err := json.Unmarshal(response.Body.Bytes(), &challenge); err != nil || challenge.UserCode != "WXYZ-1234" {
		t.Fatalf("challenge = %+v, %v", challenge, err)
	}
	if strings.Contains(response.Body.String(), "device") {
		t.Fatal("login challenge exposed the device code")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status := accountCall(handler, http.MethodGet, "/api/accounts/login/"+challenge.LoginID, cookie)
		var state webaccounts.LoginState
		_ = json.Unmarshal(status.Body.Bytes(), &state)
		if state.State == "done" {
			return cookie, state.Account.ID
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("login did not finish")
	return nil, ""
}

func TestAccountAPIRejectsCrossSiteAndHeaderlessRequests(t *testing.T) {
	handler, _ := newAccountTestHandler(t)
	for name, mutate := range map[string]func(*http.Request){
		"no header": func(r *http.Request) { r.Header.Set("Origin", "http://decryptmypack.test") },
		"cross-site": func(r *http.Request) {
			r.Header.Set(accountHeader, "1")
			r.Header.Set("Origin", "https://evil.example")
		},
		"bad origin": func(r *http.Request) { r.Header.Set(accountHeader, "1"); r.Header.Set("Origin", "::not a url") },
	} {
		request := httptest.NewRequest(http.MethodPost, "http://decryptmypack.test/api/accounts/login", nil)
		mutate(request)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden || len(response.Result().Cookies()) != 0 {
			t.Fatalf("%s: status = %d, cookies = %v", name, response.Code, response.Result().Cookies())
		}
	}
}

func TestAccountAPIListsNothingAndSetsNoCookieForAnonymousVisitors(t *testing.T) {
	handler, _ := newAccountTestHandler(t)
	response := accountCall(handler, http.MethodGet, "/api/accounts", nil)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"accounts":[],"serviceAccount":true}` {
		t.Fatalf("status = %d body = %s", response.Code, response.Body)
	}
	if len(response.Result().Cookies()) != 0 {
		t.Fatal("anonymous visitor received a tracking cookie")
	}
}

func TestVisitorAccountDownloadsAndFillsSharedCache(t *testing.T) {
	handler, downloader := newAccountTestHandler(t)
	cookie, accountID := signIn(t, handler)

	list := accountCall(handler, http.MethodGet, "/api/accounts", cookie)
	if !strings.Contains(list.Body.String(), "AlexMines42") || strings.Contains(list.Body.String(), "visitor-access") {
		t.Fatalf("account list = %s", list.Body)
	}

	request := httptest.NewRequest(http.MethodGet, "http://decryptmypack.test/download?host=play.example.test", nil)
	request.Header.Set(downloadAccountHeader, accountID)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("download status = %d: %s", response.Code, response.Body)
	}
	if used := <-downloader.usedAccount; used != "visitor-access" {
		t.Fatalf("download used %q, want the visitor's account", used)
	}

	waitForCachedPack(t, handler)
}

// waitForCachedPack polls until the background download has stored play.example.test in the shared cache.
func waitForCachedPack(t *testing.T, handler http.Handler) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cached := httptest.NewRecorder()
		handler.ServeHTTP(cached, httptest.NewRequest(http.MethodGet, "http://decryptmypack.test/download?host=play.example.test", nil))
		status := httptest.NewRecorder()
		handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "http://decryptmypack.test/status?host=play.example.test", nil))
		if cached.Code == http.StatusOK && strings.Contains(cached.Body.String(), "/packs/") && strings.Contains(status.Body.String(), `"ready"`) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("downloaded pack was not added to the shared cache")
}

func TestDownloadAccountHeaderNeedsTheOwningVisitor(t *testing.T) {
	handler, downloader := newAccountTestHandler(t)
	_, accountID := signIn(t, handler)

	request := httptest.NewRequest(http.MethodGet, "http://decryptmypack.test/download?host=play.example.test", nil)
	request.Header.Set(downloadAccountHeader, accountID)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an account header without its visitor cookie", response.Code)
	}
	select {
	case used := <-downloader.usedAccount:
		t.Fatalf("download started with %q", used)
	default:
	}
}

func TestDownloadWithoutAccountHeaderUsesServiceAccount(t *testing.T) {
	handler, downloader := newAccountTestHandler(t)
	cookie, _ := signIn(t, handler)
	request := httptest.NewRequest(http.MethodGet, "http://decryptmypack.test/download?host=play.example.test", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d", response.Code)
	}
	if used := <-downloader.usedAccount; used != "service" {
		t.Fatalf("a cookie alone made the download use %q; only the explicit header may", used)
	}
	waitForCachedPack(t, handler)
}

func TestRemovingAccountDeletesIt(t *testing.T) {
	handler, _ := newAccountTestHandler(t)
	cookie, accountID := signIn(t, handler)
	response := accountCall(handler, http.MethodDelete, "/api/accounts/"+accountID, cookie)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"accounts":[],"serviceAccount":true}` {
		t.Fatalf("remove status = %d body = %s", response.Code, response.Body)
	}
	if again := accountCall(handler, http.MethodDelete, "/api/accounts/"+accountID, cookie); again.Code != http.StatusNotFound {
		t.Fatalf("second remove status = %d", again.Code)
	}
}

func TestWithoutServiceAccountRequiresVisitorAccountForNewServers(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Flush() })
	accounts, err := webaccounts.Open(t.TempDir(), bytes.Repeat([]byte{3}, 32), accountTestAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	downloader := &accountTestDownloader{
		publicTestDownloader: publicTestDownloader{body: testOuterArchive(t, map[string][]byte{"Pack.zip": []byte("pack")})},
		usedAccount:          make(chan string, 4),
	}
	handler := New(store, downloader, WithWebAccounts(accounts), WithoutServiceAccount(), withTargetResolver(func(string) (string, error) {
		return "8.8.8.8", nil
	})).Handler()

	anonymous := httptest.NewRecorder()
	handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "http://decryptmypack.test/download?host=play.example.test", nil))
	if anonymous.Code != http.StatusUnauthorized || !strings.Contains(anonymous.Body.String(), "add a Microsoft account") {
		t.Fatalf("anonymous status = %d body = %s", anonymous.Code, anonymous.Body)
	}
	if list := accountCall(handler, http.MethodGet, "/api/accounts", nil); !strings.Contains(list.Body.String(), `"serviceAccount":false`) {
		t.Fatalf("account list does not report the missing service account: %s", list.Body)
	}

	cookie, accountID := signIn(t, handler)
	request := httptest.NewRequest(http.MethodGet, "http://decryptmypack.test/download?host=play.example.test", nil)
	request.Header.Set(downloadAccountHeader, accountID)
	request.AddCookie(cookie)
	withAccount := httptest.NewRecorder()
	handler.ServeHTTP(withAccount, request)
	if withAccount.Code != http.StatusAccepted {
		t.Fatalf("account download status = %d: %s", withAccount.Code, withAccount.Body)
	}
	if used := <-downloader.usedAccount; used != "visitor-access" {
		t.Fatalf("download used %q", used)
	}
	waitForCachedPack(t, handler)

	cached := httptest.NewRecorder()
	handler.ServeHTTP(cached, httptest.NewRequest(http.MethodGet, "http://decryptmypack.test/download?host=play.example.test", nil))
	if cached.Code != http.StatusOK {
		t.Fatalf("cached pack needs an account: status = %d", cached.Code)
	}
}
