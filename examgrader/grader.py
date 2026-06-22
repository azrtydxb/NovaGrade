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


class MarkScheme(Protocol):
    def grade_question(self, q: TranscribedQuestion) -> GradedQuestion: ...


class LLMJudge:
    def __init__(self, client):
        self.client = client

    def grade_question(self, q: TranscribedQuestion) -> GradedQuestion:
        flags: list[str] = []
        if not q.student_answer.strip():
            # a blank answer is a legitimate zero, not an OCR problem
            flags.append("blank_answer")
        elif q.read_confidence < 0.5:
            # handwriting was present but hard to read — worth a human check
            flags.append("low_read_confidence")
        prompt = (
            f"{JUDGE_PROMPT}\n\n"
            f"Question {q.question_no}: {q.question_text}\n"
            f"Maximum marks: {q.max_marks}\n"
            f"Student answer: {q.student_answer!r}"
        )
        try:
            r = self.client.chat_json([text_part(prompt)], max_tokens=400)
            awarded = max(0.0, min(float(r["awarded_marks"]), float(q.max_marks)))
            return GradedQuestion(
                question_no=q.question_no, section=q.section, max_marks=q.max_marks,
                awarded_marks=awarded, student_answer=q.student_answer,
                justification=str(r.get("justification", "")),
                grade_confidence=float(r.get("grade_confidence", 0.0)),
                flags=flags,
            )
        except Exception as e:  # noqa: BLE001 - isolate one question's failure
            return GradedQuestion(
                question_no=q.question_no, section=q.section, max_marks=q.max_marks,
                awarded_marks=0.0, student_answer=q.student_answer,
                justification=f"grading failed: {e}", grade_confidence=0.0,
                flags=flags + ["grading_failed"],
            )


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
    # max_total derived from the paper's own marks so the total can never exceed it
    if max_total is None:
        max_total = sum(q.max_marks for q in paper.questions)
    score_100 = round(100 * total / max_total, 1) if max_total else 0.0
    return GradedPaper(
        subject=paper.subject, source_pdf=paper.source_pdf, questions=graded,
        section_totals=section_totals, total=total, max_total=max_total,
        score_100=score_100,
    )
