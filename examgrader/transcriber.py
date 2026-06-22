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
    "Also IGNORE the cover/registration page and all exam metadata — do NOT turn the "
    "examination name, subject, date, time, duration, total marks, the conducting "
    "authority, or the candidate's name/index number into questions. "
    "Return ONLY a JSON array; each element has keys: "
    '"section" (the section letter/number this question belongs to, or null), '
    '"question_no" (e.g. "1a"), '
    '"max_marks" (the marks for THIS item only; if one "(N marks)" label covers '
    "several lettered sub-parts a, b, c..., divide N evenly across those sub-parts so "
    "their max_marks sum to N — never give each sub-part the full N; use 0 if no marks "
    "are shown), "
    '"question_text" (the printed question, concise), '
    '"student_answer" (the handwriting transcribed exactly; use an empty string if the '
    "answer space is blank or you cannot read it — never guess or invent an answer), "
    '"read_confidence" (0..1, your confidence in reading the handwriting). '
    "Do not invent questions, options, or answers that are not actually written on this page."
)


def mark_budget_hint(mark_map: dict) -> str:
    """Turn a {total, sections} mark map into a prompt hint that constrains mark allocation."""
    if not mark_map:
        return ""
    parts = []
    if mark_map.get("total") is not None:
        parts.append(f"the whole paper is worth {mark_map['total']:g} marks")
    sections = mark_map.get("sections") or {}
    if sections:
        secs = ", ".join(f"Section {k} = {v:g}" for k, v in sections.items())
        parts.append(f"section budgets are: {secs}")
    if not parts:
        return ""
    return (
        " IMPORTANT mark budget for the whole exam: " + "; ".join(parts) + ". "
        "Assign max_marks so that, across the entire paper, each section's questions sum to "
        "that section's budget and the paper sums to the total — if questions share a "
        "'(N marks)' label, split N across them rather than repeating it."
    )


def transcribe_page(client, png_path: str, extra: str = "") -> list[dict]:
    content = [text_part(TRANSCRIBE_PROMPT + extra), image_part(png_path)]
    result = client.chat_json(content, max_tokens=2000)
    return result if isinstance(result, list) else result.get("questions", [])


def _transcribe_one_page(client, png_path: str, extra: str = "") -> list[TranscribedQuestion]:
    """Transcribe a single page, isolating failures. Returns [] if the page call
    fails; skips individual malformed questions while keeping the valid ones."""
    try:
        raws = transcribe_page(client, png_path, extra)
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


def _dedupe_question_nos(questions: list[TranscribedQuestion]) -> list[TranscribedQuestion]:
    """Make question_no values unique so none is silently lost downstream (guide keys and
    the scaffolder are keyed by question_no). Repeats get a '#N' suffix."""
    seen: dict[str, int] = {}
    for q in questions:
        n = q.question_no
        if n in seen:
            seen[n] += 1
            q.question_no = f"{n}#{seen[n]}"
        else:
            seen[n] = 1
    return questions


def transcribe_paper(
    client, png_paths, subject: str, source_pdf: str, max_workers: int | None = None,
    mark_map: dict | None = None,
) -> TranscribedPaper:
    workers = SETTINGS.vlm_concurrency if max_workers is None else max_workers
    extra = mark_budget_hint(mark_map or {})
    per_page = map_ordered(
        lambda path: _transcribe_one_page(client, path, extra), list(png_paths), workers
    )
    questions = _dedupe_question_nos([q for page_questions in per_page for q in page_questions])
    return TranscribedPaper(subject=subject, source_pdf=source_pdf, questions=questions)


def transcribe_reconciled(
    client, png_paths, subject: str, source_pdf: str, mark_map: dict | None = None,
    max_passes: int = 2, max_workers: int | None = None,
) -> TranscribedPaper:
    """Transcribe up to `max_passes` times, keeping the pass whose total marks best match the
    paper's stated total. Stops early on an exact match. Falls back to a single pass when the
    stated total is unknown."""
    expected = (mark_map or {}).get("total")
    best: TranscribedPaper | None = None
    best_diff: float | None = None
    passes = max_passes if expected is not None else 1
    for i in range(max(1, passes)):
        paper = transcribe_paper(client, png_paths, subject, source_pdf, max_workers, mark_map)
        if expected is None:
            return paper
        diff = abs(sum(q.max_marks for q in paper.questions) - expected)
        if best is None or diff < best_diff:
            best, best_diff = paper, diff
        if diff == 0:
            break
    return best
