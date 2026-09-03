// Ported from GNU gzip 1.14, trees.c: output deflated data using Huffman
// coding.
//
// Copyright (C) 1997-1999, 2009-2025 Free Software Foundation, Inc.
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

const (
	maxBits     = 15  // all codes must not exceed maxBits bits
	maxBLBits   = 7   // bit length codes must not exceed maxBLBits bits
	lengthCodes = 29  // number of length codes, not counting END_BLOCK
	literals    = 256 // number of literal bytes 0..255
	endBlock    = 256 // end of block literal code
	lCodes      = literals + 1 + lengthCodes
	dCodes      = 30 // number of distance codes
	blCodes     = 19 // number of codes used to transfer the bit lengths

	storedBlock = 0
	staticTrees = 1
	dynTrees    = 2

	// LIT_BUFSIZE and DIST_BUFSIZE for a build without SMALL_MEM or
	// MEDIUM_MEM, which is what distributions ship.
	litBufsize  = 0x8000
	distBufsize = 0x8000

	rep36     = 16 // repeat previous bit length 3-6 times (2 bits of repeat count)
	repz310   = 17 // repeat a zero length 3-10 times (3 bits of repeat count)
	repz11138 = 18 // repeat a zero length 11-138 times (7 bits of repeat count)

	heapSize = 2*lCodes + 1 // maximum heap size
	smallest = 1            // index within the heap array of the least frequent node
)

var (
	extraLbits  = [lengthCodes]int{0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0}
	extraDbits  = [dCodes]int{0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13}
	extraBlbits = [blCodes]int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 3, 7}

	// blOrder: the lengths of the bit length codes are sent in order of
	// decreasing probability, to avoid transmitting the lengths for unused
	// bit length codes.
	blOrder = [blCodes]uint8{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}
)

// ctData is ct_data. C overlays freq with code and dad with len in unions;
// the port keeps one field for each pair and reads it under whichever
// meaning the C code does at that point, so the aliasing is preserved.
type ctData struct {
	fc uint16 // freq while building the tree, then code
	dl uint16 // dad while building the tree, then len
}

// treeDesc is tree_desc.
type treeDesc struct {
	dynTree    []ctData // the dynamic tree
	staticTree []ctData // corresponding static tree or nil
	extraBits  []int    // extra bits for each code or nil
	extraBase  int      // base index for extraBits
	elems      int      // max number of elements in the tree
	maxLength  int      // max bit length for the codes
	maxCode    int      // largest code with non zero frequency
}

// Tables built once by ctInit for every deflater.
var (
	staticLtree [lCodes + 2]ctData // codes 286 and 287 make the tree canonical
	staticDtree [dCodes]ctData
	lengthCode  [maxMatch - minMatch + 1]uint8 // length code for each normalized match length
	distCode    [512]uint8                     // distance codes: 3..258 then the top 8 bits of 15-bit distances
	baseLength  [lengthCodes]int               // first normalized length for each code
	baseDist    [dCodes]int                    // first normalized distance for each code
)

func init() { ctInit() }

// ctInit is the once-per-process half of ct_init: the static tables.
func ctInit() {
	// Initialize the mapping length (0..255) -> length code (0..28).
	length := 0
	code := 0
	for code = 0; code < lengthCodes-1; code++ {
		baseLength[code] = length
		for n := 0; n < 1<<extraLbits[code]; n++ {
			lengthCode[length] = uint8(code)
			length++
		}
	}
	// Length 255 (match length 258) can be represented in two ways: code
	// 284 + 5 bits or code 285; use the best encoding.
	lengthCode[length-1] = uint8(code)

	// Initialize the mapping dist (0..32K) -> dist code (0..29).
	dist := 0
	for code = 0; code < 16; code++ {
		baseDist[code] = dist
		for n := 0; n < 1<<extraDbits[code]; n++ {
			distCode[dist] = uint8(code)
			dist++
		}
	}
	dist >>= 7 // from now on, all distances are divided by 128
	for ; code < dCodes; code++ {
		baseDist[code] = dist << 7
		for n := 0; n < 1<<(extraDbits[code]-7); n++ {
			distCode[256+dist] = uint8(code)
			dist++
		}
	}

	// Construct the codes of the static literal tree.
	var blCount [maxBits + 1]uint16
	n := 0
	for n <= 143 {
		staticLtree[n].dl = 8
		n++
		blCount[8]++
	}
	for n <= 255 {
		staticLtree[n].dl = 9
		n++
		blCount[9]++
	}
	for n <= 279 {
		staticLtree[n].dl = 7
		n++
		blCount[7]++
	}
	for n <= 287 {
		staticLtree[n].dl = 8
		n++
		blCount[8]++
	}
	genCodes(staticLtree[:], lCodes+1, &blCount)

	// The static distance tree is trivial.
	for n = 0; n < dCodes; n++ {
		staticDtree[n].dl = 5
		staticDtree[n].fc = uint16(biReverse(uint(n), 5))
	}
}

