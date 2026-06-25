// Package pipeline implements the per-stage processing logic for the NovaGrade
// pipeline. Each function in this package is a pure transformation step that
// can be tested independently of the queue and object-store infrastructure.
package pipeline

import (
	"context"
	"fmt"
	"os"
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
	prefix := filepath.Join(outDir, "page")

	// Resolve pdftoppm — prefer the Homebrew path on macOS, fall back to PATH.
	pdftoppm, err := resolveBinary("pdftoppm", "/opt/homebrew/bin/pdftoppm")
	if err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}

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
	sort.Strings(matches)

	magick, err := resolveBinary("magick", "/opt/homebrew/bin/magick")
	if err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}

	var contentPages []string
	for _, png := range matches {
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
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("render: magick identify %q: %w", pngPath, err)
	}
	mean, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return false, fmt.Errorf("render: parse mean brightness %q: %w", string(out), err)
	}
	return mean >= BlankThreshold, nil
}

// resolveBinary returns preferredPath if it exists and is executable, otherwise
// falls back to exec.LookPath(name). Returns an error if neither is found.
func resolveBinary(name, preferredPath string) (string, error) {
	if _, err := os.Stat(preferredPath); err == nil {
		return preferredPath, nil
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("binary %q not found (tried %s and PATH): %w", name, preferredPath, err)
	}
	return p, nil
}
