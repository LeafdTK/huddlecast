package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

type ctxKey int

const userKey ctxKey = 1

type User struct {
	ID    string
	Name  string
	Admin bool
}

func userFrom(ctx context.Context) User {
	u, _ := ctx.Value(userKey).(User)
	return u
}

const cookieName = "hc_session"

func (s *Server) sign(payload string) string {
	m := hmac.New(sha256.New, []byte(s.cfg.Web.SessionSecret))
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Server) setSession(w http.ResponseWriter, userID, name string) {
	exp := time.Now().Add(7 * 24 * time.Hour).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte(userID)) + "." + base64.RawURLEncoding.EncodeToString([]byte(name)) + "." + strconv.FormatInt(exp, 10)
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: payload + "." + s.sign(payload), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: strings.HasPrefix(s.cfg.PublicURL, "https://"), Expires: time.Unix(exp, 0),
	})
}

func (s *Server) readSession(r *http.Request) (userID, name string, ok bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", "", false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 4 {
		return "", "", false
	}
	payload := strings.Join(parts[:3], ".")
	if subtle.ConstantTimeCompare([]byte(s.sign(payload)), []byte(parts[3])) != 1 {
		return "", "", false
	}
	exp, _ := strconv.ParseInt(parts[2], 10, 64)
	if time.Now().Unix() > exp {
		return "", "", false
	}
	id, _ := base64.RawURLEncoding.DecodeString(parts[0])
	n, _ := base64.RawURLEncoding.DecodeString(parts[1])
	return string(id), string(n), true
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, name, ok := s.readSession(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		u := User{ID: id, Name: name}
		if s.cfg.Web.Auth == "password" && id == "admin" {
			u.Admin = true
		} else {
			wl, err := s.store.IsWhitelisted(r.Context(), id)
			if err != nil || !wl {
				http.Error(w, "not on the whitelist", http.StatusForbidden)
				return
			}
			u.Admin, _ = s.store.IsAdmin(r.Context(), id)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	})
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !userFrom(r.Context()).Admin {
			http.Error(w, "admins only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && s.cfg.Web.Auth == "password" {
		if subtle.ConstantTimeCompare([]byte(r.FormValue("password")), []byte(s.cfg.Web.Password)) == 1 {
			s.setSession(w, "admin", "admin")
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		s.render(w, r, "login.html", map[string]any{"Error": "wrong password", "Mode": s.cfg.Web.Auth})
		return
	}
	s.render(w, r, "login.html", map[string]any{"Mode": s.cfg.Web.Auth})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) redirectURI() string {
	return strings.TrimRight(s.cfg.PublicURL, "/") + "/auth/callback"
}

func (s *Server) handleSlackAuth(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	state := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: "hc_state", Value: state, Path: "/", HttpOnly: true, MaxAge: 600, SameSite: http.SameSiteLaxMode})
	q := url.Values{
		"response_type": {"code"}, "scope": {"openid profile"}, "client_id": {s.cfg.Slack.ClientID},
		"redirect_uri": {s.redirectURI()}, "state": {state}, "team": {s.cfg.Slack.TeamID},
	}
	http.Redirect(w, r, "https://slack.com/openid/connect/authorize?"+q.Encode(), http.StatusFound)
}

func (s *Server) handleSlackCallback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("hc_state")
	if err != nil || c.Value == "" || c.Value != r.URL.Query().Get("state") {
		http.Error(w, "bad oauth state", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	resp, err := slack.GetOpenIDConnectTokenContext(r.Context(), http.DefaultClient, s.cfg.Slack.ClientID, s.cfg.Slack.ClientSecret, code, s.redirectURI())
	if err != nil {
		http.Error(w, "slack token exchange failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	claims, err := parseIDToken(resp.IdToken)
	if err != nil {
		http.Error(w, "bad id token: "+err.Error(), http.StatusBadGateway)
		return
	}
	if claims.TeamID != "" && claims.TeamID != s.cfg.Slack.TeamID {
		http.Error(w, "wrong slack workspace", http.StatusForbidden)
		return
	}
	ok, err := s.store.IsWhitelisted(r.Context(), claims.UserID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		s.log.Warn("web login from non-whitelisted user", "user", claims.UserID)
		http.Error(w, fmt.Sprintf("slack user %s is not on the huddlecast whitelist", claims.UserID), http.StatusForbidden)
		return
	}
	s.setSession(w, claims.UserID, claims.Name)
	http.Redirect(w, r, "/", http.StatusFound)
}

type idClaims struct {
	UserID string `json:"https://slack.com/user_id"`
	TeamID string `json:"https://slack.com/team_id"`
	Name   string `json:"name"`
	Exp    int64  `json:"exp"`
}

func parseIDToken(tok string) (*idClaims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("not a jwt")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c idClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if c.UserID == "" {
		return nil, fmt.Errorf("no user id claim")
	}
	if c.Exp != 0 && time.Now().Unix() > c.Exp {
		return nil, fmt.Errorf("expired")
	}
	return &c, nil
}
