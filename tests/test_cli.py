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
