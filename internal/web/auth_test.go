package web

import (
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LeafdTK/huddlecast/internal/config"
)

func TestParseIDToken(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://slack.com/user_id":"U1","https://slack.com/team_id":"T1","name":"n"}`))
	c, err := parseIDToken("h." + payload + ".s")
	if err != nil || c.UserID != "U1" || c.TeamID != "T1" {
		t.Fatalf("%v %+v", err, c)
	}
	if _, err := parseIDToken("garbage"); err == nil {
		t.Fatal("expected error")
	}
}

func TestSessionCookieRoundTrip(t *testing.T) {
	s := &Server{cfg: &config.Config{Web: config.Web{SessionSecret: "0123456789abcdef0123456789abcdef"}}, log: slog.Default()}
	rec := httptest.NewRecorder()
	s.setSession(rec, "U1", "name")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	id, name, ok := s.readSession(req)
	if !ok || id != "U1" || name != "name" {
		t.Fatalf("roundtrip failed: %q %q %v", id, name, ok)
	}
	s.cfg.Web.SessionSecret = "another-secret-that-is-long-enough"
	if _, _, ok := s.readSession(req); ok {
		t.Fatal("cookie should not verify with a different secret")
	}
}
