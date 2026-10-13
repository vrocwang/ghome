package server

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"net/http"
	"strings"
)

// maxWebhookBody bounds what an unauthenticated caller can make us buffer.
const maxWebhookBody = 1 << 20 // 1 MiB

// giteaPushPayload is the part of a Gitea push event we need.
type giteaPushPayload struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// handleWebhook turns a `git push` into an immediate reload.
//
// This is the fast path; polling remains as the safety net, so a missed or
// misconfigured webhook degrades to "up to one poll interval late" instead of
// "broken".
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if s.opts.WebhookSecret == "" {
		s.writeJSON(w, http.StatusForbidden, navError{
			Error: "webhook is disabled; start ghome with --webhook-secret (or GHOME_WEBHOOK_SECRET) to enable it",
		})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, navError{Error: "could not read request body"})
		return
	}

	if !validWebhookSignature(r.Header, body, s.opts.WebhookSecret) {
		s.log.Warn("rejected webhook with a bad signature", "remote", clientIP(r))
		s.writeJSON(w, http.StatusUnauthorized, navError{Error: "signature verification failed"})
		return
	}

	event := r.Header.Get("X-Gitea-Event")
	if event == "" {
		event = r.Header.Get("X-GitHub-Event")
	}
	if event == "ping" {
		// Gitea sends this when the webhook is created or tested.
		s.writeJSON(w, http.StatusOK, map[string]string{"status": "pong"})
		return
	}

	var payload giteaPushPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		s.log.Warn("webhook body is not JSON", "error", err, "remote", clientIP(r))
		s.writeJSON(w, http.StatusBadRequest, navError{Error: "could not parse the webhook payload"})
		return
	}

	// Ignore pushes to other branches: only the branch we actually read from
	// should cause a reload.
	if want := s.opts.Branch; want != "" && payload.Ref != "" {
		if payload.Ref != "refs/heads/"+want && payload.Ref != "refs/tags/"+want {
			s.writeJSON(w, http.StatusAccepted, map[string]string{
				"status": "ignored",
				"reason": "ref " + payload.Ref + " is not " + want,
			})
			return
		}
	}

	if s.opts.Trigger != nil {
		s.opts.Trigger()
	}
	s.log.Info("webhook received, reloading",
		"event", event,
		"ref", payload.Ref,
		"repo", payload.Repository.FullName,
		"commit", shorten(payload.After),
		"remote", clientIP(r))

	s.writeJSON(w, http.StatusAccepted, map[string]string{"status": "reload requested"})
}

// validWebhookSignature accepts the signature headers Gitea sends, plus the
// GitHub compatible ones, so the same endpoint also works against a Gitea
// instance configured with either format.
func validWebhookSignature(h http.Header, body []byte, secret string) bool {
	if sig := h.Get("X-Gitea-Signature"); sig != "" {
		return hmacMatches(sig, body, secret, sha256.New, "")
	}
	if sig := h.Get("X-Hub-Signature-256"); sig != "" {
		return hmacMatches(sig, body, secret, sha256.New, "sha256=")
	}
	if sig := h.Get("X-Hub-Signature"); sig != "" {
		return hmacMatches(sig, body, secret, sha1.New, "sha1=")
	}
	return false
}

// hmacMatches computes the HMAC of body and compares it in constant time. The
// prefix ("sha256=") is optional so both header styles work.
func hmacMatches(sig string, body []byte, secret string, hash func() hash.Hash, prefix string) bool {
	sig = strings.TrimSpace(sig)
	if prefix != "" {
		sig = strings.TrimPrefix(sig, prefix)
	}
	want := hmac.New(hash, []byte(secret))
	want.Write(body)
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(sig)), []byte(hex.EncodeToString(want.Sum(nil)))) == 1
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func shorten(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
