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


def test_canonical_section():
    assert markmap.canonical_section("Section A") == "A"
    assert markmap.canonical_section("A") == "A"
    assert markmap.canonical_section("section c") == "C"
    assert markmap.canonical_section(None) is None
    assert markmap.canonical_section("") is None


def test_section_sums_and_reconcile():
    paper = TranscribedPaper(subject="E", source_pdf="e.pdf", questions=[
        _q("1", 10, section="Section A"), _q("2", 12, section="A"), _q("3", 25, section="B"),
    ])
    assert markmap.section_sums(paper.questions) == {"A": 22.0, "B": 25.0}
    rows = markmap.section_reconcile({"sections": {"A": 20, "B": 25}}, paper)
    by = {r["section"]: r for r in rows}
    assert by["A"]["detected"] == 22.0 and by["A"]["ok"] is False and by["A"]["difference"] == 2.0
    assert by["B"]["ok"] is True


def test_transcribe_reconciled_retranscribes_only_off_section_pages(tmp_path):
    # page 0 = Section A (off: 5 vs budget 20), page 1 = Section B (ok: 25)
    p0 = tmp_path / "page-01.png"; p0.write_bytes(b"\x89PNG\r\n")
    p1 = tmp_path / "page-02.png"; p1.write_bytes(b"\x89PNG\r\n")

    def A(marks, no="a"):
        return {"section": "A", "question_no": no, "max_marks": marks, "question_text": "q",
                "student_answer": "x", "read_confidence": 0.9}
    def B(marks, no="b"):
        return {"section": "B", "question_no": no, "max_marks": marks, "question_text": "q",
                "student_answer": "x", "read_confidence": 0.9}

    calls = {"A_pages": 0}

    class ByPage:
        # returns Section-A content for page 0, Section-B for page 1; on the SECOND read of
        # page 0 it finds the full 20 marks (simulating recovery)
        def chat_json(self, content, **k):
            text = content[0]["text"]
            url = content[1]["image_url"]["url"]
            # distinguish page by its bytes
            import base64
            raw = base64.b64decode(url.split(",", 1)[1])
            if raw.endswith(b"\r\n"):  # both share bytes; use a counter on focus instead
                pass
            # page identity via which marks we return: track by a side flag in the prompt
            if "Section A" in text and "Focus" in text:
                calls["A_pages"] += 1
                return [A(20)]            # recovered on the targeted re-read
            # first pass: infer page by call order
            return None

    # simpler deterministic fake keyed by image bytes
    p0.write_bytes(b"\x89PNGAAA"); p1.write_bytes(b"\x89PNGBBB")

    class Fake:
        def chat_json(self, content, **k):
            import base64
            raw = base64.b64decode(content[1]["image_url"]["url"].split(",", 1)[1])
            page = "A" if b"AAA" in raw else "B"
            focus = "Focus on these sections" in content[0]["text"]
            if page == "A":
                return [A(20)] if focus else [A(5)]   # under-detected first, recovered on focus
            return [B(25)]

    paper = transcriber.transcribe_reconciled(
        Fake(), [str(p0), str(p1)], "E", "e.pdf",
        mark_map={"total": 45, "sections": {"A": 20, "B": 25}}, max_passes=2, max_workers=1,
    )
    sums = markmap.section_sums(paper.questions)
    assert sums["A"] == 20.0   # off section A was re-transcribed and recovered
    assert sums["B"] == 25.0   # B was fine, untouched
