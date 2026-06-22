"""Extract the exam's stated marks distribution and use it to validate transcription.

NESA papers print their mark budget up front ("Section A (20 marks) ... Total /100"). We read
that once, then check the transcribed per-question marks against it — a mismatch means
transcription missed or mis-marked questions, and triggers another pass.
"""
import re

from examgrader.llm_client import image_part, text_part
from examgrader.schemas import TranscribedPaper

MARKMAP_PROMPT = (
    "This is the front/instructions page of an exam. Extract the OFFICIAL marks distribution "
    "printed here. Return ONLY a JSON object: "
    '{"total": <total marks as a number, or null>, '
    '"sections": {"A": <marks>, "B": <marks>, ...}}. '
    "Use the section labels exactly as printed (e.g. A, B, C, D). If the paper has no labelled "
    "sections, return \"sections\": {}. Do not invent values not printed on the page."
)


def extract_mark_map(client, png_path: str) -> dict:
    """Read the stated {total, sections} off the instructions page. Returns {} on failure."""
    try:
        r = client.chat_json([text_part(MARKMAP_PROMPT), image_part(png_path)], max_tokens=400)
    except Exception:  # noqa: BLE001 - a missing mark map just disables reconciliation
        return {}
    if not isinstance(r, dict):
        return {}
    total = r.get("total")
    sections = r.get("sections") or {}
    out: dict = {}
    if isinstance(total, (int, float)):
        out["total"] = float(total)
    out["sections"] = {str(k): float(v) for k, v in sections.items()
                       if isinstance(v, (int, float))}
    return out


def detected_total(paper: TranscribedPaper) -> float:
    return sum(q.max_marks for q in paper.questions)


def canonical_section(section: str | None) -> str | None:
    """Normalize a transcribed section label to a budget key: 'Section A' / 'A' -> 'A'."""
    if not section:
        return None
    s = re.sub(r"section", "", str(section), flags=re.IGNORECASE).strip()
    return s.split()[0].upper() if s else None


def map_sections(mark_map: dict) -> dict:
    """Section budgets keyed by canonical label, dropping blanks."""
    out = {}
    for k, v in (mark_map.get("sections") or {}).items():
        ck = canonical_section(k)
        if ck:
            out[ck] = float(v)
    return out


def section_sums(questions) -> dict:
    """Detected marks per canonical section ('?' for unlabelled)."""
    sums: dict[str, float] = {}
    for q in questions:
        key = canonical_section(q.section) or "?"
        sums[key] = sums.get(key, 0.0) + q.max_marks
    return sums


def section_reconcile(mark_map: dict, paper: TranscribedPaper) -> list[dict]:
    """Per-section expected vs detected, for the sections the paper states a budget for."""
    budgets = map_sections(mark_map)
    detected = section_sums(paper.questions)
    rows = []
    for sec in sorted(budgets):
        exp, det = budgets[sec], detected.get(sec, 0.0)
        rows.append({"section": sec, "expected": exp, "detected": det,
                     "difference": det - exp, "ok": det == exp})
    return rows


def reconcile(mark_map: dict, paper: TranscribedPaper) -> dict:
    """Compare the stated total against the transcribed total."""
    expected = mark_map.get("total")
    detected = detected_total(paper)
    ok = expected is None or detected == expected
    return {
        "expected_total": expected,
        "detected_total": detected,
        "difference": None if expected is None else detected - expected,
        "ok": ok,
    }
