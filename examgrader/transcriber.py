import sys

from examgrader.config import SETTINGS
from examgrader.llm_client import image_part, text_part
from examgrader.parallel import map_ordered
from examgrader.schemas import TranscribedPaper, TranscribedQuestion

TRANSCRIBE_PROMPT = (
    "This is one scanned page of a primary-school exam with PRINTED questions and "
    "HANDWRITTEN student answers. Extract ONLY the real questions that have an answer "
    "space a student could write in. "
    "IGNORE instruction text, rubric lines, section-overview lines such as "
    "'Section A: Comprehension (20 marks)', headings, footers, page numbers, and "
    "reading passages that have no answer blank. "
    "Return ONLY a JSON array; each element has keys: "
    '"section" (the section letter/number this question belongs to, or null), '
    '"question_no" (e.g. "1a"), '
    '"max_marks" (number from the printed "(N marks)" label, or 0 if none shown), '
    '"question_text" (the printed question, concise), '
    '"student_answer" (the handwriting transcribed exactly; use an empty string if the '
    "answer space is blank or you cannot read it — never guess or invent an answer), "
    '"read_confidence" (0..1, your confidence in reading the handwriting). '
    "Do not invent questions, options, or answers that are not actually written on this page."
)


def transcribe_page(client, png_path: str) -> list[dict]:
    content = [text_part(TRANSCRIBE_PROMPT), image_part(png_path)]
    result = client.chat_json(content, max_tokens=2000)
    return result if isinstance(result, list) else result.get("questions", [])


def _transcribe_one_page(client, png_path: str) -> list[TranscribedQuestion]:
    """Transcribe a single page, isolating failures. Returns [] if the page call
    fails; skips individual malformed questions while keeping the valid ones."""
    try:
        raws = transcribe_page(client, png_path)
    except Exception as e:  # noqa: BLE001 - a page-level failure must not sink the paper
        print(f"[transcriber] skipped page {png_path}: {e}", file=sys.stderr)
        return []
    out: list[TranscribedQuestion] = []
    for raw in raws:
        try:
            out.append(TranscribedQuestion(**raw))
        except Exception as e:  # noqa: BLE001 - a single bad question must not drop the page
            print(f"[transcriber] skipped question on {png_path}: {e}", file=sys.stderr)
    return out


def transcribe_paper(
    client, png_paths, subject: str, source_pdf: str, max_workers: int | None = None
) -> TranscribedPaper:
    workers = SETTINGS.vlm_concurrency if max_workers is None else max_workers
    per_page = map_ordered(
        lambda path: _transcribe_one_page(client, path), list(png_paths), workers
    )
    questions = [q for page_questions in per_page for q in page_questions]
    return TranscribedPaper(subject=subject, source_pdf=source_pdf, questions=questions)
