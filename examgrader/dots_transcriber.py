"""Two-stage transcription: dots.ocr reads the page faithfully, then a text model structures
it into questions. dots.ocr reads printed marks and handwriting far more accurately than a
general VLM doing structured extraction directly, which fixes the mark mis-attribution.
"""
import json
import re
import sys

from examgrader.config import SETTINGS
from examgrader.llm_client import image_part, text_part
from examgrader.markmap import canonical_section, map_sections
from examgrader.parallel import map_ordered
from examgrader.schemas import TranscribedPaper, TranscribedQuestion
from examgrader.transcriber import _dedupe_question_nos, mark_budget_hint

_SECTION_HEADER_RE = re.compile(r"section\s+([A-D])\b", re.IGNORECASE)


def _detect_section(printed_text: str) -> str | None:
    """Find a 'Section X' header in a page's printed text (sections span pages)."""
    m = _SECTION_HEADER_RE.search(printed_text)
    return m.group(1).upper() if m else None


def _carry_sections_forward(per_page, mark_map) -> list:
    """Pages don't repeat the section header, so questions land under '?'. Walk pages in
    order, remember the last header seen, and stamp it onto questions that lack a valid one."""
    valid = set(map_sections(mark_map or {}))
    current = None
    out = []
    for page_section, questions in per_page:
        if page_section:
            current = page_section
        for q in questions:
            cs = canonical_section(q.section)
            if current and (not cs or (valid and cs not in valid)):
                q.section = current
            out.append(q)
    return out

OCR_PROMPT = (
    "Transcribe this scanned exam page exactly as printed. Preserve every question number, "
    "every printed marks label in parentheses such as (5 marks) or (1 mark), and transcribe "
    "the student's HANDWRITTEN answers in place next to each question. Output faithful "
    "text/markdown — do not summarize, do not invent, do not skip the marks labels."
)

STRUCTURE_PROMPT = (
    "Below is a faithful transcription of ONE exam page: printed questions plus the student's "
    "handwritten answers. Extract the real, answerable questions as a JSON array; each element: "
    '"section" (the section letter/number or null), '
    '"question_no" (e.g. "1a"), '
    '"max_marks" (the marks for THIS item from the printed "(N marks)"; if a single label '
    "covers sub-parts a, b, c, divide it evenly so they sum to N; use 0 if none shown), "
    '"question_text" (the printed question, concise), '
    '"student_answer" (the handwritten answer exactly; empty string if blank), '
    '"read_confidence" (use 1.0). '
    "IGNORE instructions, rubric lines, section-overview lines, the cover/registration page and "
    "exam metadata (examination name, subject, date, time, total marks, authority, candidate "
    "name/index). Return ONLY the JSON array."
)


def ocr_page(ocr_client, png_path: str) -> str:
    """Faithful full-page transcription via dots.ocr."""
    return ocr_client.chat_text([text_part(OCR_PROMPT), image_part(png_path)], max_tokens=4000)


def structure_page(text_client, page_text: str, mark_map: dict | None = None) -> list[dict]:
    """Turn one page's faithful transcription into structured question records."""
    prompt = (
        STRUCTURE_PROMPT + mark_budget_hint(mark_map or {})
        + "\n\nPAGE TRANSCRIPTION:\n" + page_text
    )
    result = text_client.chat_json([text_part(prompt)], max_tokens=2500)
    return result if isinstance(result, list) else result.get("questions", [])


def _one_page(ocr_client, text_client, png_path: str, mark_map) -> list[TranscribedQuestion]:
    try:
        page_text = ocr_page(ocr_client, png_path)
    except Exception as e:  # noqa: BLE001 - a page-level OCR failure must not sink the paper
        print(f"[dots] OCR skipped {png_path}: {e}", file=sys.stderr)
        return []
    try:
        raws = structure_page(text_client, page_text, mark_map)
    except Exception as e:  # noqa: BLE001 - a structuring failure must not sink the paper
        print(f"[dots] structuring skipped {png_path}: {e}", file=sys.stderr)
        return []
    out: list[TranscribedQuestion] = []
    for raw in raws:
        try:
            out.append(TranscribedQuestion(**raw))
        except Exception as e:  # noqa: BLE001 - one bad question must not drop the page
            print(f"[dots] question skipped on {png_path}: {e}", file=sys.stderr)
    return out


def transcribe_paper_dots(
    ocr_client, text_client, png_paths, subject: str, source_pdf: str,
    mark_map: dict | None = None, max_workers: int | None = None,
) -> TranscribedPaper:
    workers = SETTINGS.vlm_concurrency if max_workers is None else max_workers
    per_page = map_ordered(
        lambda p: _one_page(ocr_client, text_client, p, mark_map), list(png_paths), workers
    )
    questions = _dedupe_question_nos([q for page in per_page for q in page])
    return TranscribedPaper(subject=subject, source_pdf=source_pdf, questions=questions)


