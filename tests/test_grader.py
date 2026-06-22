from examgrader import grader
from examgrader.schemas import TranscribedPaper, TranscribedQuestion, GradedQuestion


def _q(no, max_marks, ans, conf=0.9, section="A", text="q"):
    return TranscribedQuestion(section=section, question_no=no, max_marks=max_marks,
                               question_text=text, student_answer=ans, read_confidence=conf)


def test_llmjudge_maps_and_clamps(fake_client_factory):
    client = fake_client_factory([
        {"awarded_marks": 99, "justification": "correct", "grade_confidence": 0.9},
    ])
    judge = grader.LLMJudge(client)
    g = judge.grade_question(_q("1a", 5, "False"))
    assert isinstance(g, GradedQuestion)
    assert g.awarded_marks == 5  # clamped to max_marks
    assert g.justification == "correct"


def test_llmjudge_flags_low_read_confidence(fake_client_factory):
    # non-empty handwriting but low confidence -> human should check the OCR
    client = fake_client_factory([
        {"awarded_marks": 1, "justification": "ok", "grade_confidence": 0.8},
    ])
    g = grader.LLMJudge(client).grade_question(_q("2a", 1, "Principal", conf=0.3))
    assert "low_read_confidence" in g.flags
    assert "blank_answer" not in g.flags


def test_llmjudge_blank_answer_flagged_blank_not_low_confidence(fake_client_factory):
    # an unanswered question (empty, conf 0.0) is a legitimate zero, not an OCR failure
    client = fake_client_factory([
        {"awarded_marks": 0, "justification": "no answer", "grade_confidence": 1.0},
    ])
    g = grader.LLMJudge(client).grade_question(_q("1a", 5, "", conf=0.0))
    assert g.flags == ["blank_answer"]
    assert "low_read_confidence" not in g.flags


def test_llmjudge_handles_call_failure():
    class Boom:
        def chat_json(self, *a, **k): raise RuntimeError("down")
    g = grader.LLMJudge(Boom()).grade_question(_q("3a", 2, "Circumference"))
    assert g.awarded_marks == 0
    assert "grading_failed" in g.flags


def test_grade_paper_totals(fake_client_factory):
    client = fake_client_factory([
        {"awarded_marks": 1, "justification": "a", "grade_confidence": 1.0},
        {"awarded_marks": 0, "justification": "b", "grade_confidence": 1.0},
        {"awarded_marks": 2, "justification": "c", "grade_confidence": 1.0},
    ])
    paper = TranscribedPaper(subject="Math", source_pdf="Math paper.pdf", questions=[
        _q("1a", 1, "False", section="A"),
        _q("1b", 1, "True", section="A"),
        _q("3a", 2, "Circumference", section="B"),
    ])
    # max_workers=1 keeps the order-based queue fake deterministic
    gp = grader.grade_paper(grader.LLMJudge(client), paper, max_workers=1)
    assert gp.total == 3.0
    assert gp.section_totals == {"A": 1.0, "B": 2.0}
    assert gp.max_total == 4.0  # derived from sum of max_marks (1+1+2)


def test_grade_paper_max_total_derived_and_not_exceeded(fake_client_factory):
    # even if the model returns inflated marks, derived max_total >= total
    client = fake_client_factory([
        {"awarded_marks": 5, "justification": "x", "grade_confidence": 1.0},
        {"awarded_marks": 5, "justification": "y", "grade_confidence": 1.0},
    ])
    paper = TranscribedPaper(subject="X", source_pdf="x.pdf", questions=[
        _q("1a", 5, "a", section="A"),
        _q("1b", 5, "b", section="A"),
    ])
    gp = grader.grade_paper(grader.LLMJudge(client), paper, max_workers=1)
    assert gp.max_total == 10.0      # 5 + 5, not hardcoded 100
    assert gp.total <= gp.max_total
    assert gp.score_100 == 100.0     # 10/10 normalized to 100


def test_grade_paper_explicit_max_total_respected(fake_client_factory):
    client = fake_client_factory([
        {"awarded_marks": 1, "justification": "x", "grade_confidence": 1.0},
    ])
    paper = TranscribedPaper(subject="X", source_pdf="x.pdf", questions=[_q("1a", 1, "a")])
    gp = grader.grade_paper(grader.LLMJudge(client), paper, max_total=100.0, max_workers=1)
    assert gp.max_total == 100.0  # explicit override still honored


def test_grade_paper_concurrent_preserves_order_and_totals():
    # Each question is graded concurrently; awarded marks must map to the right
    # question regardless of completion order.
    import time

    class ByQuestion:
        # awards marks equal to the digit in the student's answer; question "qN"
        # answered "N" -> N marks. Earlier questions sleep longer so completion
        # order is reversed from input order.
        def chat_json(self, content, **kw):
            text = content[0]["text"]
            n = int(text.split("Student answer:")[1].strip().strip("'\""))
            time.sleep(0.02 * (5 - n))
            return {"awarded_marks": n, "justification": f"got {n}", "grade_confidence": 1.0}

    paper = TranscribedPaper(subject="Math", source_pdf="m.pdf", questions=[
        _q("q1", 5, "1", section="A"),
        _q("q2", 5, "2", section="A"),
        _q("q3", 5, "3", section="B"),
    ])
    gp = grader.grade_paper(grader.LLMJudge(ByQuestion()), paper, max_workers=4)
    assert [g.awarded_marks for g in gp.questions] == [1.0, 2.0, 3.0]
    assert gp.section_totals == {"A": 3.0, "B": 3.0}
    assert gp.total == 6.0


