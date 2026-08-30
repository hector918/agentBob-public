package providers

import (
	"crypto/rand"
	"encoding/binary"
)

// newSeed draws a fresh generation seed. Seeds are NOT exposed as a tool
// parameter (their meaning is invisible to a user), so every call gets a random
// one — which also keeps identical prompts from silently returning the backend's
// cached result instead of a new take on the request.
//
// Bounded to 2^53 so the value survives the JSON round-trip into the workflow
// without precision loss in any float-based consumer along the way.
func newSeed() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 1
	}
	return int64(binary.BigEndian.Uint64(b[:]) & ((1 << 53) - 1))
}
