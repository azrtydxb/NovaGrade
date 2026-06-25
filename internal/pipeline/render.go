// Package pipeline implements the per-stage processing logic for the NovaGrade
// pipeline. Each function in this package is a pure transformation step that
// can be tested independently of the queue and object-store infrastructure.
package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	// DefaultDPI is the render resolution used when converting PDF pages to PNG
	// images. Mirrors SETTINGS.render_dpi from the Python POC (pdf_to_images.py).
	DefaultDPI = 150

	// BlankThreshold is the mean pixel brightness (0..1, 1=pure white) above
	// which a page is considered blank and excluded from the output.
	// Mirrors the threshold in is_blank() from the Python POC.
	BlankThreshold = 0.985
)

// RenderPDF converts every page of the PDF at pdfPath into a PNG image inside
// outDir (which must already exist or be a t.TempDir() path), then filters out
// near-blank pages using ImageMagick's mean-pixel brightness metric.
//
// It mirrors the Python POC's content_pages() function:
//
//	pdftoppm -png -r <DPI> <pdf> <prefix>   → renders all pages
//	magick identify -format "%[fx:mean]" <png>  → float 0..1; >= BlankThreshold ⟹ blank
//
// The returned slice is sorted in ascending filename order (page-1, page-2, …)
// with blank pages omitted. An error is returned if pdftoppm fails or if any
// magick identify invocation fails.
func RenderPDF(ctx context.Context, pdfPath, outDir string) ([]string, error) {
	// Resolve both binaries via PATH before any subprocess is spawned.
	// Fail fast if either is absent — portable across macOS and Linux.
	// On macOS with Homebrew, ensure /opt/homebrew/bin is in PATH.
	pdftoppm, err := exec.LookPath("pdftoppm")
	if err != nil {
		return nil, fmt.Errorf("render: pdftoppm not found in PATH: %w", err)
	}

	magick, err := exec.LookPath("magick")
	if err != nil {
		return nil, fmt.Errorf("render: magick not found in PATH: %w", err)
	}

	prefix := filepath.Join(outDir, "page")

	// Run: pdftoppm -png -r <DPI> <pdfPath> <prefix>
	cmd := exec.CommandContext(ctx, pdftoppm, "-png", "-r", strconv.Itoa(DefaultDPI), pdfPath, prefix)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("render: pdftoppm: %w\n%s", err, out)
	}

	// Collect generated PNGs (pdftoppm names them <prefix>-<N>.png).
	pattern := prefix + "-*.png"
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("render: glob %q: %w", pattern, err)
	}
	sort.Slice(matches, func(i, j int) bool {
		return PageIndex(matches[i]) < PageIndex(matches[j])
	})

	var contentPages []string
	for _, png := range matches {
		// Check for context cancellation before each page to allow prompt shutdown.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		blank, err := isBlank(ctx, magick, png)
		if err != nil {
			return nil, err
		}
		if !blank {
			contentPages = append(contentPages, png)
		}
	}
	return contentPages, nil
}

// isBlank returns true when the mean pixel brightness of the PNG is at or
// above BlankThreshold, indicating a near-white (empty/scanned-blank) page.
func isBlank(ctx context.Context, magickBin, pngPath string) (bool, error) {
	cmd := exec.CommandContext(ctx, magickBin, "identify", "-format", "%[fx:mean]", pngPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("render: magick identify %q: %w\n%s", pngPath, err, stderr.String())
	}
	mean, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return false, fmt.Errorf("render: parse mean brightness %q: %w", string(out), err)
	}
	return mean >= BlankThreshold, nil
}

// PageIndex extracts the trailing page number from a pdftoppm output filename.
// pdftoppm names files like <prefix>-<N>.png (N may be zero-padded).
// Returns -1 if the integer cannot be parsed, so such files sort first.
func PageIndex(filename string) int {
	base := filepath.Base(filename)
	// Strip extension.
	if idx := strings.LastIndex(base, "."); idx >= 0 {
		base = base[:idx]
	}
	// Find last '-' separator.
	if idx := strings.LastIndex(base, "-"); idx >= 0 {
		n, err := strconv.Atoi(base[idx+1:])
		if err == nil {
			return n
		}
	}
	return -1
}
