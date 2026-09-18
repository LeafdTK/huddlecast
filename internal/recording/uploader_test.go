package recording

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeafdTK/huddlecast/internal/config"
	"github.com/LeafdTK/huddlecast/internal/store"
)

func TestScanIndexesLocalFiles(t *testing.T) {
	dir := t.TempDir()
	keyDir := filepath.Join(dir, "streamkey1")
	if err := os.MkdirAll(keyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keyDir, "seg.mp4"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	u, err := New(config.R2{UploadGrace: 0}, dir, st, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if u.cli != nil {
		t.Fatal("r2 client should be nil when disabled")
	}
	u.scan(context.Background())

	recs, _ := st.ListRecordings(context.Background(), 0)
	if len(recs) != 1 {
		t.Fatalf("want 1 indexed local recording, got %d: %+v", len(recs), recs)
	}
	if recs[0].Storage != "local" || recs[0].StreamKey != "streamkey1" || recs[0].Location != "streamkey1/seg.mp4" {
		t.Fatalf("bad indexed recording: %+v", recs[0])
	}
	if got := u.URL(recs[0]); got != "/recordings/files/streamkey1/seg.mp4" {
		t.Errorf("local url: %q", got)
	}
}