// treeState is the per-file state of trees.c.
type treeState struct {
	dynLtree [heapSize]ctData     // literal and length tree
	dynDtree [2*dCodes + 1]ctData // distance tree
	blTree   [2*blCodes + 1]ctData

	lDesc, dDesc, blDesc treeDesc

	blCount [maxBits + 1]uint16 // number of codes at each bit length for an optimal tree
	heap    [2*lCodes + 1]int   // heap used to build the Huffman trees
	heapLen int                 // number of elements in the heap
	heapMax int                 // element of largest frequency
	depth   [2*lCodes + 1]uint8 // depth of each subtree, tie breaker for equal frequencies

	lBuf      [litBufsize]uint8     // buffer for literals or lengths
	dBuf      [distBufsize]uint16   // buffer for distances
	flagBuf   [litBufsize / 8]uint8 // bit array distinguishing literals from lengths in lBuf
	lastLit   uint                  // running index in lBuf
	lastDist  uint                  // running index in dBuf
	lastFlags uint                  // running index in flagBuf
	flags     uint8                 // current flags not yet saved in flagBuf
	flagBit   uint8                 // current bit used in flags

	optLen        uint64 // bit length of current block with optimal trees
	staticLen     uint64 // bit length of current block with static trees
	compressedLen int64  // total bit length of compressed file
}

// initTrees is the per-file half of ct_init.
func (d *deflater) initTrees() {
	d.lDesc = treeDesc{dynTree: d.dynLtree[:], staticTree: staticLtree[:], extraBits: extraLbits[:], extraBase: literals + 1, elems: lCodes, maxLength: maxBits}
	d.dDesc = treeDesc{dynTree: d.dynDtree[:], staticTree: staticDtree[:], extraBits: extraDbits[:], extraBase: 0, elems: dCodes, maxLength: maxBits}
	d.blDesc = treeDesc{dynTree: d.blTree[:], staticTree: nil, extraBits: extraBlbits[:], extraBase: 0, elems: blCodes, maxLength: maxBLBits}
	d.compressedLen = 0
	d.initBlock()
}

// initBlock is init_block: initialize a new block.
func (d *deflater) initBlock() {
	for n := 0; n < lCodes; n++ {
		d.dynLtree[n].fc = 0
	}
	for n := 0; n < dCodes; n++ {
		d.dynDtree[n].fc = 0
	}
	for n := 0; n < blCodes; n++ {
		d.blTree[n].fc = 0
	}
	d.dynLtree[endBlock].fc = 1
	d.optLen, d.staticLen = 0, 0
	d.lastLit, d.lastDist, d.lastFlags = 0, 0, 0
	d.flags, d.flagBit = 0, 1
}

// smaller compares two subtrees, using the tree depth as tie breaker when
// the subtrees have equal frequency.
func (d *deflater) smaller(tree []ctData, n, m int) bool {
	return tree[n].fc < tree[m].fc || (tree[n].fc == tree[m].fc && d.depth[n] <= d.depth[m])
}

// pqdownheap restores the heap property by moving down the tree starting at
// node k, exchanging a node with the smallest of its two sons if necessary.
func (d *deflater) pqdownheap(tree []ctData, k int) {
	v := d.heap[k]
	j := k << 1 // left son of k
	for j <= d.heapLen {
		// Set j to the smallest of the two sons.
		if j < d.heapLen && d.smaller(tree, d.heap[j+1], d.heap[j]) {
			j++
		}
		// Exit if v is smaller than both sons.
		if d.smaller(tree, v, d.heap[j]) {
			break
		}
		// Exchange v with the smallest son.
		d.heap[k] = d.heap[j]
		k = j
		j <<= 1
	}
	d.heap[k] = v
}

