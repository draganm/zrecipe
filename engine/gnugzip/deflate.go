// Ported from GNU gzip 1.14, deflate.c: compress data using the deflation
// algorithm.
//
// Copyright (C) 1999, 2006, 2009-2025 Free Software Foundation, Inc.
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

// Sizes for a build without SMALL_MEM or MEDIUM_MEM, which is what
// distributions ship.
const (
	wsize      = 0x8000    // window size, a power of two of at least 32K
	windowSize = 2 * wsize // sliding window: input is read into the second half and moved down
	hashBits   = 15
	hashSize   = 1 << hashBits
	hashMask   = hashSize - 1
	wmask      = wsize - 1
	nilPos     = 0 // tail of hash chains

	minMatch     = 3
	maxMatch     = 258
	minLookahead = maxMatch + minMatch + 1 // minimum lookahead except at end of input
	maxDist      = wsize - minLookahead    // match distances are limited to this
	tooFar       = 4096                    // matches of length 3 are discarded beyond this distance
	rsyncWin     = 4096

	// hShift: number of bits by which insH must be shifted at each input
	// step so that after minMatch steps the oldest byte no longer takes
	// part in the hash key.
	hShift = (hashBits + minMatch - 1) / minMatch

	noChunkEnd = 0xFFFFFFFF // rsyncChunkEnd when no sequence point is pending
)

// config is one row of configuration_table: values for maxLazyMatch,
// goodMatch and maxChainLength depending on the pack level.
type config struct {
	goodLength uint // reduce lazy search above this match length
	maxLazy    uint // do not perform lazy search above this match length
	niceLength uint // quit search above this match length
	maxChain   uint
}

var configurationTable = [10]config{
	/* 0 */ {0, 0, 0, 0}, // store only (rejected by lmInit)
	/* 1 */ {4, 4, 8, 4}, // maximum speed, no lazy matches
	/* 2 */ {4, 5, 16, 8},
	/* 3 */ {4, 6, 32, 32},

	/* 4 */ {4, 4, 16, 16}, // lazy matches
	/* 5 */ {8, 16, 32, 32},
	/* 6 */ {8, 16, 128, 128},
	/* 7 */ {8, 32, 128, 256},
	/* 8 */ {32, 128, 258, 1024},
	/* 9 */ {32, 258, 258, 4096}, // maximum compression
}

// phases of the resumable compressor. GNU gzip's compressor is one loop
// that pulls input from inside; here the loop is unrolled into a state
// machine so that the writer can feed it incrementally.
const (
	phaseInit      = iota // lmInit's first read of 2*wsize is pending
	phaseInitTopUp        // lmInit's top-up loop before hashing the first bytes
	phaseLoop             // run one iteration of the main loop
	phaseTopUp            // end-of-iteration top-up loop
	phaseDone
)

// matchState is the per-file state of deflate.c.
type matchState struct {
	// window is the sliding window, 2*wsize as in C plus two bytes of slack:
	// INSERT_STRING reads window[s+2] for the last positions of the input,
	// where C reads whatever follows its array. Those values never reach the
	// output because matching is off once strstart exceeds
	// windowSize-minLookahead.
	window [windowSize + 2]byte
	prev   [wsize]uint16    // link to older string with same hash index
	head   [hashSize]uint16 // heads of the hash chains or nilPos

	blockStart int64 // window position at the beginning of the current output block; negative after a slide
	insH       uint  // hash index of string to be inserted
	prevLength uint  // length of the best match at previous step
	strstart   uint  // start of string to insert
	matchStart uint  // start of matching string
	eofile     bool  // set at end of input
	lookahead  uint  // number of valid bytes ahead in window

	maxChainLength uint
	maxLazyMatch   uint // also max_insert_length for levels <= 3
	goodMatch      uint
	niceMatch      uint

	rsyncSum      uint64 // rolling sum of rsync window
	rsyncChunkEnd uint64 // next rsync sequence point

	// Locals of deflate_fast / gzip_deflate that live across iterations.
	hashHead       uint
	prevMatch      uint
	flush          int
	matchAvailable bool
	matchLength    uint

	phase int
}

// insertString is INSERT_STRING: insert string s in the dictionary and
// return the previous head of its hash chain.
func (d *deflater) insertString(s uint) uint {
	d.updateHash(d.window[s+minMatch-1])
	matchHead := uint(d.head[d.insH])
	d.prev[s&wmask] = uint16(matchHead)
	d.head[d.insH] = uint16(s)
	return matchHead
}

// updateHash is UPDATE_HASH.
func (d *deflater) updateHash(c byte) {
	d.insH = ((d.insH << hShift) ^ uint(c)) & hashMask
}