# --- Hybrid: dots.ocr (printed questions + marks) + qwen3-vl (student answers) ---

ANSWER_PROMPT = (
    "Look at this scanned exam page. For EACH question the student attempted, report the "
    "student's answer EXACTLY: for circle/select/tick questions report which option they "
    "circled or ticked (e.g. \"B\", or the circled word); for written questions report what "
    "they handwrote. Use an empty string for blank questions. "
    'Return ONLY a JSON array: [{"question_no": "1a", "student_answer": "..."}].'
)

HYBRID_STRUCTURE_PROMPT = (
    "You are given two views of ONE exam page:\n"
    "(1) OCR_TRANSCRIPTION — faithful printed text; AUTHORITATIVE for question numbers, "
    "question text, and the printed marks like '(N marks)'.\n"
    "(2) STUDENT_ANSWERS — a vision model's reading of what the student wrote or circled; "
    "AUTHORITATIVE for the student_answer (especially circled/ticked options).\n"
    "Produce a JSON array of the real, answerable questions; each element: "
    '"section" (letter/number or null), "question_no" (e.g. "1a"), '
    '"max_marks" (from the OCR printed marks; if one "(N marks)" label covers sub-parts a,b,c '
    "divide it evenly so they sum to N; 0 if none shown), "
    '"question_text" (concise), '
    '"student_answer" (from STUDENT_ANSWERS, matched by question number; empty if none), '
    '"read_confidence" (use 1.0). '
    "IGNORE instructions, section-overview lines, the cover/registration page and exam "
    "metadata. Return ONLY the JSON array."
)


def read_answers(vlm_client, png_path: str) -> list[dict]:
    """Read the student's answers (incl. circled options) from the page via a vision model."""
    r = vlm_client.chat_json([text_part(ANSWER_PROMPT), image_part(png_path)], max_tokens=2000)
    return r if isinstance(r, list) else r.get("answers", [])


def structure_page_hybrid(text_client, printed_text: str, answers: list,
                          mark_map: dict | None = None) -> list[dict]:
    prompt = (
        HYBRID_STRUCTURE_PROMPT + mark_budget_hint(mark_map or {})
        + "\n\nOCR_TRANSCRIPTION:\n" + printed_text
        + "\n\nSTUDENT_ANSWERS:\n" + json.dumps(answers, ensure_ascii=False)
    )
    result = text_client.chat_json([text_part(prompt)], max_tokens=2500)
    return result if isinstance(result, list) else result.get("questions", [])


def _one_page_hybrid(ocr_client, vlm_client, text_client, png_path, mark_map):
    """Returns (page_section, questions) — page_section is the header on this page, if any."""
    try:
        printed = ocr_page(ocr_client, png_path)
    except Exception as e:  # noqa: BLE001 - OCR failure must not sink the paper
        print(f"[hybrid] OCR skipped {png_path}: {e}", file=sys.stderr)
        return None, []
    page_section = _detect_section(printed)
    try:
        answers = read_answers(vlm_client, png_path)
    except Exception as e:  # noqa: BLE001 - answers are best-effort; keep the questions
        print(f"[hybrid] answers skipped {png_path}: {e}", file=sys.stderr)
        answers = []
    try:
        raws = structure_page_hybrid(text_client, printed, answers, mark_map)
    except Exception as e:  # noqa: BLE001 - structuring failure must not sink the paper
        print(f"[hybrid] structuring skipped {png_path}: {e}", file=sys.stderr)
        return page_section, []
    out: list[TranscribedQuestion] = []
    for raw in raws:
        try:
            out.append(TranscribedQuestion(**raw))
        except Exception as e:  # noqa: BLE001 - one bad question must not drop the page
            print(f"[hybrid] question skipped on {png_path}: {e}", file=sys.stderr)
    return page_section, out


def transcribe_paper_hybrid(
    ocr_client, vlm_client, text_client, png_paths, subject: str, source_pdf: str,
    mark_map: dict | None = None, max_workers: int | None = None,
) -> TranscribedPaper:
    workers = SETTINGS.vlm_concurrency if max_workers is None else max_workers
    per_page = map_ordered(
        lambda p: _one_page_hybrid(ocr_client, vlm_client, text_client, p, mark_map),
        list(png_paths), workers,
    )
    questions = _dedupe_question_nos(_carry_sections_forward(per_page, mark_map))
    return TranscribedPaper(subject=subject, source_pdf=source_pdf, questions=questions)