// genBitlen is gen_bitlen: compute the optimal bit lengths for a tree and
// update the total bit length for the current block.
func (d *deflater) genBitlen(desc *treeDesc) {
	tree := desc.dynTree
	extra := desc.extraBits
	base := desc.extraBase
	maxCode := desc.maxCode
	maxLength := desc.maxLength
	stree := desc.staticTree
	overflow := 0 // number of elements with bit length too large

	for bits := 0; bits <= maxBits; bits++ {
		d.blCount[bits] = 0
	}

	// In a first pass, compute the optimal bit lengths (which may overflow
	// in the case of the bit length tree).
	tree[d.heap[d.heapMax]].dl = 0 // root of the heap

	h := d.heapMax + 1
	for ; h < heapSize; h++ {
		n := d.heap[h]
		bits := int(tree[tree[n].dl].dl) + 1 // dad's len
		if bits > maxLength {
			bits = maxLength
			overflow++
		}
		tree[n].dl = uint16(bits) // overwrites dad, which is no longer needed

		if n > maxCode {
			continue // not a leaf node
		}

		d.blCount[bits]++
		xbits := 0
		if n >= base {
			xbits = extra[n-base]
		}
		f := uint64(tree[n].fc)
		d.optLen += f * uint64(bits+xbits)
		if stree != nil {
			d.staticLen += f * uint64(int(stree[n].dl)+xbits)
		}
	}
	if overflow == 0 {
		return
	}

	// Find the first bit length which could increase.
	for {
		bits := maxLength - 1
		for d.blCount[bits] == 0 {
			bits--
		}
		d.blCount[bits]--      // move one leaf down the tree
		d.blCount[bits+1] += 2 // move one overflow item as its brother
		d.blCount[maxLength]--
		overflow -= 2
		if overflow <= 0 {
			break
		}
	}

	// Now recompute all bit lengths, scanning in increasing frequency.
	// h is still equal to heapSize.
	for bits := maxLength; bits != 0; bits-- {
		n := int(d.blCount[bits])
		for n != 0 {
			h--
			m := d.heap[h]
			if m > maxCode {
				continue
			}
			if int(tree[m].dl) != bits {
				d.optLen += uint64((int64(bits) - int64(tree[m].dl)) * int64(tree[m].fc))
				tree[m].dl = uint16(bits)
			}
			n--
		}
	}
}

// genCodes is gen_codes: generate the codes for a given tree and bit counts
// (which need not be optimal).
func genCodes(tree []ctData, maxCode int, blCount *[maxBits + 1]uint16) {
	var nextCode [maxBits + 1]uint16 // next code value for each bit length
	var code uint16                  // running code value

	// The distribution counts are first used to generate the code values
	// without bit reversal.
	for bits := 1; bits <= maxBits; bits++ {
		code = (code + blCount[bits-1]) << 1
		nextCode[bits] = code
	}
	for n := 0; n <= maxCode; n++ {
		length := int(tree[n].dl)
		if length == 0 {
			continue
		}
		tree[n].fc = uint16(biReverse(uint(nextCode[length]), length))
		nextCode[length]++
	}
}

