// Ported from GNU gzip 1.14, bits.c: output variable-length bit strings.
//
// Copyright (C) 1999, 2009-2025 Free Software Foundation, Inc.
// Copyright (C) 1992-1993 Jean-loup Gailly
//
// This program is free software; you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation; either version 3, or (at your option)
// any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package gnugzip

// bufBits is Buf_size: the number of bits held in biBuf.
const bufBits = 16

// bitWriter is the state of bits.c: a 16-bit bit buffer filled from the
// least significant bit, in front of the byte slice that stands in for
// gzip's outbuf. The writer flushes out to its destination.
type bitWriter struct {
	biBuf   uint16 // bi_buf
	biValid int    // bi_valid: number of valid bits in biBuf
	out     []byte
}

// putByte is put_byte: append one byte of compressed output.
func (b *bitWriter) putByte(c byte) { b.out = append(b.out, c) }

// putShort is put_short: append a 16-bit value, least significant byte first.
func (b *bitWriter) putShort(w uint16) { b.out = append(b.out, byte(w), byte(w>>8)) }

// sendBits is send_bits: send value on length bits. length <= 16 and value
// fits in length bits.
func (b *bitWriter) sendBits(value int, length int) {
	// If not enough room in biBuf, use (valid) bits from biBuf and
	// (16 - biValid) bits from value, leaving (width - (16-biValid))
	// unused bits in value.
	if b.biValid > bufBits-length {
		b.biBuf |= uint16(value << b.biValid)
		b.putShort(b.biBuf)
		b.biBuf = uint16(value) >> (bufBits - b.biValid)
		b.biValid += length - bufBits
	} else {
		b.biBuf |= uint16(value << b.biValid)
		b.biValid += length
	}
}

// biReverse is bi_reverse: reverse the first length bits of code.
// 1 <= length <= 15.
func biReverse(code uint, length int) uint {
	var res uint
	for {
		res |= code & 1
		code >>= 1
		res <<= 1
		length--
		if length <= 0 {
			break
		}
	}
	return res >> 1
}

// biWindup is bi_windup: write out any remaining bits in an incomplete byte.
func (b *bitWriter) biWindup() {
	if b.biValid > 8 {
		b.putShort(b.biBuf)
	} else if b.biValid > 0 {
		b.putByte(byte(b.biBuf))
	}
	b.biBuf = 0
	b.biValid = 0
}

// copyBlock is copy_block: copy a stored block, writing first the length and
// its one's complement when header is set. buf holds at least length bytes.
func (b *bitWriter) copyBlock(buf []byte, length uint, header bool) {
	b.biWindup() // align on byte boundary
	if header {
		b.putShort(uint16(length))
		b.putShort(^uint16(length))
	}
	b.out = append(b.out, buf[:length]...)
}
