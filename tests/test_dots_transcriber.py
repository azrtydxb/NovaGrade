from examgrader import dots_transcriber as dt
from examgrader.schemas import TranscribedPaper

QS = [
    {"question_no": "1a", "max_marks": 5, "question_text": "q", "student_answer": "False", "read_confidence": 1.0},
    {"question_no": "1b", "max_marks": 1, "question_text": "q", "student_answer": "True", "read_confidence": 1.0},
]


class FakeOCR:
    def __init__(self, text="1) ... (5 marks) False"):
        self.text, self.calls = text, 0
    def chat_text(self, content, **k):
        self.calls += 1
        return self.text


class FakeStruct:
    def __init__(self, questions):
        self.questions = questions
    def chat_json(self, content, **k):
        return self.questions


def test_ocr_page_returns_raw_text(tmp_path):
    p = tmp_path / "p.png"; p.write_bytes(b"\x89PNG\r\n")
    assert dt.ocr_page(FakeOCR("hello (5 marks)"), str(p)) == "hello (5 marks)"


def test_structure_page_parses_list():
    assert dt.structure_page(FakeStruct(QS), "page text") == QS


def test_transcribe_paper_dots_two_stage(tmp_path):
    p = tmp_path / "page-01.png"; p.write_bytes(b"\x89PNG\r\n")
    ocr = FakeOCR()
    tp = dt.transcribe_paper_dots(ocr, FakeStruct(QS), [str(p)], "Math", "m.pdf", max_workers=1)
    assert isinstance(tp, TranscribedPaper)
    assert [q.question_no for q in tp.questions] == ["1a", "1b"]
    assert ocr.calls == 1  # one OCR call per page


def test_transcribe_paper_dots_isolates_ocr_failure(tmp_path):
    p1 = tmp_path / "page-01.png"; p1.write_bytes(b"\x89PNG\r\n")
    p2 = tmp_path / "page-02.png"; p2.write_bytes(b"\x89PNG\r\n")

    class FlakyOCR:
        def __init__(self): self.n = 0
        def chat_text(self, content, **k):
            self.n += 1
            if self.n == 1:
                raise RuntimeError("ocr down")
            return "page text"

    tp = dt.transcribe_paper_dots(FlakyOCR(), FakeStruct(QS), [str(p1), str(p2)],
                                  "Math", "m.pdf", max_workers=1)
    assert len(tp.questions) == 2  # page 1 OCR failed -> skipped; page 2 ok


# --- hybrid (dots OCR + VLM answers + merge) ---

class FakeVLMAnswers:
    def __init__(self, answers): self.answers = answers; self.calls = 0
    def chat_json(self, content, **k):
        self.calls += 1
        return self.answers


class MergeStruct:
    """Merges OCR text (questions+marks) with the VLM answers passed in the prompt."""
    def chat_json(self, content, **k):
        import json as _json
        text = content[0]["text"]
        ans = _json.loads(text.split("STUDENT_ANSWERS:\n", 1)[1])
        by = {a["question_no"]: a["student_answer"] for a in ans}
        return [{"question_no": "1a", "max_marks": 1, "question_text": "q",
                 "student_answer": by.get("1a", ""), "read_confidence": 1.0}]


def test_read_answers_returns_list(tmp_path):
    p = tmp_path / "p.png"; p.write_bytes(b"\x89PNG\r\n")
    vlm = FakeVLMAnswers([{"question_no": "1a", "student_answer": "B"}])
    assert dt.read_answers(vlm, str(p)) == [{"question_no": "1a", "student_answer": "B"}]


def test_hybrid_merges_vlm_answer_into_question(tmp_path):
    p = tmp_path / "page-01.png"; p.write_bytes(b"\x89PNG\r\n")
    ocr = FakeOCR("1a) ... (1 mark)")
    vlm = FakeVLMAnswers([{"question_no": "1a", "student_answer": "B (circled)"}])
    tp = dt.transcribe_paper_hybrid(ocr, vlm, MergeStruct(), [str(p)], "S", "s.pdf", max_workers=1)
    assert len(tp.questions) == 1
    assert tp.questions[0].student_answer == "B (circled)"  # circled answer from the VLM
    assert tp.questions[0].max_marks == 1                    # mark from the OCR/structurer
    assert vlm.calls == 1
