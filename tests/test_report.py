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
    assert "Grade: 5 / 100" in md   # 5 awarded out of max_total 100 -> 5/100
    assert "raw 5/100" in md        # raw marks shown alongside
    assert "1a" in md and "2a" in md
    assert "⚠" in md  # flagged question marked


def test_to_markdown_normalizes_to_100():
    # 86 out of a derived max of 145 should headline as ~59/100
    paper = GradedPaper(
        subject="English", source_pdf="e.pdf", questions=[],
        section_totals={}, total=86.0, max_total=145.0,
    )
    md = report.to_markdown(paper)
    assert "Grade: 59.3 / 100" in md
    assert "raw 86/145" in md


def test_blank_answer_flag_not_marked_review():
    # a blank_answer flag is informational and must NOT get the ⚠ review marker
    paper = GradedPaper(
        subject="English", source_pdf="e.pdf",
        questions=[
            GradedQuestion(question_no="1a", section="A", max_marks=2, awarded_marks=0,
                           student_answer="", justification="no answer", grade_confidence=1.0,
                           flags=["blank_answer"]),
        ],
        section_totals={"A": 0.0}, total=0.0, max_total=100.0,
    )
    md = report.to_markdown(paper)
    assert "blank_answer" in md      # the flag is still shown in the table
    assert "⚠" not in md             # but it does not trigger the review marker


def test_write_report_creates_files(tmp_path):
    j, m = report.write_report(_paper(), str(tmp_path))
    assert j.endswith("Math paper.results.json")
    assert m.endswith("Math paper.report.md")
    assert json.loads(open(j).read())["subject"] == "Math"
    assert "Math" in open(m).read()
