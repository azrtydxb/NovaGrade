import os
import pytest
from examgrader import pdf_to_images

MATH = "in/Math paper.pdf"
pytestmark = pytest.mark.skipif(not os.path.exists(MATH), reason="sample PDF absent")


def test_render_pdf_produces_pngs(tmp_path):
    pages = pdf_to_images.render_pdf(MATH, str(tmp_path), dpi=120)
    assert len(pages) >= 3
    assert all(p.endswith(".png") for p in pages)
    assert pages == sorted(pages)
    assert all(os.path.getsize(p) > 0 for p in pages)


def test_content_pages_drops_blanks(tmp_path):
    all_pages = pdf_to_images.render_pdf(MATH, str(tmp_path / "all"), dpi=120)
    content = pdf_to_images.content_pages(MATH, str(tmp_path / "content"), dpi=120)
    assert len(content) >= 1
    assert len(content) < len(all_pages)  # at least one blank page dropped
