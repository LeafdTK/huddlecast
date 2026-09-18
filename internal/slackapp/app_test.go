package slackapp

import "testing"

func TestChannelRegexps(t *testing.T) {
	if m := mentionRe.FindStringSubmatch("<#C0123ABC|general>"); m == nil || m[1] != "C0123ABC" || m[2] != "general" {
		t.Fatalf("mention parse: %v", m)
	}
	if m := mentionRe.FindStringSubmatch("<#C0123ABC>"); m == nil || m[1] != "C0123ABC" {
		t.Fatalf("bare mention parse: %v", m)
	}
	if !idRe.MatchString("C0123ABCDEF") || idRe.MatchString("general") || idRe.MatchString("U0123ABCDEF") {
		t.Fatal("id regexp")
	}
	if len(NewStreamKey()) != 32 {
		t.Fatal("stream key length")
	}
}
