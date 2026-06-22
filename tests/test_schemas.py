import pytest
from pydantic import ValidationError
from examgrader.schemas import (
    TranscribedQuestion, TranscribedPaper, GradedQuestion, GradedPaper,
)


def test_transcribed_question_roundtrip():
    q = TranscribedQuestion(
        section="A", question_no="1a", max_marks=5,
        question_text="(-3) x (-2) = -6", student_answer="False", read_confidence=0.9,
    )
    assert q.max_marks == 5.0
    assert q.read_confidence == 0.9


def test_read_confidence_bounds_enforced():
    with pytest.raises(ValidationError):
        TranscribedQuestion(
            question_no="1", max_marks=1, question_text="x",
            student_answer="y", read_confidence=1.5,
        )


def test_graded_paper_defaults_flags_empty():
    g = GradedQuestion(
        question_no="1", max_marks=5, awarded_marks=3,
        student_answer="False", justification="ok", grade_confidence=0.8,
    )
    assert g.flags == []


def test_papers_nest():
    tp = TranscribedPaper(
        subject="Math", source_pdf="Math paper.pdf",
        questions=[TranscribedQuestion(question_no="1", max_marks=1,
                   question_text="x", student_answer="y", read_confidence=0.5)],
    )
    assert tp.questions[0].question_no == "1"
    gp = GradedPaper(
        subject="Math", source_pdf="Math paper.pdf", questions=[],
        section_totals={"A": 3.0}, total=3.0, max_total=100.0,
    )
    assert gp.section_totals["A"] == 3.0
