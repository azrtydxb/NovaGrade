import json
from examgrader import report
from examgrader.schemas import GradedPaper, GradedQuestion


def _paper():
    return GradedPaper(
        subject="Math", source_pdf="Math paper.pdf",
        questions=[
            GradedQuestion(question_no="1a", section="A", max_marks=5, awarded_marks=5,
                           student_answer="False", justification="correct", grade_confidence=0.9),
            GradedQuestion(question_no="2a", section="B", max_marks=1, awarded_marks=0,
                           student_answer="x", justification="wrong", grade_confidence=0.4,
                           flags=["low_read_confidence"]),
        ],
        section_totals={"A": 5.0, "B": 0.0}, total=5.0, max_total=100.0,
    )


def test_to_json_roundtrips():
    data = json.loads(report.to_json(_paper()))
    assert data["total"] == 5.0
    assert data["questions"][0]["question_no"] == "1a"


def test_to_markdown_has_header_and_flag():
    md = report.to_markdown(_paper())
    assert "Math" in md
    assert "5" in md and "100" in md
    assert "1a" in md and "2a" in md
    assert "⚠" in md  # flagged question marked


def test_write_report_creates_files(tmp_path):
    j, m = report.write_report(_paper(), str(tmp_path))
    assert j.endswith("Math paper.results.json")
    assert m.endswith("Math paper.report.md")
    assert json.loads(open(j).read())["subject"] == "Math"
    assert "Math" in open(m).read()
