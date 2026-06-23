import json
import os
from examgrader import cli
from examgrader.schemas import GradedPaper

_QUESTIONS = [
    {"question_no": "1a", "max_marks": 1, "question_text": "q", "student_answer": "False", "read_confidence": 1.0},
    {"question_no": "1b", "max_marks": 1, "question_text": "q", "student_answer": "True", "read_confidence": 1.0},
    {"question_no": "2a", "max_marks": 1, "question_text": "q", "student_answer": "Principal", "read_confidence": 1.0},
    {"question_no": "3a", "max_marks": 1, "question_text": "q", "student_answer": "Circumference", "read_confidence": 1.0},
]


class _FakeOCR:
    """dots.ocr stand-in: returns faithful page text."""
    def chat_text(self, content, **k):
        return "1a) ... (1 mark) False\n1b) ... (1 mark) True\n2a) ... Principal\n3a) ... Circumference"


class _FakeVLM:
    """qwen3-vl stand-in: returns the student's answers."""
    def chat_json(self, content, **k):
        return [{"question_no": q["question_no"], "student_answer": q["student_answer"]}
                for q in _QUESTIONS]


class _FakeText:
    """grader-model stand-in serving three roles, dispatched by prompt content."""
    def chat_json(self, content, **k):
        t = content[0]["text"]
        if "marks distribution" in t:           # mark-map extraction
            return {"total": None, "sections": {}}
        if "faithful transcription of ONE exam page" in t:   # question structuring
            return list(_QUESTIONS)
        return {"awarded_marks": 1, "justification": "ok", "grade_confidence": 1.0}  # grading


def test_grade_pdf_pipeline(monkeypatch, tmp_path):
    fake_pages = [str(tmp_path / "page-01.png")]
    open(fake_pages[0], "wb").write(b"\x89PNG\r\n")
    monkeypatch.setattr(cli, "render_pdf", lambda *a, **k: fake_pages)

    gp = cli.grade_pdf("Math paper.pdf", "Math", out_dir=str(tmp_path),
                       ocr_client=_FakeOCR(), vlm_client=_FakeVLM(), grader_client=_FakeText())
    assert isinstance(gp, GradedPaper)
    assert gp.total == 4.0  # 4 questions, 1 mark each awarded
    assert os.path.exists(tmp_path / "Math paper.transcript.json")
    assert os.path.exists(tmp_path / "Math paper.results.json")
    assert json.loads((tmp_path / "Math paper.transcript.json").read_text())["questions"]


def test_grade_pdf_from_transcript_with_guide_is_deterministic(tmp_path):
    # write a transcript + a guide; grading twice must give identical, network-free results
    import json
    transcript = {
        "subject": "Math", "source_pdf": "Math paper.pdf",
        "questions": [
            {"section": "A", "question_no": "1a", "max_marks": 5, "question_text": "q",
             "student_answer": "False", "read_confidence": 0.9},
            {"section": "A", "question_no": "1b", "max_marks": 5, "question_text": "q",
             "student_answer": "True", "read_confidence": 0.9},
        ],
    }
    tpath = tmp_path / "Math.transcript.json"
    tpath.write_text(json.dumps(transcript))
    guide = {
        "1a": {"max_marks": 5, "answer": "False", "match": "exact"},
        "1b": {"max_marks": 5, "answer": "False", "match": "exact"},  # student wrong here
    }
    gpath = tmp_path / "Math.guide.json"
    gpath.write_text(json.dumps(guide))

    r1 = cli.grade_pdf(out_dir=str(tmp_path / "o1"), guide_path=str(gpath),
                       transcript_path=str(tpath))
    r2 = cli.grade_pdf(out_dir=str(tmp_path / "o2"), guide_path=str(gpath),
                       transcript_path=str(tpath))
    assert r1.total == 5.0 and r1.max_total == 10.0      # 1a right (5), 1b wrong (0)
    assert r1.score_100 == 50.0
    assert (r1.total, r1.max_total, r1.score_100) == (r2.total, r2.max_total, r2.score_100)


def test_grade_pdf_raises_on_empty_transcript(monkeypatch, tmp_path):
    page = str(tmp_path / "page-01.png"); open(page, "wb").write(b"\x89PNG\r\n")
    monkeypatch.setattr(cli, "render_pdf", lambda *a, **k: [page])

    class EmptyText:  # mark-map -> {}, structuring -> no questions
        def chat_json(self, content, **k):
            return {"total": None, "sections": {}} if "marks distribution" in content[0]["text"] else []

    import pytest
    with pytest.raises(RuntimeError, match="no questions transcribed"):
        cli.grade_pdf("x.pdf", "X", out_dir=str(tmp_path),
                      ocr_client=_FakeOCR(), vlm_client=_FakeVLM(), grader_client=EmptyText())
