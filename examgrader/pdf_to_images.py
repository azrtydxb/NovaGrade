import glob
import os
import subprocess

from examgrader.config import SETTINGS


def render_pdf(pdf_path: str, out_dir: str, dpi: int | None = None) -> list[str]:
    os.makedirs(out_dir, exist_ok=True)
    dpi = dpi or SETTINGS.render_dpi
    prefix = os.path.join(out_dir, "page")
    subprocess.run(
        ["pdftoppm", "-png", "-r", str(dpi), pdf_path, prefix],
        check=True, capture_output=True,
    )
    return sorted(glob.glob(prefix + "-*.png"))


def is_blank(png_path: str, threshold: float = 0.985) -> bool:
    """Blank if the page's mean brightness is near white (scanned empty page)."""
    out = subprocess.run(
        ["magick", "identify", "-format", "%[fx:mean]", png_path],
        check=True, capture_output=True, text=True,
    ).stdout.strip()
    mean = float(out)  # 0..1, 1.0 == pure white
    return mean >= threshold


def content_pages(pdf_path: str, out_dir: str, dpi: int | None = None) -> list[str]:
    return [p for p in render_pdf(pdf_path, out_dir, dpi) if not is_blank(p)]