# --- GuideMarkScheme (deterministic marking-guide grading) ---

class _NoCall:
    """Fallback/grader stand-in that must NOT be called by deterministic paths."""
    def __init__(self): self.calls = 0
    def grade_question(self, q):
        self.calls += 1
        return grader.GradedQuestion(question_no=q.question_no, section=q.section,
            max_marks=q.max_marks, awarded_marks=-1, student_answer=q.student_answer,
            justification="fallback", grade_confidence=0.0, flags=["fallback"])
    def chat_json(self, *a, **k):
        self.calls += 1
        raise AssertionError("LLM should not be called for objective matching")


GUIDE = {
    "1a": {"max_marks": 5, "answer": "False", "match": "exact"},
    "2a": {"max_marks": 1, "answer": "principal", "match": "exact_ci"},
    "3a": {"max_marks": 2, "accept": ["Circumference", "perimeter"], "match": "set"},
    "D1": {"max_marks": 15, "rubric": "content 6, grammar 5, structure 4", "match": "rubric"},
}


def _guide(fallback=None, client=None):
    return grader.GuideMarkScheme(GUIDE, fallback=fallback or _NoCall(), client=client)


def test_guide_exact_match_awards_full_no_llm():
    nc = _NoCall()
    g = grader.GuideMarkScheme(GUIDE, fallback=nc, client=nc).grade_question(_q("1a", 5, "False"))
    assert g.awarded_marks == 5
    assert g.max_marks == 5
    assert g.grade_confidence == 1.0
    assert nc.calls == 0  # purely deterministic, no model call


def test_guide_exact_mismatch_zero():
    g = _guide().grade_question(_q("1a", 5, "True"))
    assert g.awarded_marks == 0


def test_guide_exact_ci_and_set():
    assert _guide().grade_question(_q("2a", 1, "Principal")).awarded_marks == 1   # case-insensitive
    assert _guide().grade_question(_q("3a", 2, "perimeter")).awarded_marks == 2   # accept-list member
    assert _guide().grade_question(_q("3a", 2, "radius")).awarded_marks == 0


def test_guide_blank_is_deterministic_zero_and_flagged():
    g = _guide().grade_question(_q("1a", 5, "", conf=0.0))
    assert g.awarded_marks == 0
    assert g.flags == ["blank_answer"]
    assert g.grade_confidence == 1.0


def test_guide_deterministic_repeat():
    s = _guide()
    a = s.grade_question(_q("1a", 5, "False"))
    b = s.grade_question(_q("1a", 5, "False"))
    assert (a.awarded_marks, a.justification, a.grade_confidence) == \
           (b.awarded_marks, b.justification, b.grade_confidence)


def test_guide_unknown_question_falls_back():
    nc = _NoCall()
    g = grader.GuideMarkScheme(GUIDE, fallback=nc, client=nc).grade_question(_q("99z", 3, "x"))
    assert nc.calls == 1
    assert "fallback" in g.flags


def test_guide_rubric_uses_client_bounded(fake_client_factory):
    client = fake_client_factory([
        {"awarded_marks": 99, "justification": "good essay", "grade_confidence": 0.8},
    ])
    g = grader.GuideMarkScheme(GUIDE, fallback=_NoCall(), client=client).grade_question(
        _q("D1", 15, "A long composition...")
    )
    assert g.awarded_marks == 15  # clamped to guide max_marks
    assert g.max_marks == 15


def test_guide_total_marks():
    assert _guide().total_marks == 23.0  # 5 + 1 + 2 + 15


def test_guide_from_file(tmp_path):
    import json
    p = tmp_path / "Math.guide.json"
    p.write_text(json.dumps({"1a": {"max_marks": 5, "answer": "False", "match": "exact"}}))
    s = grader.GuideMarkScheme.from_file(str(p), fallback=_NoCall())
    assert s.total_marks == 5.0
    assert s.grade_question(_q("1a", 5, "False")).awarded_marks == 5


def test_llm_client_includes_seed(monkeypatch):
    from examgrader import llm_client
    captured = {}

    class FakeResp:
        def raise_for_status(self): pass
        def json(self): return {"choices": [{"message": {"content": "{}"}}]}

    monkeypatch.setattr(llm_client.httpx, "post",
                        lambda url, json=None, timeout=None: captured.update(p=json) or FakeResp())
    llm_client.LLMClient("http://x/v1", "m", seed=0).chat_json("hi")
    assert captured["p"]["seed"] == 0
    assert captured["p"]["temperature"] == 0.0
