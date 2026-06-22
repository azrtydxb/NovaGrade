import os

from examgrader.schemas import GradedPaper

# flags that warrant a human's attention (⚠); blank answers are expected, not review-worthy
REVIEW_FLAGS = {"low_read_confidence", "grading_failed"}


def to_json(paper: GradedPaper) -> str:
    return paper.model_dump_json(indent=2)


def to_markdown(paper: GradedPaper) -> str:
    pct = round(100 * paper.total / paper.max_total, 1) if paper.max_total else 0.0
    lines = [
        f"# {paper.subject} — graded ({paper.source_pdf})",
        "",
        f"**Grade: {pct:g} / 100**  ·  raw {paper.total:g}/{paper.max_total:g}",
        "",
        "| Q | Section | Marks | Conf | Flags | Justification |",
        "|---|---------|-------|------|-------|---------------|",
    ]
    for q in paper.questions:
        warn = "⚠ " if REVIEW_FLAGS.intersection(q.flags) else ""
        flags = ", ".join(q.flags) if q.flags else ""
        just = q.justification.replace("|", "\\|")
        lines.append(
            f"| {warn}{q.question_no} | {q.section or ''} | "
            f"{q.awarded_marks:g}/{q.max_marks:g} | {q.grade_confidence:g} | "
            f"{flags} | {just} |"
        )
    lines += ["", "## Section totals", ""]
    for sec, tot in sorted(paper.section_totals.items()):
        lines.append(f"- {sec}: {tot:g}")
    return "\n".join(lines) + "\n"


def write_report(paper: GradedPaper, out_dir: str) -> tuple[str, str]:
    os.makedirs(out_dir, exist_ok=True)
    stem = os.path.splitext(os.path.basename(paper.source_pdf))[0]
    json_path = os.path.join(out_dir, f"{stem}.results.json")
    md_path = os.path.join(out_dir, f"{stem}.report.md")
    with open(json_path, "w") as f:
        f.write(to_json(paper))
    with open(md_path, "w") as f:
        f.write(to_markdown(paper))
    return json_path, md_path
