// pgzipref is the reference producer for the pgzip engine's tests: it
// compresses stdin to stdout with github.com/klauspost/pgzip v1.2.6 over
// github.com/klauspost/compress v1.11.3, the pair umoci (and so rockcraft)
// ships. It is its own module because the main module depends on a newer
// klauspost/compress and Go cannot load two versions of one module.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/pgzip"
)

func main() {
	level := flag.Int("level", pgzip.DefaultCompression, "compression level")
	block := flag.Int("block", 1<<20, "block size in bytes")
	blocks := flag.Int("blocks", 4, "blocks in flight")
	chunk := flag.Int("chunk", 0, "write size (0: one write)")
	flag.Parse()

	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	w, err := pgzip.NewWriterLevel(os.Stdout, *level)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := w.SetConcurrency(*block, *blocks); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for len(in) > 0 {
		n := len(in)
		if *chunk > 0 && *chunk < n {
			n = *chunk
		}
		if _, err := w.Write(in[:n]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		in = in[n:]
	}
	if err := w.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