// lmInit is lm_init minus its reads, which the phase machine performs.
func (d *deflater) lmInit(packLevel int) {
	// head is zero (nilPos) from allocation; prev is initialized on the fly.
	d.rsyncChunkEnd = noChunkEnd
	d.rsyncSum = 0

	c := configurationTable[packLevel]
	d.maxLazyMatch = c.maxLazy
	d.goodMatch = c.goodLength
	d.niceMatch = c.niceLength
	d.maxChainLength = c.maxChain

	d.strstart = 0
	d.blockStart = 0

	if packLevel <= 3 {
		d.prevLength = minMatch - 1 // deflate_fast
		d.matchLength = 0
	} else {
		d.matchLength = minMatch - 1 // gzip_deflate
	}
}

// longestMatch is longest_match: set matchStart to the longest match
// starting at strstart and return its length. Matches shorter or equal to
// prevLength are discarded, in which case the result equals prevLength and
// matchStart is unchanged. curMatch is the head of the hash chain for the
// current string and its distance is <= maxDist; prevLength >= 1.
func (d *deflater) longestMatch(curMatch uint) uint {
	w := &d.window
	chainLength := d.maxChainLength
	scan := d.strstart
	bestLen := d.prevLength
	var limit uint // stop when curMatch becomes <= limit
	if d.strstart > maxDist {
		limit = d.strstart - maxDist
	}
	strend := d.strstart + maxMatch
	scanEnd1 := w[scan+bestLen-1]
	scanEnd := w[scan+bestLen]

	// Do not waste too much time if we already have a good match.
	if d.prevLength >= d.goodMatch {
		chainLength >>= 2
	}

	for {
		match := curMatch

		// Skip to next match if the match length cannot increase or if the
		// match length is less than 2.
		if w[match+bestLen] == scanEnd && w[match+bestLen-1] == scanEnd1 &&
			w[match] == w[scan] && w[match+1] == w[scan+1] {
			// It is not necessary to compare scan[2] and match[2] since
			// they are always equal when the other bytes match, given that
			// the hash keys are equal and that hashBits >= 8. C compares
			// eight bytes per test and checks for the end only every eighth
			// byte, so it may read scan[258]; the length it reports is the
			// first mismatch from byte 3 on, capped at maxMatch, which is
			// what this loop computes.
			length := uint(minMatch)
			for scan+length < strend && w[scan+length] == w[match+length] {
				length++
			}

			if length > bestLen {
				d.matchStart = curMatch
				bestLen = length
				if length >= d.niceMatch {
					break
				}
				scanEnd1 = w[scan+bestLen-1]
				scanEnd = w[scan+bestLen]
			}
		}

		curMatch = uint(d.prev[curMatch&wmask])
		if curMatch <= limit {
			break
		}
		chainLength--
		if chainLength == 0 {
			break
		}
	}
	return bestLen
}

// fillWindow is fill_window: fill the window when the lookahead becomes
// insufficient, sliding the upper half down when it is almost full. It
// returns false, having done nothing, when the read it needs cannot be
// satisfied yet.
func (d *deflater) fillWindow() bool {
	more := windowSize - d.lookahead - d.strstart // free space at the end of the window
	slide := d.strstart >= wsize+maxDist
	if slide {
		more += wsize
	}
	if !d.eofile && !d.canRead(more) {
		return false
	}

	if slide {
		copy(d.window[:wsize], d.window[wsize:windowSize])
		d.matchStart -= wsize
		d.strstart -= wsize // we now have strstart >= maxDist
		if d.rsyncChunkEnd != noChunkEnd {
			d.rsyncChunkEnd -= wsize
		}
		d.blockStart -= wsize

		for n := range d.head {
			if m := d.head[n]; m >= wsize {
				d.head[n] = m - wsize
			} else {
				d.head[n] = nilPos
			}
		}
		for n := range d.prev {
			if m := d.prev[n]; m >= wsize {
				d.prev[n] = m - wsize
			} else {
				d.prev[n] = nilPos // garbage if n is not on any hash chain, never used
			}
		}
	}
	if !d.eofile {
		n := d.readBuf(d.strstart+d.lookahead, more)
		if n == 0 {
			d.eofile = true
			// Don't let garbage pollute the dictionary.
			d.window[d.strstart+d.lookahead] = 0
			d.window[d.strstart+d.lookahead+1] = 0
		} else {
			d.lookahead += n
		}
	}
	return true
}

