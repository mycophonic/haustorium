// Package shared provides common constants for PCM audio analysis.
package shared

// Common constants for PCM audio processing and analysis.
const (
	MaxValue16 = 32768.0      // 2^15 — 16-bit signed PCM normalization divisor
	MaxValue24 = 8388608.0    // 2^23 — 24-bit signed PCM normalization divisor
	MaxValue32 = 2147483648.0 // 2^31 — 32-bit signed PCM normalization divisor

	Shift8       = 8        // bit shift for 24-bit decoding
	Mask24Sign   = 0x800000 // sign bit mask for 24-bit
	Mask24Extend = 0xFFFFFF // sign extension mask for 24-bit

	DBMultiplier   = 20     // 20*log10 for dB conversion
	SilenceFloorDB = -120.0 // silence/minimum dB floor

	Shift16 = 16 // bit shift for 24-bit high-byte decoding

	MsPerSec = 1000 // milliseconds per second

	FullScale  = 1.0   // 0 dBFS reference level
	LufsOffset = 0.691 // EBU R128 LUFS offset
)
