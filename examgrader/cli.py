import argparse
import os
import sys

from examgrader.config import SETTINGS
from examgrader.grader import LLMJudge, grade_paper
from examgrader.llm_client import LLMClient
from examgrader.pdf_to_images import content_pages
from examgrader.report import write_report
from examgrader.schemas import GradedPaper
from examgrader.transcriber import transcribe_paper


def grade_pdf(pdf_path, subject, *, out_dir=None, vlm_client=None, grader_client=None) -> GradedPaper:
    out_dir = out_dir or SETTINGS.out_dir
    os.makedirs(out_dir, exist_ok=True)
    vlm_client = vlm_client or LLMClient(
        SETTINGS.vlm_base_url, SETTINGS.vlm_model, SETTINGS.request_timeout, SETTINGS.max_retries
    )
    grader_client = grader_client or LLMClient(
        SETTINGS.grader_base_url, SETTINGS.grader_model, SETTINGS.request_timeout, SETTINGS.max_retries
    )
    stem = os.path.splitext(os.path.basename(pdf_path))[0]

    pages = content_pages(pdf_path, os.path.join(out_dir, f"{stem}_pages"))
    transcript = transcribe_paper(vlm_client, pages, subject, os.path.basename(pdf_path))
    with open(os.path.join(out_dir, f"{stem}.transcript.json"), "w") as f:
        f.write(transcript.model_dump_json(indent=2))

    paper = grade_paper(LLMJudge(grader_client), transcript)
    write_report(paper, out_dir)
    return paper


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Grade a scanned exam PDF on the DGX.")
    ap.add_argument("pdf")
    ap.add_argument("--subject", default=None)
    ap.add_argument("--out", default=SETTINGS.out_dir)
    args = ap.parse_args(argv)
    subject = args.subject or os.path.splitext(os.path.basename(args.pdf))[0]
    paper = grade_pdf(args.pdf, subject, out_dir=args.out)
    print(f"{subject}: {paper.total:g}/{paper.max_total:g}")
    print(f"Reports written under {args.out}/", file=sys.stderr)
    return 0
