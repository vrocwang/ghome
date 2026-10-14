package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/vrocwang/ghome/internal/nav"
)

// Verification verdicts are cached for two reasons. A page load should not
// reach the forge, and a wrong token should not be a way to make ghome hammer
// it -- so failures are remembered too, briefly.
const (
	verifyTimeout    = 10 * time.Second
	verifySuccessTTL = 5 * time.Minute
	verifyFailureTTL = 30 * time.Second
	verifyMaxEntries = 256
)

// verifier answers "may this token read the config repository", with a small
// cache. The token is never stored, only its hash.
//
// A nil verifier is the safe default: the private entries stay hidden and
// nothing can unlock them.
type verifier struct {
	check func(context.Context, string) bool
	now   func() time.Time

	mu   sync.Mutex
	seen map[string]verdict
}

type verdict struct {
	ok    bool
	until time.Time
}

func newVerifier(check func(context.Context, string) bool) *verifier {
	return &verifier{
		check: check,
		now:   time.Now,
		seen:  make(map[string]verdict, 8),
	}
}

// allowed reports whether the token may see the private entries.
func (v *verifier) allowed(ctx context.Context, token string) bool {
	if v == nil || v.check == nil || token == "" {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(sum[:])

	v.mu.Lock()
	if got, ok := v.seen[key]; ok && v.now().Before(got.until) {
		v.mu.Unlock()
		return got.ok
	}
	v.mu.Unlock()

	// Detach from the caller's context. A client that gave up mid-request would
	// otherwise cancel this call, and the failure would be cached as "this token
	// is refused" -- locking a legitimate viewer out for the whole failure TTL.
	// The timeout keeps a hanging forge from piling up goroutines.
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), verifyTimeout)
	defer cancel()

	ok := v.check(checkCtx, token)

	ttl := verifyFailureTTL
	if ok {
		ttl = verifySuccessTTL
	}
	v.mu.Lock()
	if len(v.seen) >= verifyMaxEntries {
		v.evictLocked()
	}
	v.seen[key] = verdict{ok: ok, until: v.now().Add(ttl)}
	v.mu.Unlock()

	return ok
}

// evictLocked drops expired verdicts, and everything if that was not enough.
// The map is bounded so that a stream of distinct wrong tokens cannot grow it.
func (v *verifier) evictLocked() {
	now := v.now()
	for k, got := range v.seen {
		if now.After(got.until) {
			delete(v.seen, k)
		}
	}
	if len(v.seen) >= verifyMaxEntries {
		v.seen = make(map[string]verdict, 8)
	}
}

// viewFor applies the private tier to a payload for this particular caller.
//
// The filtering happens here, on the server, because a link that reaches the
// browser is not private any more -- hiding it in the page would still hand it
// to anyone with curl.
func (s *Server) viewFor(ctx context.Context, token string, payload nav.Payload) nav.Payload {
	total := payload.PrivateEntries()
	if total == 0 {
		return payload
	}
	if s.private.allowed(ctx, token) {
		payload.Private = &nav.PrivateView{Total: total}
		return payload
	}
	out := payload.Public()
	out.Private = &nav.PrivateView{Total: total, Locked: true}
	return out
}
