"""Scaffold a marking-guide template from a transcript.

Produces a guide keyed by question_no with the transcribed max_marks pre-filled, a default
match type, and the student's transcribed answer kept under `_student_answer` as an authoring
hint (the grader ignores keys it doesn't use). A human fills in `answer` / `accept` / `rubric`.
"""
import argparse
import json
import os

from examgrader.schemas import TranscribedPaper

# questions worth at least this many marks default to the open-ended `rubric` template
RUBRIC_MARK_THRESHOLD = 5.0


def scaffold_from_transcript(transcript: TranscribedPaper) -> dict:
    guide: dict[str, dict] = {}
    for q in transcript.questions:
        if q.question_no in guide:
            continue  # first occurrence wins; duplicate question_no would collide in JSON
        if q.max_marks >= RUBRIC_MARK_THRESHOLD:
            entry = {"max_marks": q.max_marks, "match": "rubric", "rubric": ""}
        else:
            entry = {"max_marks": q.max_marks, "match": "exact_ci", "answer": ""}
        entry["_student_answer"] = q.student_answer  # authoring hint, ignored by the grader
        guide[q.question_no] = entry
    return guide


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Scaffold a marking-guide template from a transcript.")
    ap.add_argument("transcript", help="path to a *.transcript.json")
    ap.add_argument("--out", default=None, help="output path (default: in/<subject>.guide.template.json)")
    args = ap.parse_args(argv)

    transcript = TranscribedPaper.model_validate_json(open(args.transcript).read())
    guide = scaffold_from_transcript(transcript)
    out = args.out or os.path.join("in", f"{transcript.subject}.guide.template.json")
    os.makedirs(os.path.dirname(out) or ".", exist_ok=True)
    with open(out, "w") as f:
        json.dump(guide, f, indent=2, ensure_ascii=False)
        f.write("\n")
    print(f"{transcript.subject}: scaffolded {len(guide)} questions -> {out}")
    return 0
