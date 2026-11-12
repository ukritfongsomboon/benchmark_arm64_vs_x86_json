//go:build !arm64

package main

// No NEON on this architecture: the /neon/ routes are not registered.
const hasNEON = false

func indexNEON(s string) int { panic("NEON not available on this architecture") }