// buildTree is build_tree: construct one Huffman tree and assign the code
// bit strings and lengths. Update the total bit length for the current block.
func (d *deflater) buildTree(desc *treeDesc) {
	tree := desc.dynTree
	stree := desc.staticTree
	elems := desc.elems
	maxCode := -1 // largest code with non zero frequency
	node := elems // next internal node of the tree

	// Construct the initial heap, with least frequent element in
	// heap[smallest]. The sons of heap[n] are heap[2*n] and heap[2*n+1].
	// heap[0] is not used.
	d.heapLen = 0
	d.heapMax = heapSize

	for n := 0; n < elems; n++ {
		if tree[n].fc != 0 {
			d.heapLen++
			d.heap[d.heapLen] = n
			maxCode = n
			d.depth[n] = 0
		} else {
			tree[n].dl = 0
		}
	}

	// The pkzip format requires that at least one distance code exists,
	// and that at least one bit should be sent even if there is only one
	// possible code. So to avoid special checks later on we force at least
	// two codes of non zero frequency.
	for d.heapLen < 2 {
		var nw int
		if maxCode < 2 {
			maxCode++
			nw = maxCode
		}
		d.heapLen++
		d.heap[d.heapLen] = nw
		tree[nw].fc = 1
		d.depth[nw] = 0
		d.optLen--
		if stree != nil {
			d.staticLen -= uint64(stree[nw].dl)
		}
		// nw is 0 or 1 so it does not have extra bits.
	}
	desc.maxCode = maxCode

	// The elements heap[heapLen/2+1 .. heapLen] are leaves of the tree,
	// establish sub-heaps of increasing lengths.
	for n := d.heapLen / 2; n >= 1; n-- {
		d.pqdownheap(tree, n)
	}

	// Construct the Huffman tree by repeatedly combining the least two
	// frequent nodes.
	for {
		// pqremove: n = node of least frequency
		n := d.heap[smallest]
		d.heap[smallest] = d.heap[d.heapLen]
		d.heapLen--
		d.pqdownheap(tree, smallest)
		m := d.heap[smallest] // m = node of next least frequency

		d.heapMax--
		d.heap[d.heapMax] = n // keep the nodes sorted by frequency
		d.heapMax--
		d.heap[d.heapMax] = m

		// Create a new node father of n and m.
		tree[node].fc = tree[n].fc + tree[m].fc
		dn, dm := d.depth[n], d.depth[m]
		if dm >= dn {
			dn = dm
		}
		d.depth[node] = dn + 1
		tree[n].dl = uint16(node)
		tree[m].dl = uint16(node)
		// And insert the new node in the heap.
		d.heap[smallest] = node
		node++
		d.pqdownheap(tree, smallest)

		if d.heapLen < 2 {
			break
		}
	}

	d.heapMax--
	d.heap[d.heapMax] = d.heap[smallest]

	// At this point, the fields freq and dad are set. We can now generate
	// the bit lengths.
	d.genBitlen(desc)

	// The field len is now set, we can generate the bit codes.
	genCodes(tree, maxCode, &d.blCount)
}

// scanTree is scan_tree: scan a literal or distance tree to determine the
// frequencies of the codes in the bit length tree.
func (d *deflater) scanTree(tree []ctData, maxCode int) {
	prevlen := -1              // last emitted length
	nextlen := int(tree[0].dl) // length of next code
	count := 0                 // repeat count of the current code
	maxCount := 7              // max repeat count
	minCount := 4              // min repeat count

	if nextlen == 0 {
		maxCount, minCount = 138, 3
	}
	tree[maxCode+1].dl = 0xffff // guard

	for n := 0; n <= maxCode; n++ {
		curlen := nextlen
		nextlen = int(tree[n+1].dl)
		count++
		if count < maxCount && curlen == nextlen {
			continue
		} else if count < minCount {
			d.blTree[curlen].fc += uint16(count)
		} else if curlen != 0 {
			if curlen != prevlen {
				d.blTree[curlen].fc++
			}
			d.blTree[rep36].fc++
		} else if count <= 10 {
			d.blTree[repz310].fc++
		} else {
			d.blTree[repz11138].fc++
		}
		count = 0
		prevlen = curlen
		if nextlen == 0 {
			maxCount, minCount = 138, 3
		} else if curlen == nextlen {
			maxCount, minCount = 6, 3
		} else {
			maxCount, minCount = 7, 4
		}
	}
}

// sendTree is send_tree: send a literal or distance tree in compressed form,
// using the codes in blTree.
func (d *deflater) sendTree(tree []ctData, maxCode int) {
	prevlen := -1
	nextlen := int(tree[0].dl)
	count := 0
	maxCount := 7
	minCount := 4

	// tree[maxCode+1].dl guard already set by scanTree.
	if nextlen == 0 {
		maxCount, minCount = 138, 3
	}

	for n := 0; n <= maxCode; n++ {
		curlen := nextlen
		nextlen = int(tree[n+1].dl)
		count++
		if count < maxCount && curlen == nextlen {
			continue
		} else if count < minCount {
			for {
				d.sendCode(curlen, d.blTree[:])
				count--
				if count == 0 {
					break
				}
			}
		} else if curlen != 0 {
			if curlen != prevlen {
				d.sendCode(curlen, d.blTree[:])
				count--
			}
			d.sendCode(rep36, d.blTree[:])
			d.sendBits(count-3, 2)
		} else if count <= 10 {
			d.sendCode(repz310, d.blTree[:])
			d.sendBits(count-3, 3)
		} else {
			d.sendCode(repz11138, d.blTree[:])
			d.sendBits(count-11, 7)
		}
		count = 0
		prevlen = curlen
		if nextlen == 0 {
			maxCount, minCount = 138, 3
		} else if curlen == nextlen {
			maxCount, minCount = 6, 3
		} else {
			maxCount, minCount = 7, 4
		}
	}
}

