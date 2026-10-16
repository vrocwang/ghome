package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/vrocwang/ghome/internal/nav"
	"github.com/vrocwang/ghome/internal/source"
)

// Verification verdicts are cached for two reasons. A page load should not
// reach the forge, and a wrong token should not be a way to make ghome hammer
// it -- so failures are remembered too, briefly.
const (
	verifyTimeout    = 10 * time.Second
	verifySuccessTTL = 5 * time.Minute
	verifyFailureTTL = 5 * time.Minute
	// verifyUnavailableTTL is much shorter: nothing was learned about the token,
	// so remembering it for long would lock a legitimate viewer out over a
	// forge hiccup.
	verifyUnavailableTTL = 30 * time.Second
	verifyMaxEntries     = 256
	// verifyMaxInflight bounds how many checks can be in flight at once. A
	// verdict only exists once a check has finished, so without this an
	// unauthenticated caller decides how many requests ghome makes to the forge
	// at the same time -- from the address and with the credential path the
	// config fetch itself uses.
	verifyMaxInflight = 8
)

// verifier answers "may this token read the config repository", with a small
// cache. The token is never stored, only its hash.
//
// The zero value is not usable -- its semaphore is nil, which would block
// forever rather than fail -- so build one with newVerifier.
//
// A nil verifier is the safe default: the private entries stay hidden and
// nothing can unlock them.
type verifier struct {
	check func(context.Context, string) source.Access
	now   func() time.Time

	mu   sync.Mutex
	seen map[string]verdict
	// inflight collapses concurrent checks of the same token into one cache
	// miss, so a burst of requests carrying the same unseen token is one
	// question to the forge rather than one per request.
	inflight map[string]chan struct{}
	// sem bounds the total number of concurrent checks; see verifyMaxInflight.
	sem chan struct{}
}

type verdict struct {
	access source.Access
	until  time.Time
}

func newVerifier(check func(context.Context, string) source.Access) *verifier {
	return &verifier{
		check:    check,
		now:      time.Now,
		seen:     make(map[string]verdict, 8),
		inflight: make(map[string]chan struct{}, 4),
		sem:      make(chan struct{}, verifyMaxInflight),
	}
}

// allowed reports whether the token may see the private entries, and why not
// when it may not. "Unavailable" is a distinct answer on purpose: it is the
// difference between "this token is wrong" (clear it, tell the user) and "the
// forge could not be asked" (keep it, tell the user to retry).
func (v *verifier) allowed(ctx context.Context, token string) source.Access {
	if v == nil || v.check == nil || token == "" {
		return source.AccessRejected
	}
	sum := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(sum[:])

	v.mu.Lock()
	if got, ok := v.seen[key]; ok && v.now().Before(got.until) {
		v.mu.Unlock()
		return got.access
	}
	if wait, running := v.inflight[key]; running {
		// Someone is already asking the forge this exact question. Wait for
		// their answer instead of asking it again.
		v.mu.Unlock()
		<-wait
		v.mu.Lock()
		got, ok := v.seen[key]
		v.mu.Unlock()
		if ok {
			return got.access
		}
		// The check finished without leaving a verdict: it was cut short, so
		// nothing was learned about this token either.
		return source.AccessUnavailable
	}
	wait := make(chan struct{})
	v.inflight[key] = wait
	v.mu.Unlock()

	// Detach from the caller's context. A client that gave up mid-request would
	// otherwise cancel this call, and the failure would be cached as "this token
	// is refused" -- locking a legitimate viewer out for the whole failure TTL.
	// The timeout keeps a hanging forge from piling up goroutines, and it also
	// bounds how long a caller waits for a free slot.
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), verifyTimeout)
	defer cancel()

	access, cacheable := v.run(checkCtx, token)
	v.finish(key, wait, access, cacheable)
	return access
}

// run performs one check behind the concurrency bound. A check that never
// started, or that the bound cut short, says nothing about the token, so its
// outcome is not cached.
func (v *verifier) run(ctx context.Context, token string) (access source.Access, cacheable bool) {
	select {
	case v.sem <- struct{}{}:
		defer func() { <-v.sem }()
	case <-ctx.Done():
		return source.AccessUnavailable, false
	}
	return v.check(ctx, token), true
}

// finish records the verdict, releases everyone waiting on this token, and
// closes the flight. The verdict is installed before the waiters are woken, so
// a waiter that wakes up finds it.
func (v *verifier) finish(key string, wait chan struct{}, access source.Access, cacheable bool) {
	v.mu.Lock()
	delete(v.inflight, key)
	if cacheable {
		ttl := verifyUnavailableTTL
		switch access {
		case source.AccessOK:
			ttl = verifySuccessTTL
		case source.AccessRejected:
			ttl = verifyFailureTTL
		}
		if len(v.seen) >= verifyMaxEntries {
			v.evictLocked()
		}
		v.seen[key] = verdict{access: access, until: v.now().Add(ttl)}
	}
	v.mu.Unlock()

	close(wait)
}

// evictLocked makes room for one verdict. Expired entries go first; if nothing
// has expired, the one with the least time left is dropped.
//
// Dropping exactly one is the point. Emptying the map instead threw away the
// verdicts of viewers who were still using them, so a burst of wrong tokens
// also made every real viewer wait for a fresh forge check -- and during a
// forge hiccup turned their standing "ok" into "unavailable".
func (v *verifier) evictLocked() {
	now := v.now()
	oldestKey := ""
	var oldestUntil time.Time
	for k, got := range v.seen {
		if now.After(got.until) {
			delete(v.seen, k)
			continue
		}
		if oldestKey == "" || got.until.Before(oldestUntil) {
			oldestKey, oldestUntil = k, got.until
		}
	}
	if oldestKey != "" && len(v.seen) >= verifyMaxEntries {
		delete(v.seen, oldestKey)
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
	access := s.private.allowed(ctx, token)
	if access == source.AccessOK {
		payload.Private = &nav.PrivateView{Total: total, Check: string(access)}
		return payload
	}
	out := payload.Public()
	out.Private = &nav.PrivateView{Total: total, Locked: true, Check: string(access)}
	return out
}
