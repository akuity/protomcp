package protomcp

import (
	"crypto/sha256"
	"encoding/hex"
)

// ElicitationState derives the opaque RequestState value that binds a
// multi-round-trip elicitation answer to the exact tool call that
// prompted it. Generated elicitation gates publish it on the
// input-required result and require the retry to echo it back alongside
// a matching answer; a stale or replayed answer (for example from a
// CallToolParams struct the SDK's client middleware mutated in place on
// an earlier call) therefore re-prompts instead of silently confirming.
//
// The value is a plain content hash, not an authenticator or a nonce: a
// client willing to lie about the user's answer can compute it, and a
// byte-identical call re-issued with an already-echoed answer will not
// re-prompt. Elicitation is a UX confirmation for honest clients, not a
// server-enforced authorization control — enforce authorization (and,
// where identical replays matter, idempotency) server-side.
func ElicitationState(toolName string, rawArgs []byte) string {
	h := sha256.New()
	h.Write([]byte(toolName))
	h.Write([]byte{0})
	h.Write(rawArgs)
	return hex.EncodeToString(h.Sum(nil))
}