// buildBlTree is build_bl_tree: construct the Huffman tree for the bit
// lengths and return the index in blOrder of the last bit length code to
// send.
func (d *deflater) buildBlTree() int {
	// Determine the bit length frequencies for literal and distance trees.
	d.scanTree(d.dynLtree[:], d.lDesc.maxCode)
	d.scanTree(d.dynDtree[:], d.dDesc.maxCode)

	// Build the bit length tree.
	d.buildTree(&d.blDesc)
	// optLen now includes the length of the tree representations, except
	// the lengths of the bit lengths codes and the 5+5+4 bits for the counts.

	// Determine the number of bit length codes to send. The pkzip format
	// requires that at least 4 bit length codes be sent.
	maxBlindex := blCodes - 1
	for ; maxBlindex >= 3; maxBlindex-- {
		if d.blTree[blOrder[maxBlindex]].dl != 0 {
			break
		}
	}
	// Update optLen to include the bit length tree and counts.
	d.optLen += uint64(3*(maxBlindex+1) + 5 + 5 + 4)
	return maxBlindex
}

// sendAllTrees is send_all_trees: send the header for a block using dynamic
// Huffman trees: the counts, the lengths of the bit length codes, the
// literal tree and the distance tree.
func (d *deflater) sendAllTrees(lcodes, dcodes, blcodes int) {
	d.sendBits(lcodes-257, 5) // not +255 as stated in appnote.txt
	d.sendBits(dcodes-1, 5)
	d.sendBits(blcodes-4, 4) // not -3 as stated in appnote.txt
	for rank := 0; rank < blcodes; rank++ {
		d.sendBits(int(d.blTree[blOrder[rank]].dl), 3)
	}
	d.sendTree(d.dynLtree[:], lcodes-1) // send the literal tree
	d.sendTree(d.dynDtree[:], dcodes-1) // send the distance tree
}

// flushBlock is flush_block: determine the best encoding for the current
// block (dynamic trees, static trees or store) and output it. hasBuf and
// bufStart stand in for the C buf pointer, which is NULL when the block
// start has slid out of the window; storedLen is the block's length in
// input bytes; pad means pad output to a byte boundary; eof means this is
// the last block.
func (d *deflater) flushBlock(hasBuf bool, bufStart uint, storedLen uint64, pad, eof bool) {
	d.flagBuf[d.lastFlags] = d.flags // save the flags for the last 8 items

	// Construct the literal and distance trees.
	d.buildTree(&d.lDesc)
	d.buildTree(&d.dDesc)
	// At this point, optLen and staticLen are the total bit lengths of the
	// compressed block data, excluding the tree representations.

	// Build the bit length tree for the above two trees, and get the index
	// in blOrder of the last bit length code to send.
	maxBlindex := d.buildBlTree()

	// Determine the best encoding. Compute first the block length in bytes.
	optLenb := (d.optLen + 3 + 7) >> 3
	staticLenb := (d.staticLen + 3 + 7) >> 3

	if staticLenb <= optLenb {
		optLenb = staticLenb
	}

	eofBit := 0
	if eof {
		eofBit = 1
	}

	// C's first branch, a stored file instead of a stored block, needs
	// seekable(), which gzip.h defines as 0; it never runs.
	if storedLen+4 <= optLenb && hasBuf {
		// 4: two words for the lengths. The test on hasBuf is only
		// necessary if LIT_BUFSIZE > WSIZE.
		d.sendBits((storedBlock<<1)+eofBit, 3) // send block type
		d.compressedLen = (d.compressedLen + 3 + 7) &^ 7
		d.compressedLen += int64(storedLen+4) << 3
		d.copyBlock(d.window[bufStart:], uint(uint32(storedLen)), true) // with header
	} else if staticLenb == optLenb {
		d.sendBits((staticTrees<<1)+eofBit, 3)
		d.compressBlock(staticLtree[:], staticDtree[:])
		d.compressedLen += 3 + int64(d.staticLen)
	} else {
		d.sendBits((dynTrees<<1)+eofBit, 3)
		d.sendAllTrees(d.lDesc.maxCode+1, d.dDesc.maxCode+1, maxBlindex+1)
		d.compressBlock(d.dynLtree[:], d.dynDtree[:])
		d.compressedLen += 3 + int64(d.optLen)
	}
	d.initBlock()

	if eof {
		d.biWindup()
		d.compressedLen += 7 // align on byte boundary
	} else if pad && d.compressedLen%8 != 0 {
		d.sendBits((storedBlock<<1)+eofBit, 3) // send block type
		d.compressedLen = (d.compressedLen + 3 + 7) &^ 7
		d.copyBlock(nil, 0, true) // with header
	}
}

