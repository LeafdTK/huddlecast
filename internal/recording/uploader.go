package recording

import (
	"context"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/LeafdTK/huddlecast/internal/config"
	"github.com/LeafdTK/huddlecast/internal/store"
)

type Uploader struct {
	cfg   config.R2
	dir   string
	store *store.Store
	log   *slog.Logger
	cli   *minio.Client
}

func New(r2 config.R2, dir string, st *store.Store, log *slog.Logger) (*Uploader, error) {
	u := &Uploader{cfg: r2, dir: dir, store: st, log: log}
	if !r2.Enabled() {
		return u, nil
	}
	endpoint := strings.TrimPrefix(strings.TrimPrefix(r2.Endpoint, "https://"), "http://")
	cli, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(r2.AccessKeyID, r2.SecretAccessKey, ""),
		Secure: true,
		Region: r2.Region,
	})
	if err != nil {
		return nil, err
	}
	u.cli = cli
	return u, nil
}

func (u *Uploader) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	u.scan(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			u.scan(ctx)
		}
	}
}

func (u *Uploader) scan(ctx context.Context) {
	seen := map[string]bool{}
	if recs, err := u.store.ListRecordings(ctx, 5000); err == nil {
		for _, r := range recs {
			seen[r.Storage+":"+r.Location] = true
		}
	}
	_ = filepath.WalkDir(u.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() == 0 {
			return nil
		}
		if time.Since(info.ModTime()) < u.cfg.UploadGrace {
			return nil
		}
		rel, _ := filepath.Rel(u.dir, p)
		rel = filepath.ToSlash(rel)
		streamKey := strings.SplitN(rel, "/", 2)[0]

		if !seen["local:"+rel] {
			if _, err := u.store.AddRecording(ctx, store.Recording{StreamKey: streamKey, Name: d.Name(), Storage: "local", Location: rel, Size: info.Size()}); err == nil {
				seen["local:"+rel] = true
			}
		}
		if u.cli == nil {
			return nil
		}
		key := u.cfg.Prefix + rel
		if seen["r2:"+key] {
			return nil
		}
		if _, err := u.cli.FPutObject(ctx, u.cfg.Bucket, key, p, minio.PutObjectOptions{ContentType: "video/mp4"}); err != nil {
			u.log.Warn("r2 upload", "file", rel, "err", err)
			return nil
		}
		u.log.Info("uploaded recording to r2", "key", key, "bytes", info.Size())
		if _, err := u.store.AddRecording(ctx, store.Recording{StreamKey: streamKey, Name: d.Name(), Storage: "r2", Location: key, Size: info.Size()}); err == nil {
			seen["r2:"+key] = true
		}
		if u.cfg.DeleteLocal {
			_ = os.Remove(p)
		}
		return nil
	})
}

func (u *Uploader) URL(r store.Recording) string {
	if r.Storage == "local" {
		return "/recordings/files/" + r.Location
	}
	if u.cfg.PublicBaseURL != "" {
		return strings.TrimSuffix(u.cfg.PublicBaseURL, "/") + "/" + strings.TrimPrefix(r.Location, u.cfg.Prefix)
	}
	if u.cli == nil {
		return ""
	}
	link, err := u.cli.PresignedGetObject(context.Background(), u.cfg.Bucket, r.Location, 6*time.Hour, url.Values{})
	if err != nil {
		u.log.Warn("presign", "key", r.Location, "err", err)
		return ""
	}
	return link.String()
}
