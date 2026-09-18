package media

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

type AuthRequest struct {
	User     string `json:"user"`
	Password string `json:"password"`
	Token    string `json:"token"`
	IP       string `json:"ip"`
	Action   string `json:"action"`
	Path     string `json:"path"`
	Protocol string `json:"protocol"`
	ID       string `json:"id"`
	Query    string `json:"query"`
}

type KeyChecker interface {
	StreamKeyExists(ctx context.Context, key string) (bool, error)
}

func AuthHandler(keys KeyChecker, mirrorToken string, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req AuthRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		allowed := false
		switch {
		case strings.HasPrefix(req.Path, "live/"), strings.HasPrefix(req.Path, "rtmp/"):
			key := req.Path[strings.Index(req.Path, "/")+1:]
			ok, err := keys.StreamKeyExists(r.Context(), key)
			if err != nil {
				log.Error("auth: key lookup", "err", err)
			}
			allowed = ok && (req.Action == "publish" || req.Action == "read" || req.Action == "playback")

			if allowed && req.Action == "publish" {
				for _, cred := range []string{req.Password, req.Token} {
					if cred != "" && cred != key {
						allowed = false
					}
				}
			}
		case strings.HasPrefix(req.Path, "mirror/"):
			switch req.Action {
			case "publish":
				allowed = mirrorToken != "" && (req.Token == mirrorToken || req.Password == mirrorToken)
			case "read", "playback":
				allowed = true
			}
		}
		if !allowed {
			log.Warn("mediamtx auth denied", "action", req.Action, "path", req.Path, "ip", req.IP, "protocol", req.Protocol)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}