// ctTally is ct_tally: save the match info and tally the frequency counts.
// Returns non-zero if the current block must be flushed. dist is the
// distance of the matched string; lc is match length - minMatch, or the
// unmatched byte when dist is 0.
func (d *deflater) ctTally(dist uint, lc int) int {
	d.lBuf[d.lastLit] = uint8(lc)
	d.lastLit++
	if dist == 0 {
		// lc is the unmatched char
		d.dynLtree[lc].fc++
	} else {
		// Here, lc is the match length - minMatch
		dist-- // dist = match distance - 1
		d.dynLtree[int(lengthCode[lc])+literals+1].fc++
		d.dynDtree[dCode(dist)].fc++

		d.dBuf[d.lastDist] = uint16(dist)
		d.lastDist++
		d.flags |= d.flagBit
	}
	d.flagBit <<= 1

	// Output the flags if they fill a byte.
	if d.lastLit&7 == 0 {
		d.flagBuf[d.lastFlags] = d.flags
		d.lastFlags++
		d.flags = 0
		d.flagBit = 1
	}
	// Try to guess if it is profitable to stop the current block here.
	if d.level > 2 && d.lastLit&0xfff == 0 {
		// Compute an upper bound for the compressed length.
		outLength := uint64(d.lastLit) * 8
		inLength := uint64(int64(d.strstart) - d.blockStart)
		for dcode := 0; dcode < dCodes; dcode++ {
			outLength += uint64(d.dynDtree[dcode].fc) * uint64(5+extraDbits[dcode])
		}
		outLength >>= 3
		if uint64(d.lastDist) < uint64(d.lastLit)/2 && outLength < inLength/2 {
			return 1
		}
	}
	// We avoid equality with LIT_BUFSIZE because of wraparound at 64K on
	// 16 bit machines and because stored blocks are restricted to 64K-1
	// bytes.
	if d.lastLit == litBufsize-1 || d.lastDist == distBufsize {
		return 1
	}
	return 0
}

// compressBlock is compress_block: send the block data compressed using the
// given Huffman trees.
func (d *deflater) compressBlock(ltree, dtree []ctData) {
	var lx, dx, fx uint // running indexes in lBuf, dBuf and flagBuf
	var flag uint8      // current flags

	if d.lastLit != 0 {
		for {
			if lx&7 == 0 {
				flag = d.flagBuf[fx]
				fx++
			}
			lc := int(d.lBuf[lx])
			lx++
			if flag&1 == 0 {
				d.sendCode(lc, ltree) // send a literal byte
			} else {
				// Here, lc is the match length - minMatch
				code := int(lengthCode[lc])
				d.sendCode(code+literals+1, ltree) // send the length code
				extra := extraLbits[code]
				if extra != 0 {
					lc -= baseLength[code]
					d.sendBits(lc, extra) // send the extra length bits
				}
				dist := uint(d.dBuf[dx])
				dx++
				// Here, dist is the match distance - 1
				code = dCode(dist)
				d.sendCode(code, dtree) // send the distance code
				extra = extraDbits[code]
				if extra != 0 {
					dist -= uint(baseDist[code])
					d.sendBits(int(dist), extra) // send the extra distance bits
				}
			}
			flag >>= 1
			if lx >= d.lastLit {
				break
			}
		}
	}
	d.sendCode(endBlock, ltree)
}

// sendCode is send_code: send a code of the given tree.
func (d *deflater) sendCode(c int, tree []ctData) {
	d.sendBits(int(tree[c].fc), int(tree[c].dl))
}

// dCode is d_code: the distance code for dist, which is the distance - 1.
func dCode(dist uint) int {
	if dist < 256 {
		return int(distCode[dist])
	}
	return int(distCode[256+(dist>>7)])
}