// rsyncRoll is rsync_roll: with an initial offset of start, advance rsync's
// rolling checksum by num bytes.
func (d *deflater) rsyncRoll(start, num uint) {
	if start < rsyncWin {
		// before window fills
		for i := start; i < rsyncWin; i++ {
			if i == start+num {
				return
			}
			d.rsyncSum += uint64(d.window[i])
		}
		num -= rsyncWin - start
		start = rsyncWin
	}

	// buffer after window full
	for i := start; i < start+num; i++ {
		d.rsyncSum += uint64(d.window[i])          // new character in
		d.rsyncSum -= uint64(d.window[i-rsyncWin]) // old character out
		if d.rsyncChunkEnd == noChunkEnd && d.rsyncSum%rsyncWin == 0 {
			d.rsyncChunkEnd = uint64(i)
		}
	}
}

// rsyncCheck sets flush to 2 when a sequence point has been passed.
func (d *deflater) rsyncCheck() {
	if d.rsync && uint64(d.strstart) > d.rsyncChunkEnd {
		d.rsyncChunkEnd = noChunkEnd
		d.flush = 2
	}
}

// flushBlockMacro is FLUSH_BLOCK: flush the current block with the given
// end-of-file flag. strstart is set to the end of the current match.
func (d *deflater) flushBlockMacro(eof bool) {
	hasBuf := d.blockStart >= 0
	var bufStart uint
	if hasBuf {
		bufStart = uint(d.blockStart)
	}
	d.flushBlock(hasBuf, bufStart, uint64(int64(d.strstart)-d.blockStart), d.flush-1 != 0, eof)
}

// stepFast is one iteration of deflate_fast, used for levels 1 to 3: no
// lazy evaluation of matches, and new strings are inserted in the
// dictionary only for unmatched strings or for short matches.
func (d *deflater) stepFast() {
	// Insert the string window[strstart .. strstart+2] in the dictionary,
	// and set hashHead to the head of the hash chain.
	d.hashHead = d.insertString(d.strstart)

	// Find the longest match, discarding those <= prevLength. At this point
	// we have always matchLength < minMatch.
	if d.hashHead != nilPos && d.strstart-d.hashHead <= maxDist &&
		d.strstart <= windowSize-minLookahead {
		// To simplify the code, we prevent matches with the string of
		// window index 0 (in particular we have to avoid a match of the
		// string with itself at the start of the input file).
		d.matchLength = d.longestMatch(d.hashHead)
		if d.matchLength > d.lookahead {
			d.matchLength = d.lookahead
		}
	}
	if d.matchLength >= minMatch {
		d.flush = d.ctTally(d.strstart-d.matchStart, int(d.matchLength-minMatch))

		d.lookahead -= d.matchLength

		if d.rsync {
			d.rsyncRoll(d.strstart, d.matchLength)
		}
		// Insert new strings in the hash table only if the match length is
		// not too large. This saves time but degrades compression.
		if d.matchLength <= d.maxLazyMatch {
			d.matchLength-- // string at strstart already in hash table
			for {
				d.strstart++
				d.hashHead = d.insertString(d.strstart)
				// strstart never exceeds wsize-maxMatch, so there are
				// always minMatch bytes ahead. If lookahead < minMatch
				// these bytes are garbage, but it does not matter since
				// the next lookahead bytes will be emitted as literals.
				d.matchLength--
				if d.matchLength == 0 {
					break
				}
			}
			d.strstart++
		} else {
			d.strstart += d.matchLength
			d.matchLength = 0
			d.insH = uint(d.window[d.strstart])
			d.updateHash(d.window[d.strstart+1])
		}
	} else {
		// No match, output a literal byte.
		d.flush = d.ctTally(0, int(d.window[d.strstart]))
		if d.rsync {
			d.rsyncRoll(d.strstart, 1)
		}
		d.lookahead--
		d.strstart++
	}
	d.rsyncCheck()
	if d.flush != 0 {
		d.flushBlockMacro(false)
		d.blockStart = int64(d.strstart)
	}
}

