package pipeline_test

import (
	"context"
	"os/exec"
	"sort"
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
	if len(pages) != 2 {
		t.Fatalf("expected exactly 2 content pages, got %d", len(pages))
	}
}

func TestRenderPDF_numericPageOrder(t *testing.T) {
	// Verify pageIndex helper and numeric sort for both un-padded and zero-padded filenames.
	cases := []struct {
		input []string
		want  []string
	}{
		{
			input: []string{"page-2.png", "page-10.png", "page-1.png"},
			want:  []string{"page-1.png", "page-2.png", "page-10.png"},
		},
		{
			input: []string{"page-02.png", "page-10.png", "page-01.png"},
			want:  []string{"page-01.png", "page-02.png", "page-10.png"},
		},
	}
	for _, tc := range cases {
		got := make([]string, len(tc.input))
		copy(got, tc.input)
		sort.Slice(got, func(i, j int) bool {
			return pipeline.PageIndex(got[i]) < pipeline.PageIndex(got[j])
		})
		for i, name := range got {
			if name != tc.want[i] {
				t.Errorf("position %d: got %q, want %q (full order: %v)", i, name, tc.want[i], got)
			}
		}
	}
}
