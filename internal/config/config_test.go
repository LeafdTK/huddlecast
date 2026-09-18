package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const good = `
slack:
  team_id: T1
  bot_token: xoxb-1
  app_token: xapp-1
  client_id: c
  client_secret: s
accounts:
  - name: a
    cookie_d: ${TEST_COOKIE}
web:
  session_secret: 0123456789abcdef0123456789abcdef
`

func TestLoadExpandsEnvAndDefaults(t *testing.T) {
	t.Setenv("TEST_COOKIE", "xoxd-secret")
	c, err := Load(write(t, good))
	if err != nil {
		t.Fatal(err)
	}
	if c.Accounts[0].CookieD != "xoxd-secret" {
		t.Errorf("cookie not expanded: %q", c.Accounts[0].CookieD)
	}
	if c.Listen != ":8080" || c.Browser.Width != 1280 || c.Huddle.JoinTimeout == 0 || c.Slack.SlashCommand != "/huddlecast" {
		t.Errorf("defaults not applied: %+v", c)
	}
}

func TestLoadRejectsUnsetEnv(t *testing.T) {
	os.Unsetenv("TEST_COOKIE")
	_, err := Load(write(t, good))
	if err == nil || !strings.Contains(err.Error(), "TEST_COOKIE") {
		t.Fatalf("expected unset env error, got %v", err)
	}
}

func TestValidate(t *testing.T) {
	t.Setenv("TEST_COOKIE", "x")
	cases := map[string]string{
		"no accounts":        strings.Replace(good, "accounts:\n  - name: a\n    cookie_d: ${TEST_COOKIE}\n", "", 1),
		"bad web auth":       good + "  auth: nope\n",
		"password no value":  good + "  auth: password\n",
		"short secret":       strings.Replace(good, "0123456789abcdef0123456789abcdef", "short", 1),
		"duplicate accounts": strings.Replace(good, "accounts:\n", "accounts:\n  - name: a\n    cookie_d: x\n", 1),
	}
	for name, body := range cases {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
