package main

import "unsafe"

const hasNEON = true

// scanNEON (neon_arm64.s) returns the index of the first byte in p[0:n] that
// needs escaping, or n. Requires n >= 16; never reads outside p[0:n].
//
//go:noescape
func scanNEON(p *byte, n int, escapeHTML bool) int

// indexNEON: strings shorter than one 16-byte vector use the scalar loop.
func indexNEON(s string) int {
	if len(s) < 16 {
		return indexScalar(s)
	}
	return scanNEON(unsafe.StringData(s), len(s), true)
}
