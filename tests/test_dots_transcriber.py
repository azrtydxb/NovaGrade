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


# --- hybrid (dots OCR questions+marks + VLM answers anchored to those question_nos) ---

class FakeVLMAnswers:
    """Returns answers keyed to whatever question_nos it's asked about."""
    def __init__(self, mapping): self.mapping = mapping; self.calls = 0
    def chat_json(self, content, **k):
        self.calls += 1
        return [{"question_no": no, "student_answer": a} for no, a in self.mapping.items()]


class QStruct:
    """Structurer: returns questions + marks (no answers)."""
    def chat_json(self, content, **k):
        return [{"question_no": "1a", "max_marks": 1, "question_text": "q"}]


def test_structure_questions_returns_list():
    assert dt.structure_questions(QStruct(), "page text") == \
        [{"question_no": "1a", "max_marks": 1, "question_text": "q"}]


def test_read_answers_for_returns_keyed_dict(tmp_path):
    p = tmp_path / "p.png"; p.write_bytes(b"\x89PNG\r\n")
    vlm = FakeVLMAnswers({"1a": "B"})
    out = dt.read_answers_for(vlm, str(p), [{"question_no": "1a", "question_text": "q"}])
    assert out == {"1a": "B"}


def test_hybrid_attaches_vlm_answer_to_structured_question(tmp_path):
    p = tmp_path / "page-01.png"; p.write_bytes(b"\x89PNG\r\n")
    ocr = FakeOCR("1a) ... (1 mark)")
    vlm = FakeVLMAnswers({"1a": "B (circled)"})
    tp = dt.transcribe_paper_hybrid(ocr, vlm, QStruct(), [str(p)], "S", "s.pdf", max_workers=1)
    assert len(tp.questions) == 1
    assert tp.questions[0].student_answer == "B (circled)"  # answer from the VLM
    assert tp.questions[0].max_marks == 1                    # mark from dots/structurer
    assert vlm.calls == 1
