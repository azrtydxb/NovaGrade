import json
import os
from examgrader import cli
from examgrader.schemas import GradedPaper


def test_grade_pdf_pipeline(monkeypatch, fake_client_factory, golden_transcript_dict, tmp_path):
    # stub rendering so we don't need poppler in this unit test
    fake_pages = [str(tmp_path / "page-01.png")]
    open(fake_pages[0], "wb").write(b"\x89PNG\r\n")
    monkeypatch.setattr(cli, "content_pages", lambda *a, **k: fake_pages)

    vlm = fake_client_factory([golden_transcript_dict["questions"]])
    # one grader reply per question (4 in the golden fixture)
    grader_replies = [{"awarded_marks": 1, "justification": "ok", "grade_confidence": 1.0}] * 4
    grd = fake_client_factory(grader_replies)

    gp = cli.grade_pdf("Math paper.pdf", "Math", out_dir=str(tmp_path),
                       vlm_client=vlm, grader_client=grd)
    assert isinstance(gp, GradedPaper)
    assert gp.total == 4.0
    # transcript + report artifacts written
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
