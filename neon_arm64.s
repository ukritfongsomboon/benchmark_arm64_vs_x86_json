

#include "textflag.h"

// ARM64 Advanced SIMD (NEON) scanner for JSON string escaping.
//
// func scanNEON(p *byte, n int, escapeHTML bool) int
//
// Returns the index of the first byte in p[0:n] that is NOT safe to copy
// verbatim into a JSON string, or n if all bytes are safe. Requires n >= 16.
//
// Unsafe bytes: '"' (0x22), '\\' (0x5C), 0x00-0x1F, 0x80-0xFF, and when
// escapeHTML != 0 also '<' (0x3C), '>' (0x3E), '&' (0x26). Bytes >= 0x80 are
// reported so the Go side can validate UTF-8 and catch U+2028/U+2029.
//
// Technique: nibble-table classification (as used by simdjson). Each byte b
// is split into hi = b>>4 and lo = b&0x0F. Two 16-entry tables map each
// nibble to a set of class bits; b is unsafe iff HI[hi] & LO[lo] != 0.
//
//   class bit  0x01 CTRL  hi in {0,1},   any lo
//              0x02 HIGH  hi in {8..15}, any lo
//              0x04 QUOTE hi=2, lo=2   (0x22)
//              0x08 AMP   hi=2, lo=6   (0x26, HTML only)
//              0x10 LT    hi=3, lo=C   (0x3C, HTML only)
//              0x20 GT    hi=3, lo=E   (0x3E, HTML only)
//              0x40 BSLASH hi=5, lo=C  (0x5C)
//
// The HTML-only bits are simply absent from the plain HI table, so one code
// path serves both modes. Per 16-byte vector this costs 5 instructions:
//
//   VUSHR $4   (USHR  Vd.16B, Vn.16B, #4)  high nibbles
//   VAND       (AND   Vd.16B, Vn.16B, Vm.16B) low nibbles via 0x0F mask
//   VTBL  x2   (TBL   Vd.16B, {Vn.16B}, Vm.16B) table lookups
//   VAND       (AND)  intersect class bits -> nonzero lane == unsafe byte
//
// The main loop handles 64 bytes (4 vectors) per iteration and ORs the four
// results (VORR) before a single scalar test; the lanes are moved to general
// registers with VMOV Vn.D[i], Rd (UMOV). The first unsafe lane is located
// with RBIT+CLZ (count trailing zeros) on the little-endian 64-bit half.
//
// Instructions deliberately NOT used because the Go assembler (go1.25) does
// not accept them: VCMHI/VCMHS (unsigned compare), VUMAXV, VSHRN, VBIC.
// The table approach needs no compares, so no WORD-encoded instructions are
// required.

// Tables: 16 bytes HI followed by 16 bytes LO, little-endian 8-byte words.
// HI plain: 01 01 04 00 00 40 00 00 | 02 x8
// HI html : 01 01 0C 30 00 40 00 00 | 02 x8
// LO      : 03 03 07 03 03 03 0B 03 | 03 03 03 03 53 03 23 03
DATA tabPlain<>+0x00(SB)/8, $0x0000400000040101
DATA tabPlain<>+0x08(SB)/8, $0x0202020202020202
DATA tabPlain<>+0x10(SB)/8, $0x030B030303070303
DATA tabPlain<>+0x18(SB)/8, $0x0323035303030303
GLOBL tabPlain<>(SB), RODATA|NOPTR, $32

DATA tabHTML<>+0x00(SB)/8, $0x00004000300C0101
DATA tabHTML<>+0x08(SB)/8, $0x0202020202020202
DATA tabHTML<>+0x10(SB)/8, $0x030B030303070303
DATA tabHTML<>+0x18(SB)/8, $0x0323035303030303
GLOBL tabHTML<>(SB), RODATA|NOPTR, $32

// CLASSIFY(in, tmp, out): out lane != 0  <=>  in lane is unsafe.
// Uses V16 = HI table, V17 = LO table, V18 = 0x0F splat.
#define CLASSIFY(in, tmp, out) \
	VUSHR	$4, in.B16, tmp.B16; \
	VAND	V18.B16, in.B16, out.B16; \
	VTBL	tmp.B16, [V16.B16], tmp.B16; \
	VTBL	out.B16, [V17.B16], out.B16; \
	VAND	tmp.B16, out.B16, out.B16

