package content

import (
	"context"
	"fmt"
	"io"
	"os/exec"

	"super-speedy-search/internal/config"
)

// Extractor turns a document into a plain-text stream for searching.
// PDF extraction is adapter-shaped on purpose: dependency choices differ per
// platform (external pdftotext in Docker, optional on desktop).
type Extractor interface {
	Extract(ctx context.Context, path string) (io.ReadCloser, error)
}

// NewPDFExtractor returns a pdftotext-backed extractor, or nil when PDF
// search is disabled or the binary cannot be found.
func NewPDFExtractor(cfg config.PDF) (Extractor, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	bin := cfg.PdftotextPath
	if bin == "" {
		found, err := exec.LookPath("pdftotext")
		if err != nil {
			return nil, fmt.Errorf("pdf search enabled but pdftotext not found on PATH: %w", err)
		}
		bin = found
	}
	return &pdftotext{bin: bin}, nil
}

type pdftotext struct {
	bin string
}

func (p *pdftotext) Extract(ctx context.Context, path string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, p.bin, "-q", "-enc", "UTF-8", path, "-")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &cmdReader{ReadCloser: out, cmd: cmd}, nil
}

type cmdReader struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (c *cmdReader) Close() error {
	c.ReadCloser.Close()
	return c.cmd.Wait()
}
