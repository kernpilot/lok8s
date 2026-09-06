package tilt

// posixCksum is the POSIX `cksum` CRC (the exact algorithm the coreutils
// binary implements): CRC-32 with polynomial 0x04C11DB7, MSB-first,
// zero-initialised, with the data LENGTH appended as its minimal
// little-endian byte sequence, complemented at the end. tilt::port pipes the
// domain through `cksum | awk '{print $1}'` — the derived web port must not
// move under the Go implementation, or every running session would look
// "down" to the port probe.
func posixCksum(data []byte) uint32 {
	var crc uint32
	step := func(b byte) {
		crc ^= uint32(b) << 24
		for i := 0; i < 8; i++ {
			if crc&0x8000_0000 != 0 {
				crc = (crc << 1) ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	for _, b := range data {
		step(b)
	}
	for n := len(data); n != 0; n >>= 8 {
		step(byte(n))
	}
	return ^crc
}
