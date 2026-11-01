package service

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/restartfu/decryptmypack/internal/webaccounts"
	"golang.org/x/oauth2"
)

const (
	visitorCookie = "dmp_visitor"
	// accountHeader marks a same-origin script request; browsers cannot send it cross-site without CORS.
	accountHeader = "X-DMP-Request"
	// downloadAccountHeader names the account a visitor chose for an uncached download.
	downloadAccountHeader = "X-DMP-Account"
	visitorCookieMaxAge   = 180 * 24 * time.Hour
	loginStartTimeout     = 20 * time.Second
)

// WithoutServiceAccount runs the service with no Microsoft token of its own, so servers that are not
// cached yet can only be fetched with a visitor's account.
func WithoutServiceAccount() Option {
	return func(s *Service) { s.serviceAccount = false }
}

// WithWebAccounts lets website visitors add Microsoft accounts and download with them.
func WithWebAccounts(store *webaccounts.Store) Option {
	return func(s *Service) { s.webAccounts = store }
}

func (s *Service) registerAccountRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/accounts", s.listAccounts)
	mux.HandleFunc("POST /api/accounts/login", s.beginAccountLogin)
	mux.HandleFunc("GET /api/accounts/login/{login}", s.accountLoginStatus)
	mux.HandleFunc("DELETE /api/accounts/login/{login}", s.cancelAccountLogin)
	mux.HandleFunc("POST /api/accounts/{account}/select", s.selectAccount)
	mux.HandleFunc("DELETE /api/accounts/{account}", s.removeAccount)
}

// accountRequest validates an account API call and returns the caller's visitor, if any.
func (s *Service) accountRequest(w http.ResponseWriter, r *http.Request) (webaccounts.Visitor, bool, bool) {
	w.Header().Set("Cache-Control", "no-store")
	if s.webAccounts == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "accounts are not enabled on this server"})
		return webaccounts.Visitor{}, false, false
	}
	if r.Header.Get(accountHeader) != "1" || !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "request rejected"})
		return webaccounts.Visitor{}, false, false
	}
	visitor, ok := requestVisitor(r)
	return visitor, ok, true
}

func (s *Service) listAccounts(w http.ResponseWriter, r *http.Request) {
	visitor, found, ok := s.accountRequest(w, r)
	if !ok {
		return
	}
	list := []webaccounts.Account{}
	if found {
		var err error
		if list, err = s.webAccounts.List(visitor); err != nil {
			log.Printf("could not read web account vault: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "your saved accounts could not be read"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": list, "serviceAccount": s.serviceAccount})
}

func (s *Service) beginAccountLogin(w http.ResponseWriter, r *http.Request) {
	visitor, found, ok := s.accountRequest(w, r)
	if !ok {
		return
	}
	if !found {
		var cookie string
		var err error
		visitor, cookie, err = webaccounts.NewVisitor()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not start sign-in"})
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     visitorCookie,
			Value:    cookie,
			Path:     "/",
			MaxAge:   int(visitorCookieMaxAge.Seconds()),
			HttpOnly: true,
			Secure:   !localRequest(r),
			SameSite: http.SameSiteLaxMode,
		})
	}
	ctx, cancel := context.WithTimeout(r.Context(), loginStartTimeout)
	defer cancel()
	challenge, err := s.webAccounts.BeginLogin(ctx, visitor)
	switch {
	case errors.Is(err, webaccounts.ErrTooManyAccounts), errors.Is(err, webaccounts.ErrLoginsBusy):
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
		return
	case err != nil:
		log.Printf("could not begin web account login: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Microsoft sign-in could not be started. Try again shortly."})
		return
	}
	writeJSON(w, http.StatusOK, challenge)
}

func (s *Service) accountLoginStatus(w http.ResponseWriter, r *http.Request) {
	visitor, found, ok := s.accountRequest(w, r)
	if !ok {
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": webaccounts.ErrLoginNotFound.Error()})
		return
	}
	state, err := s.webAccounts.LoginStatus(visitor, r.PathValue("login"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Service) cancelAccountLogin(w http.ResponseWriter, r *http.Request) {
	visitor, found, ok := s.accountRequest(w, r)
	if !ok {
		return
	}
	if found {
		s.webAccounts.CancelLogin(visitor, r.PathValue("login"))
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) selectAccount(w http.ResponseWriter, r *http.Request) {
	s.changeAccount(w, r, s.webAccounts.Select)
}

func (s *Service) removeAccount(w http.ResponseWriter, r *http.Request) {
	s.changeAccount(w, r, s.webAccounts.Remove)
}

// changeAccount applies one account mutation and returns the visitor's updated list.
func (s *Service) changeAccount(w http.ResponseWriter, r *http.Request, change func(webaccounts.Visitor, string) error) {
	visitor, found, ok := s.accountRequest(w, r)
	if !ok {
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": webaccounts.ErrAccountNotFound.Error()})
		return
	}
	if err := change(visitor, r.PathValue("account")); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, webaccounts.ErrAccountNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	list, err := s.webAccounts.List(visitor)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "your saved accounts could not be read"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": list, "serviceAccount": s.serviceAccount})
}

// downloadTokenSource returns the visitor's chosen account for an uncached download, or nil to use the
// service account. The account is only used when the page names it in a same-origin header, so a link
// or form on another site can never make a visitor's account join a server.
func (s *Service) downloadTokenSource(r *http.Request) (oauth2.TokenSource, error) {
	accountID := strings.TrimSpace(r.Header.Get(downloadAccountHeader))
	if accountID == "" {
		return nil, nil
	}
	if s.webAccounts == nil || !sameOrigin(r) {
		return nil, errAccountUnavailable
	}
	visitor, ok := requestVisitor(r)
	if !ok {
		return nil, errAccountUnavailable
	}
	source, err := s.webAccounts.TokenSource(visitor, accountID)
	if err != nil {
		return nil, errAccountUnavailable
	}
	return source, nil
}

var errAccountRequired = errors.New("add a Microsoft account to fetch servers that are not cached yet")

var errAccountUnavailable = errors.New("the selected Microsoft account is no longer available; choose or add an account and try again")

func requestVisitor(r *http.Request) (webaccounts.Visitor, bool) {
	cookie, err := r.Cookie(visitorCookie)
	if err != nil {
		return webaccounts.Visitor{}, false
	}
	return webaccounts.ParseVisitor(cookie.Value)
}

// sameOrigin rejects requests whose Origin header names another site.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}

// localRequest reports whether the request targets this machine, where plain-HTTP cookies are needed.
func localRequest(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
