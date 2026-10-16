package server

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

	// MaxBytesReader rather than LimitReader: an oversized body is refused
	// outright instead of being silently truncated to something whose signature
	// cannot match, and the reader tells the server to hang up afterwards.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeJSON(w, http.StatusRequestEntityTooLarge, navError{
				Error: fmt.Sprintf("webhook body exceeds %d bytes", maxWebhookBody),
			})
			return
		}
		s.writeJSON(w, http.StatusBadRequest, navError{Error: "could not read request body"})
		return
	}

	if !validWebhookSignature(r.Header, body, s.opts.WebhookSecret) {
		s.log.Warn("rejected webhook with a bad signature", "remote", clientIP(r))
		s.writeJSON(w, http.StatusUnauthorized, navError{Error: "signature verification failed"})
		return
	}

	// Gitea sends X-Gitea-Event; Forgejo sends X-Forgejo-Event alongside it, and
	// older Forgejo releases send only the Gitea-named one.
	event := r.Header.Get("X-Forgejo-Event")
	if event == "" {
		event = r.Header.Get("X-Gitea-Event")
	}
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

	// Any push reloads. There is no branch filter here on purpose: a git server
	// can filter deliveries per webhook, which is both stronger (globs, one
	// filter per consumer) and cheaper (the request is never sent), and ghome
	// reads one ref anyway. A push that changed nothing costs one fetch, and the
	// watcher's fingerprint comparison stops it there.
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

// signatureHeaders are the signature headers accepted, in no particular order
// of preference: any one of them matching is enough.
//
// Forgejo is a Gitea fork and older releases sign only with the Gitea-named
// header, while its documentation shows the Forgejo-named one; newer releases
// appear to send both. Rather than depend on which, accept either. Gitea,
// Forgejo and Gogs all sign the raw body with HMAC-SHA256, so the values are
// interchangeable when present. The GitHub-style pair is accepted so the same
// endpoint works for a Gitea or Forgejo instance configured that way.
var signatureHeaders = []struct {
	header string
	prefix string
	hash   func() hash.Hash
}{
	{"X-Forgejo-Signature", "", sha256.New},
	{"X-Gitea-Signature", "", sha256.New},
	{"X-Hub-Signature-256", "sha256=", sha256.New},
	{"X-Hub-Signature", "sha1=", sha1.New},
}

// validWebhookSignature reports whether any supplied signature matches.
//
// Every candidate is compared in constant time. A present-but-wrong header is
// not fatal on its own: if a proxy injects a stray header, a correct one
// alongside it still counts.
func validWebhookSignature(h http.Header, body []byte, secret string) bool {
	for _, candidate := range signatureHeaders {
		sig := h.Get(candidate.header)
		if sig == "" {
			continue
		}
		if hmacMatches(sig, body, secret, candidate.hash, candidate.prefix) {
			return true
		}
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
