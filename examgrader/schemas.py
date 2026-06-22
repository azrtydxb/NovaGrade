from pydantic import BaseModel, Field


class TranscribedQuestion(BaseModel):
    section: str | None = None
    question_no: str
    max_marks: float
    question_text: str
    student_answer: str
    read_confidence: float = Field(ge=0.0, le=1.0)


class TranscribedPaper(BaseModel):
    subject: str
    source_pdf: str
    questions: list[TranscribedQuestion]


class GradedQuestion(BaseModel):
    question_no: str
    section: str | None = None
    max_marks: float
    awarded_marks: float
    student_answer: str
    justification: str
    grade_confidence: float = Field(ge=0.0, le=1.0)
    flags: list[str] = Field(default_factory=list)


class GradedPaper(BaseModel):
    subject: str
    source_pdf: str
    questions: list[GradedQuestion]
    section_totals: dict[str, float]
    total: float
    max_total: float
    score_100: float = 0.0  # total normalized to a 0–100 scale
