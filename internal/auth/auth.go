package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultWindow      = 5 * time.Minute
	signaturePrefix    = "sha256="
	HeaderTimestamp    = "X-Timestamp"
	HeaderSignature    = "X-Signature"
)

var (
	ErrMissingHeader = errors.New("missing signature headers")
	ErrBadTimestamp  = errors.New("invalid timestamp")
	ErrStale         = errors.New("timestamp outside window")
	ErrBadSignature  = errors.New("signature mismatch")
	ErrReplay        = errors.New("replayed request")
)

type Verifier struct {
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	seen map[string]int64 // signature -> timestamp
}

func NewVerifier() *Verifier {
	return &Verifier{
		window: defaultWindow,
		now:    time.Now,
		seen:   make(map[string]int64),
	}
}

func (v *Verifier) Verify(secret, body []byte, timestamp, signature string) error {
	if timestamp == "" || signature == "" {
		return ErrMissingHeader
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrBadTimestamp
	}
	now := v.now().Unix()
	if abs(now-ts) > int64(v.window/time.Second) {
		return ErrStale
	}

	expected := computeSignature(secret, timestamp, body)
	if !hmac.Equal([]byte(signature), []byte(expected)) {
		return ErrBadSignature
	}

	if v.markSeen(signature, ts, now) {
		return ErrReplay
	}
	return nil
}

func computeSignature(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("\n"))
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// Sign is a helper for tests and tooling — not used by the server itself.
func Sign(secret []byte, timestamp string, body []byte) string {
	return computeSignature(secret, timestamp, body)
}

func (v *Verifier) markSeen(signature string, ts, now int64) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.seen[signature]; ok {
		return true
	}
	v.seen[signature] = ts
	v.gcLocked(now)
	return false
}

func (v *Verifier) gcLocked(now int64) {
	cutoff := now - int64(v.window/time.Second)
	for sig, ts := range v.seen {
		if ts < cutoff {
			delete(v.seen, sig)
		}
	}
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// Describe returns a stable, redacted error string for logging without leaking the real signature.
func Describe(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrMissingHeader):
		return "missing-header"
	case errors.Is(err, ErrBadTimestamp):
		return "bad-timestamp"
	case errors.Is(err, ErrStale):
		return "stale"
	case errors.Is(err, ErrBadSignature):
		return "bad-signature"
	case errors.Is(err, ErrReplay):
		return "replay"
	default:
		return fmt.Sprintf("unknown:%s", strings.ReplaceAll(err.Error(), " ", "_"))
	}
}
