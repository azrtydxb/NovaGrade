import json
from typing import Protocol

from examgrader.config import SETTINGS
from examgrader.llm_client import text_part
from examgrader.parallel import map_ordered
from examgrader.schemas import (
    GradedPaper, GradedQuestion, TranscribedPaper, TranscribedQuestion,
)

JUDGE_PROMPT = (
    "You are grading one exam question. You are given the question, its maximum marks, "
    "and the student's answer (already transcribed from handwriting). Decide the correct "
    "answer yourself, then award marks. For questions that show working, give partial "
    "credit for correct method. Return ONLY a JSON object with keys: "
    '"awarded_marks" (number, 0..max), '
    '"justification" (one sentence), '
    '"grade_confidence" (0..1).'
)

RUBRIC_PROMPT = (
    "You are grading one exam question strictly against the official marking guide's rubric. "
    "Award marks ONLY as the rubric allows — do not invent your own criteria. "
    "Return ONLY a JSON object with keys: "
    '"awarded_marks" (number, 0..max), "justification" (one sentence), '
    '"grade_confidence" (0..1).'
)


def answer_flags(q: TranscribedQuestion) -> list[str]:
    """Flag a question by the nature of its answer (shared by all mark schemes)."""
    if not q.student_answer.strip():
        return ["blank_answer"]  # legitimate zero, not an OCR problem
    if q.read_confidence < 0.5:
        return ["low_read_confidence"]  # handwriting hard to read — worth a human check
    return []


class MarkScheme(Protocol):
    def grade_question(self, q: TranscribedQuestion) -> GradedQuestion: ...


def _award_from_llm(client, prompt: str, q: TranscribedQuestion, max_marks: float,
                    flags: list[str]) -> GradedQuestion:
    """Run one grading LLM call and map the reply to a GradedQuestion, isolating failures."""
    try:
        r = client.chat_json([text_part(prompt)], max_tokens=400)
        awarded = max(0.0, min(float(r["awarded_marks"]), float(max_marks)))
        return GradedQuestion(
            question_no=q.question_no, section=q.section, max_marks=max_marks,
            awarded_marks=awarded, student_answer=q.student_answer,
            justification=str(r.get("justification", "")),
            grade_confidence=float(r.get("grade_confidence", 0.0)), flags=flags,
        )
    except Exception as e:  # noqa: BLE001 - isolate one question's failure
        return GradedQuestion(
            question_no=q.question_no, section=q.section, max_marks=max_marks,
            awarded_marks=0.0, student_answer=q.student_answer,
            justification=f"grading failed: {e}", grade_confidence=0.0,
            flags=flags + ["grading_failed"],
        )


class LLMJudge:
    """POC mark scheme: the reasoning model decides the correct answer and awards marks.
    Flexible but non-deterministic and unaware of the official mark allocation."""

    def __init__(self, client):
        self.client = client

    def grade_question(self, q: TranscribedQuestion) -> GradedQuestion:
        prompt = (
            f"{JUDGE_PROMPT}\n\n"
            f"Question {q.question_no}: {q.question_text}\n"
            f"Maximum marks: {q.max_marks}\n"
            f"Student answer: {q.student_answer!r}"
        )
        return _award_from_llm(self.client, prompt, q, q.max_marks, answer_flags(q))


class GuideMarkScheme:
    """Grades against an official marking guide (question_no -> entry).

    Objective entries (match exact / exact_ci / set) are graded by deterministic string
    comparison — no LLM, identical every run. Open-ended entries (match rubric) are graded
    by the LLM bounded by the rubric. Questions absent from the guide fall back to `fallback`.
    The guide also supplies the canonical max_marks, so the paper total lands on its true max.
    """

    def __init__(self, guide: dict, fallback: MarkScheme, client=None):
        self.guide = guide
        self.fallback = fallback
        self.client = client

    @classmethod
    def from_file(cls, path: str, fallback: MarkScheme, client=None) -> "GuideMarkScheme":
        with open(path) as f:
            return cls(json.load(f), fallback, client)

    @property
    def total_marks(self) -> float:
        """Sum of the guide's max_marks — the paper's true maximum."""
        return sum(float(e.get("max_marks", 0)) for e in self.guide.values())

    def grade_question(self, q: TranscribedQuestion) -> GradedQuestion:
        entry = self.guide.get(q.question_no)
        if entry is None:
            return self.fallback.grade_question(q)

        max_marks = float(entry.get("max_marks", q.max_marks))
        flags = answer_flags(q)
        answer = q.student_answer.strip()

        if not answer:  # blank -> deterministic zero
            return GradedQuestion(
                question_no=q.question_no, section=q.section, max_marks=max_marks,
                awarded_marks=0.0, student_answer=q.student_answer,
                justification="blank answer", grade_confidence=1.0, flags=flags,
            )

        match = entry.get("match", "exact")
        if match in ("exact", "exact_ci", "set"):
            ok = _objective_match(entry, answer, match)
            return GradedQuestion(
                question_no=q.question_no, section=q.section, max_marks=max_marks,
                awarded_marks=max_marks if ok else 0.0, student_answer=q.student_answer,
                justification="matches marking guide" if ok else "does not match marking guide",
                grade_confidence=1.0, flags=flags,
            )
        if match == "rubric":
            prompt = (
                f"{RUBRIC_PROMPT}\n\n"
                f"Question {q.question_no}: {q.question_text}\n"
                f"Maximum marks: {max_marks}\n"
                f"Marking guide rubric: {entry.get('rubric', '')}\n"
                f"Student answer: {q.student_answer!r}"
            )
            if self.client is None:
                return self.fallback.grade_question(q)
            return _award_from_llm(self.client, prompt, q, max_marks, flags)

        # unknown match type -> safest is to defer to the fallback judge
        return self.fallback.grade_question(q)


def _objective_match(entry: dict, answer: str, match: str) -> bool:
    if match == "set":
        accept = [str(a).strip() for a in entry.get("accept", [])]
        return any(answer.casefold() == a.casefold() for a in accept)
    expected = str(entry.get("answer", "")).strip()
    if match == "exact_ci":
        return answer.casefold() == expected.casefold()
    return answer == expected  # exact


def grade_paper(
    scheme: MarkScheme, paper: TranscribedPaper, max_total: float | None = None,
    max_workers: int | None = None,
) -> GradedPaper:
    workers = SETTINGS.grader_concurrency if max_workers is None else max_workers
    graded = map_ordered(scheme.grade_question, paper.questions, workers)
    section_totals: dict[str, float] = {}
    for g in graded:
        key = g.section or "?"
        section_totals[key] = section_totals.get(key, 0.0) + g.awarded_marks
    total = sum(g.awarded_marks for g in graded)
    # max_total derived from the graded marks so the total can never exceed it
    # (a guide supplies its own total via cli; otherwise sum the questions' max_marks)
    if max_total is None:
        max_total = sum(g.max_marks for g in graded)
    score_100 = round(100 * total / max_total, 1) if max_total else 0.0
    return GradedPaper(
        subject=paper.subject, source_pdf=paper.source_pdf, questions=graded,
        section_totals=section_totals, total=total, max_total=max_total,
        score_100=score_100,
    )
