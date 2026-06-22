from examgrader import grader
from examgrader.guide_scaffold import scaffold_from_transcript
from examgrader.schemas import TranscribedPaper, TranscribedQuestion


def _q(no, max_marks, ans):
    return TranscribedQuestion(question_no=no, max_marks=max_marks, question_text="q",
                               student_answer=ans, read_confidence=0.9)


def _paper():
    return TranscribedPaper(subject="Math", source_pdf="m.pdf", questions=[
        _q("1a", 1, "False"),
        _q("D1", 15, "an essay"),
        _q("1a", 1, "dupe"),  # duplicate question_no must be ignored
    ])


def test_scaffold_objective_vs_rubric_and_dedup():
    g = scaffold_from_transcript(_paper())
    assert set(g) == {"1a", "D1"}                 # duplicate collapsed
    assert g["1a"]["match"] == "exact_ci"         # small marks -> objective
    assert g["1a"]["answer"] == ""                # blank for the author to fill
    assert g["1a"]["max_marks"] == 1
    assert g["1a"]["_student_answer"] == "False"  # authoring hint from the first occurrence
    assert g["D1"]["match"] == "rubric"           # >=5 marks -> rubric
    assert g["D1"]["rubric"] == ""


def test_scaffolded_template_is_loadable_by_grader(tmp_path):
    import json
    g = scaffold_from_transcript(_paper())
    p = tmp_path / "Math.guide.template.json"
    p.write_text(json.dumps(g))
    scheme = grader.GuideMarkScheme.from_file(str(p), fallback=grader.LLMJudge(None))
    # the _student_answer hint is ignored; an unfilled exact_ci answer ("") scores 0
    out = scheme.grade_question(_q("1a", 1, "False"))
    assert out.awarded_marks == 0          # template answer is blank until filled in
    assert scheme.total_marks == 16.0      # 1 + 15
