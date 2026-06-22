from examgrader import markmap, transcriber
from examgrader.schemas import TranscribedPaper, TranscribedQuestion


def _q(no, max_marks, section=None):
    return TranscribedQuestion(section=section, question_no=no, max_marks=max_marks,
                               question_text="q", student_answer="a", read_confidence=0.9)


def test_extract_mark_map_normalizes(fake_client_factory, tmp_path):
    png = tmp_path / "p.png"; png.write_bytes(b"\x89PNG\r\n")  # image_part reads this
    client = fake_client_factory([{"total": 100, "sections": {"A": 20, "B": 25, "bad": "x"}}])
    mm = markmap.extract_mark_map(client, str(png))
    assert mm["total"] == 100.0
    assert mm["sections"] == {"A": 20.0, "B": 25.0}  # non-numeric dropped


def test_extract_mark_map_handles_failure():
    class Boom:
        def chat_json(self, *a, **k): raise RuntimeError("vlm down")
    assert markmap.extract_mark_map(Boom(), "p.png") == {}


def test_reconcile_flags_mismatch():
    paper = TranscribedPaper(subject="S", source_pdf="s.pdf",
                             questions=[_q("1", 40), _q("2", 30)])
    r = markmap.reconcile({"total": 100.0}, paper)
    assert r["expected_total"] == 100.0 and r["detected_total"] == 70.0
    assert r["difference"] == -30.0 and r["ok"] is False


def test_reconcile_ok_when_match_or_unknown():
    paper = TranscribedPaper(subject="S", source_pdf="s.pdf", questions=[_q("1", 100)])
    assert markmap.reconcile({"total": 100.0}, paper)["ok"] is True
    assert markmap.reconcile({}, paper)["ok"] is True  # no stated total -> nothing to check


def test_mark_budget_hint_mentions_total_and_sections():
    hint = transcriber.mark_budget_hint({"total": 100.0, "sections": {"A": 20.0}})
    assert "100" in hint and "Section A = 20" in hint
    assert transcriber.mark_budget_hint({}) == ""


def test_transcribe_reconciled_keeps_closest_to_total(tmp_path):
    p = tmp_path / "page-01.png"; p.write_bytes(b"\x89PNG\r\n")
    # pass 1 totals 8 (far), pass 2 totals 10 (exact) -> keep pass 2 and stop
    pages_replies = [
        [{"question_no": "1", "max_marks": 8, "question_text": "q",
          "student_answer": "a", "read_confidence": 0.9}],
        [{"question_no": "1", "max_marks": 10, "question_text": "q",
          "student_answer": "a", "read_confidence": 0.9}],
    ]

    class Seq:
        def __init__(self): self.n = 0
        def chat_json(self, content, **k):
            r = pages_replies[self.n]; self.n += 1; return r

    paper = transcriber.transcribe_reconciled(
        Seq(), [str(p)], "S", "s.pdf", mark_map={"total": 10.0}, max_passes=3, max_workers=1
    )
    assert sum(q.max_marks for q in paper.questions) == 10.0
