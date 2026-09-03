// Command comp-prysm analyzes compressed files and rebuilds them from
// uncompressed content.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/urfave/cli/v2"

	compprysm "github.com/draganm/comp-prysm"
)

func main() {
	if err := newApp().Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newApp() *cli.App {
	return &cli.App{
		Name:  "comp-prysm",
		Usage: "make compressed files reproducible from their content",
		Commands: []*cli.Command{
			{
				Name:      "detect",
				Usage:     "print gzip, zstd or none for a file",
				ArgsUsage: "<file>",
				Action:    detect,
			},
			{
				Name:      "analyze",
				Usage:     "find parameters that reproduce a compressed file",
				ArgsUsage: "<file>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "params", Usage: "write params JSON to `FILE` (default stdout)"},
					&cli.StringFlag{Name: "uncompressed", Usage: "write the uncompressed content to `FILE`"},
					&cli.IntFlag{Name: "parallelism", Usage: "candidates evaluated at once (default: CPUs)"},
					&cli.StringFlag{Name: "temp-dir", Usage: "spool directory for large inputs"},
				},
				Action: analyze,
			},
			{
				Name:      "recompress",
				Usage:     "rebuild a compressed file from params and uncompressed content",
				ArgsUsage: "<uncompressed> <out>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "params", Usage: "params JSON `FILE`", Required: true},
					&cli.BoolFlag{Name: "allow-version-mismatch", Usage: "try even if the engine version differs"},
				},
				Action: recompress,
			},
			{
				Name:   "engines",
				Usage:  "list available engines",
				Action: engines,
			},
		},
	}
}

func detect(c *cli.Context) error {
	if c.NArg() != 1 {
		return cli.Exit("usage: comp-prysm detect <file>", 1)
	}
	f, err := os.Open(c.Args().Get(0))
	if err != nil {
		return err
	}
	defer f.Close()
	format, err := compprysm.Detect(f)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.App.Writer, format)
	return nil
}

func analyze(c *cli.Context) error {
	if c.NArg() != 1 {
		return cli.Exit("usage: comp-prysm analyze [flags] <file>", 1)
	}
	f, err := os.Open(c.Args().Get(0))
	if err != nil {
		return err
	}
	defer f.Close()
	opts := &compprysm.Options{Parallelism: c.Int("parallelism"), TempDir: c.String("temp-dir")}
	if path := c.String("uncompressed"); path != "" {
		out, err := os.Create(path)
		if err != nil {
			return err
		}
		defer out.Close()
		opts.Uncompressed = out
	}
	p, err := compprysm.Analyze(context.Background(), f, opts)
	if err != nil {
		return err
	}
	var w io.Writer = c.App.Writer
	if path := c.String("params"); path != "" {
		pf, err := os.Create(path)
		if err != nil {
			return err
		}
		defer pf.Close()
		w = pf
	}
	return p.Write(w)
}

func recompress(c *cli.Context) error {
	if c.NArg() != 2 {
		return cli.Exit("usage: comp-prysm recompress --params <p.json> <uncompressed> <out>", 1)
	}
	pf, err := os.Open(c.String("params"))
	if err != nil {
		return err
	}
	p, err := compprysm.ReadParams(pf)
	pf.Close()
	if err != nil {
		return err
	}
	in, err := os.Open(c.Args().Get(0))
	if err != nil {
		return err
	}
	defer in.Close()
	outPath := c.Args().Get(1)
	tmp, err := os.CreateTemp(filepath.Dir(outPath), filepath.Base(outPath)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	opts := &compprysm.RecompressOptions{AllowVersionMismatch: c.Bool("allow-version-mismatch")}
	if err := compprysm.Recompress(context.Background(), p, in, tmp, opts); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), outPath)
}

func engines(c *cli.Context) error {
	tw := tabwriter.NewWriter(c.App.Writer, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tFORMAT\tVERSION")
	for _, e := range compprysm.DefaultEngines() {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Name(), e.Format(), e.Version())
	}
	return tw.Flush()
}