// Branch to found0/found8 if vector v has a nonzero lane (R4 = its base).
#define LOCATE(v) \
	VMOV	v.D[0], R7; \
	CBNZ	R7, found0; \
	VMOV	v.D[1], R7; \
	CBNZ	R7, found8

// Register use:
//   R0 base pointer      R1 n            R2 escapeHTML   R3 table pointer
//   R4 cursor            R5 end of 16B blocks            R6 end of 64B blocks
//   R7,R8 lane scratch   R9 end (p+n)
//   V0-V3 input, V4-V7 temps, V8-V11 class results, V12-V13 OR reduction
TEXT ·scanNEON(SB), NOSPLIT, $0-32
	MOVD	p+0(FP), R0
	MOVD	n+8(FP), R1
	MOVBU	escapeHTML+16(FP), R2

	MOVD	$tabPlain<>(SB), R3
	CBZ	R2, loadtables
	MOVD	$tabHTML<>(SB), R3
loadtables:
	VLD1	(R3), [V16.B16, V17.B16]
	VMOVI	$0x0f, V18.B16

	MOVD	R0, R4
	ADD	R0, R1, R9
	AND	$-64, R1, R6
	ADD	R0, R6, R6
	AND	$-16, R1, R5
	ADD	R0, R5, R5

loop64:
	CMP	R6, R4
	BHS	loop16
	VLD1.P	64(R4), [V0.B16, V1.B16, V2.B16, V3.B16]
	CLASSIFY(V0, V4, V8)
	CLASSIFY(V1, V5, V9)
	CLASSIFY(V2, V6, V10)
	CLASSIFY(V3, V7, V11)
	VORR	V8.B16, V9.B16, V12.B16
	VORR	V10.B16, V11.B16, V13.B16
	VORR	V12.B16, V13.B16, V12.B16
	VMOV	V12.D[0], R7
	VMOV	V12.D[1], R8
	ORR	R7, R8, R7
	CBZ	R7, loop64
	// Hit somewhere in the 64 bytes just loaded: find the first vector.
	SUB	$64, R4, R4
	LOCATE(V8)
	ADD	$16, R4, R4
	LOCATE(V9)
	ADD	$16, R4, R4
	LOCATE(V10)
	ADD	$16, R4, R4
	LOCATE(V11)
	// Unreachable: the OR above was nonzero.
	B	none

loop16:
	CMP	R5, R4
	BHS	tail
	VLD1.P	16(R4), [V0.B16]
	CLASSIFY(V0, V4, V8)
	VMOV	V8.D[0], R7
	VMOV	V8.D[1], R8
	ORR	R7, R8, R10
	CBZ	R10, loop16
	SUB	$16, R4, R4
	CBNZ	R7, found0
	MOVD	R8, R7
	B	found8

tail:
	// R4 == R5. Done if no partial block remains (also covers n == 0).
	CMP	R9, R4
	BEQ	none
	// Partial block: with n < 16 no in-bounds full vector exists, so read
	// nothing (documented contract: caller guarantees n >= 16).
	CMP	$16, R1
	BLT	short
	// Overlapping load of the last 16 bytes p[n-16:n]. The overlapped prefix
	// was already found clean, so the first hit here is the global first.
	SUB	$16, R9, R4
	VLD1	(R4), [V0.B16]
	CLASSIFY(V0, V4, V8)
	VMOV	V8.D[0], R7
	VMOV	V8.D[1], R8
	ORR	R7, R8, R10
	CBZ	R10, none
	CBNZ	R7, found0
	MOVD	R8, R7
	B	found8

none:
	MOVD	R1, ret+24(FP)
	RET

short:
	MOVD	ZR, ret+24(FP)
	RET

found8:
	ADD	$8, R4, R4
found0:
	// R7 = nonzero 64-bit lane mask (lane k = byte k, little-endian).
	// Byte index of the lowest nonzero lane = ctz(R7) / 8.
	RBIT	R7, R7
	CLZ	R7, R7
	ADD	R7>>3, R4, R4
	SUB	R0, R4, R4
	MOVD	R4, ret+24(FP)
	RET
