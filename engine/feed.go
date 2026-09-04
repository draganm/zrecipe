package engine

import "io"

// FeedSize is the size of the writes Feed hands to an engine writer.
const FeedSize = 32 << 10

// Feed copies src into w in writes of exactly FeedSize bytes; only the last
// write may be shorter. It never takes an io.WriterTo or io.ReaderFrom fast
// path, and it fills each chunk completely before writing it, so the write
// shape the engine sees depends neither on the type nor on the read
// granularity of src.
//
// The search that picks parameters and Recompress that later applies them
// must both feed engine writers through Feed: an engine whose output
// depends on how its input is split across Writes (zlib at level 0 sizes
// stored blocks that way) is otherwise verified under one shape and
// rebuilt under another, and the recorded Params fail to reproduce the
// file.
func Feed(w io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, FeedSize)
	var total int64
	for {
		n, err := io.ReadFull(src, buf)
		if n > 0 {
			m, werr := w.Write(buf[:n])
			total += int64(m)
			if werr != nil {
				return total, werr
			}
			if m != n {
				return total, io.ErrShortWrite
			}
		}
		switch err {
		case nil:
		case io.EOF, io.ErrUnexpectedEOF:
			return total, nil
		default:
			return total, err
		}
	}
}
