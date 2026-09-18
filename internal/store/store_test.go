package store

import (
	"context"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestWhitelist(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if ok, _ := s.IsWhitelisted(ctx, "U1"); ok {
		t.Fatal("should not be whitelisted")
	}
	if err := s.UpsertWhitelist(ctx, WhitelistEntry{UserID: "U1", Name: "one", Admin: false, AddedBy: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertWhitelist(ctx, WhitelistEntry{UserID: "U1", Name: "uno", Admin: true, AddedBy: "t"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.IsAdmin(ctx, "U1"); !ok {
		t.Fatal("admin should stick")
	}
	if err := s.UpsertWhitelist(ctx, WhitelistEntry{UserID: "U1", Name: "uno", Admin: false}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.IsAdmin(ctx, "U1"); !ok {
		t.Fatal("admin must not be downgraded by a non-admin upsert")
	}
	list, _ := s.ListWhitelist(ctx)
	if len(list) != 1 || list[0].Name != "uno" {
		t.Fatalf("unexpected list %+v", list)
	}
	_ = s.RemoveWhitelist(ctx, "U1")
	if ok, _ := s.IsWhitelisted(ctx, "U1"); ok {
		t.Fatal("should be removed")
	}
}

func TestSessionsTargetsChat(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.CreateStreamKey(ctx, StreamKey{Key: "k1", OwnerID: "U1", Label: "l"}); err != nil {
		t.Fatal(err)
	}
	if k, _ := s.GetStreamKey(ctx, "k1"); k == nil || k.OwnerID != "U1" {
		t.Fatal("key roundtrip")
	}
	if k, _ := s.GetStreamKey(ctx, "nope"); k != nil {
		t.Fatal("missing key should be nil")
	}
	sess := Session{ID: "s1", CreatedBy: "U1", SourceType: "push", SourceRef: "k1", Presentation: "screen", Status: "running"}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddTarget(ctx, Target{SessionID: "s1", ChannelID: "C1", ChannelName: "gen", Account: "a", Status: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.UpdateTargetStatus(ctx, id, "streaming", "")
	_ = s.SetTargetHuddleRoot(ctx, "s1", "C1", "1.2")
	ts, _ := s.ListTargets(ctx, "s1")
	if len(ts) != 1 || ts[0].Status != "streaming" || ts[0].HuddleRootTS != "1.2" {
		t.Fatalf("targets %+v", ts)
	}
	if _, err := s.AddChat(ctx, ChatMessage{SessionID: "s1", ChannelID: "C1", UserID: "U2", Text: "hi", TS: "1.3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddChat(ctx, ChatMessage{SessionID: "s1", ChannelID: "C1", UserID: "U2", Text: "dup", TS: "1.3"}); err != nil {
		t.Fatal(err)
	}
	msgs, _ := s.ListChat(ctx, "s1", 0, 10)
	if len(msgs) != 1 || msgs[0].Text != "hi" {
		t.Fatalf("chat dedupe failed: %+v", msgs)
	}
	running, _ := s.ListSessions(ctx, true, 10)
	if len(running) != 1 {
		t.Fatal("expected one running session")
	}
	_ = s.UpdateSession(ctx, "s1", "camera", "stopped")
	got, _ := s.GetSession(ctx, "s1")
	if got.Status != "stopped" || got.Presentation != "camera" || got.EndedAt == "" {
		t.Fatalf("session update %+v", got)
	}
	running, _ = s.ListSessions(ctx, true, 10)
	if len(running) != 0 {
		t.Fatal("expected no running sessions")
	}
	_ = s.DeleteTarget(ctx, id)
	if ts, _ := s.ListTargets(ctx, "s1"); len(ts) != 0 {
		t.Fatal("target not deleted")
	}
}

func TestIdentities(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.UpsertIdentity(ctx, Identity{Name: "bot1", CookieD: "xoxd-a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertIdentity(ctx, Identity{Name: "bot1", CookieD: "xoxd-b", Mode: "camera"}); err != nil {
		t.Fatal(err)
	}
	ids, _ := s.ListIdentities(ctx)
	if len(ids) != 1 || ids[0].CookieD != "xoxd-b" || ids[0].Mode != "camera" || !ids[0].Managed {
		t.Fatalf("bad identity upsert: %+v", ids)
	}
	if err := s.SetAccountState(ctx, "cfgbot", "ok", ""); err != nil {
		t.Fatal(err)
	}
	if ids, _ := s.ListIdentities(ctx); len(ids) != 1 {
		t.Fatalf("config account leaked into identities: %+v", ids)
	}
	if err := s.DeleteIdentity(ctx, "bot1"); err != nil {
		t.Fatal(err)
	}
	if ids, _ := s.ListIdentities(ctx); len(ids) != 0 {
		t.Fatalf("identity not deleted: %+v", ids)
	}
}

func TestRecordings(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, err := s.AddRecording(ctx, Recording{StreamKey: "k", Name: "a.mp4", Storage: "local", Location: "k/a.mp4", Size: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRecording(ctx, Recording{StreamKey: "k", Name: "a.mp4", Storage: "local", Location: "k/a.mp4", Size: 99}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRecording(ctx, Recording{StreamKey: "k", Name: "a.mp4", Storage: "r2", Location: "recordings/k/a.mp4", Size: 99}); err != nil {
		t.Fatal(err)
	}
	recs, _ := s.ListRecordings(ctx, 0)
	if len(recs) != 2 {
		t.Fatalf("want 2 rows (dedup local, plus r2), got %d: %+v", len(recs), recs)
	}
	for _, r := range recs {
		if r.Location == "k/a.mp4" && r.Size != 99 {
			t.Errorf("local size not updated on conflict: %+v", r)
		}
	}
}
