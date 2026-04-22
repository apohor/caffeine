package beans

import "crypto/rand"

// readRand is a tiny indirection over crypto/rand.Read so we can swap it
// in tests if we ever need deterministic ids. Best-effort: if crypto
// rand fails (never seen in practice) the caller falls back to time.
func readRand(b []byte) (int, error) { return rand.Read(b) }
