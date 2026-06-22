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