// stepLazy is one iteration of gzip_deflate's loop, used for levels 4 to
// 9: a match is finally adopted only if there is no better match at the
// next window position.
func (d *deflater) stepLazy() {
	// Insert the string window[strstart .. strstart+2] in the dictionary,
	// and set hashHead to the head of the hash chain.
	d.hashHead = d.insertString(d.strstart)

	// Find the longest match, discarding those <= prevLength.
	d.prevLength = d.matchLength
	d.prevMatch = d.matchStart
	d.matchLength = minMatch - 1

	if d.hashHead != nilPos && d.prevLength < d.maxLazyMatch &&
		d.strstart-d.hashHead <= maxDist &&
		d.strstart <= windowSize-minLookahead {
		// To simplify the code, we prevent matches with the string of
		// window index 0 (in particular we have to avoid a match of the
		// string with itself at the start of the input file).
		d.matchLength = d.longestMatch(d.hashHead)
		if d.matchLength > d.lookahead {
			d.matchLength = d.lookahead
		}

		// Ignore a length 3 match if it is too distant.
		if d.matchLength == minMatch && d.strstart-d.matchStart > tooFar {
			// If prevMatch is also minMatch, matchStart is garbage but we
			// will ignore the current match anyway.
			d.matchLength--
		}
	}
	// If there was a match at the previous step and the current match is
	// not better, output the previous match.
	if d.prevLength >= minMatch && d.matchLength <= d.prevLength {
		d.flush = d.ctTally(d.strstart-1-d.prevMatch, int(d.prevLength-minMatch))

		// Insert in hash table all strings up to the end of the match.
		// strstart-1 and strstart are already inserted.
		d.lookahead -= d.prevLength - 1
		d.prevLength -= 2
		if d.rsync {
			d.rsyncRoll(d.strstart, d.prevLength+1)
		}
		for {
			d.strstart++
			d.hashHead = d.insertString(d.strstart)
			// strstart never exceeds wsize-maxMatch, so there are always
			// minMatch bytes ahead. If lookahead < minMatch these bytes are
			// garbage, but it does not matter since the next lookahead
			// bytes will always be emitted as literals.
			d.prevLength--
			if d.prevLength == 0 {
				break
			}
		}
		d.matchAvailable = false
		d.matchLength = minMatch - 1
		d.strstart++

		d.rsyncCheck()
		if d.flush != 0 {
			d.flushBlockMacro(false)
			d.blockStart = int64(d.strstart)
		}
	} else if d.matchAvailable {
		// If there was no match at the previous position, output a single
		// literal. If there was a match but the current match is longer,
		// truncate the previous match to a single literal.
		d.flush = d.ctTally(0, int(d.window[d.strstart-1]))
		d.rsyncCheck()
		if d.flush != 0 {
			d.flushBlockMacro(false)
			d.blockStart = int64(d.strstart)
		}
		if d.rsync {
			d.rsyncRoll(d.strstart, 1)
		}
		d.strstart++
		d.lookahead--
	} else {
		// There is no previous match to compare with, wait for the next
		// step to decide.
		if d.rsync && uint64(d.strstart) > d.rsyncChunkEnd {
			// Reset huffman tree
			d.rsyncChunkEnd = noChunkEnd
			d.flush = 2
			d.flushBlockMacro(false)
			d.blockStart = int64(d.strstart)
		}

		d.matchAvailable = true
		if d.rsync {
			d.rsyncRoll(d.strstart, 1)
		}
		d.strstart++
		d.lookahead--
	}
}

// finish is the tail of deflate_fast / gzip_deflate once lookahead is 0.
func (d *deflater) finish() {
	if d.level > 3 && d.matchAvailable {
		d.ctTally(0, int(d.window[d.strstart-1]))
	}
	d.flushBlockMacro(true) // eof
}

// processResult says why process returned.
type processResult int

const (
	needInput  processResult = iota // a read cannot be satisfied yet
	outputFull                      // out has reached outFlush; flush and call again
	finished                        // the final block has been written
)

// process runs the compressor until it needs more input, has produced
// enough output for the writer to flush, or is done. It is the state
// machine form of gzip_deflate and its callees.
func (d *deflater) process() processResult {
	for {
		switch d.phase {
		case phaseInit:
			// lm_init: lookahead = read_buf(window, 2*WSIZE)
			if !d.canRead(windowSize) {
				return needInput
			}
			d.lookahead = d.readBuf(0, windowSize)
			if d.lookahead == 0 {
				d.eofile = true
				d.phase = phaseLoop // lm_init returns without hashing
				continue
			}
			d.eofile = false
			d.phase = phaseInitTopUp
		case phaseInitTopUp:
			// Make sure that we always have enough lookahead. This is
			// important if input comes from a device such as a tty.
			for d.lookahead < minLookahead && !d.eofile {
				if !d.fillWindow() {
					return needInput
				}
			}
			d.insH = 0
			for j := uint(0); j < minMatch-1; j++ {
				d.updateHash(d.window[j])
			}
			// If lookahead < minMatch, insH is garbage, but this is not
			// important since only literal bytes will be emitted.
			d.phase = phaseLoop
		case phaseLoop:
			if d.lookahead == 0 {
				d.finish()
				d.phase = phaseDone
				return finished
			}
			if d.level <= 3 {
				d.stepFast()
			} else {
				d.stepLazy()
			}
			d.phase = phaseTopUp
			if len(d.out) >= outFlush {
				return outputFull
			}
		case phaseTopUp:
			// Make sure that we always have enough lookahead, except at the
			// end of the input file. We need maxMatch bytes for the next
			// match, plus minMatch bytes to insert the string following the
			// next match.
			for d.lookahead < minLookahead && !d.eofile {
				if !d.fillWindow() {
					return needInput
				}
			}
			d.phase = phaseLoop
		default: // phaseDone
			return finished
		}
	}
}
