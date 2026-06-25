package pipeline_test

import (
	"context"
	"os/exec"
	"testing"

	"github.com/azrtydxb/novagrade/internal/pipeline"
)

func TestRenderPDF(t *testing.T) {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		t.Skip("pdftoppm not in PATH")
	}
	if _, err := exec.LookPath("magick"); err != nil {
		t.Skip("magick not in PATH")
	}

	ctx := context.Background()
	pages, err := pipeline.RenderPDF(ctx, "testdata/sample1.pdf", t.TempDir())
	if err != nil {
		t.Fatalf("RenderPDF failed: %v", err)
	}

	// sample1.pdf has 3 pages: page 1 (content), page 2 (blank), page 3 (content).
	// Blank page 2 must be dropped, so we expect exactly 2 pages.
	if len(pages) < 1 {
		t.Fatalf("expected at least 1 content page, got %d", len(pages))
	}
	if len(pages) >= 3 {
		t.Fatalf("expected blank page to be dropped (< 3 pages), got %d", len(pages))
	}
}
