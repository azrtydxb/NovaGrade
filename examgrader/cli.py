import argparse
import os
import sys

from examgrader.config import SETTINGS
from examgrader.grader import GuideMarkScheme, LLMJudge, grade_paper
from examgrader.llm_client import LLMClient
from examgrader.pdf_to_images import content_pages
from examgrader.report import write_report
from examgrader.schemas import GradedPaper, TranscribedPaper
from examgrader.transcriber import transcribe_paper


def _grader_client():
    return LLMClient(
        SETTINGS.grader_base_url, SETTINGS.grader_model, SETTINGS.request_timeout,
        SETTINGS.max_retries, SETTINGS.llm_seed,
    )


def grade_pdf(pdf_path=None, subject=None, *, out_dir=None, guide_path=None,
              transcript_path=None, vlm_client=None, grader_client=None) -> GradedPaper:
    out_dir = out_dir or SETTINGS.out_dir
    os.makedirs(out_dir, exist_ok=True)
    grader_client = grader_client or _grader_client()

    if transcript_path:
        # re-grade an existing transcript: no render, no OCR (reproducible + fast)
        transcript = TranscribedPaper.model_validate_json(open(transcript_path).read())
        subject = subject or transcript.subject
    else:
        vlm_client = vlm_client or LLMClient(
            SETTINGS.vlm_base_url, SETTINGS.vlm_model, SETTINGS.request_timeout,
            SETTINGS.max_retries, SETTINGS.llm_seed,
        )
        stem = os.path.splitext(os.path.basename(pdf_path))[0]
        subject = subject or stem
        pages = content_pages(pdf_path, os.path.join(out_dir, f"{stem}_pages"))
        transcript = transcribe_paper(vlm_client, pages, subject, os.path.basename(pdf_path))
        with open(os.path.join(out_dir, f"{stem}.transcript.json"), "w") as f:
            f.write(transcript.model_dump_json(indent=2))

    # With a marking guide: deterministic objective grading; guide-matched questions carry
    # the guide's authoritative max_marks, so max_total (derived in grade_paper) reflects the
    # true paper total for a complete guide and stays correct for a partial one.
    # Without: the LLM-judge decides answers (non-deterministic, marks read by the VLM).
    if guide_path:
        scheme = GuideMarkScheme.from_file(
            guide_path, fallback=LLMJudge(grader_client), client=grader_client
        )
    else:
        scheme = LLMJudge(grader_client)
    paper = grade_paper(scheme, transcript)

    write_report(paper, out_dir)
    return paper


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Grade a scanned exam PDF on the DGX.")
    ap.add_argument("pdf", nargs="?", help="exam PDF to grade (omit if --from-transcript)")
    ap.add_argument("--subject", default=None)
    ap.add_argument("--out", default=SETTINGS.out_dir)
    ap.add_argument("--guide", default=None,
                    help="path to a marking-guide JSON; enables deterministic grading")
    ap.add_argument("--from-transcript", default=None, dest="from_transcript",
                    help="re-grade a saved *.transcript.json without re-running OCR")
    args = ap.parse_args(argv)
    if not args.pdf and not args.from_transcript:
        ap.error("provide a PDF or --from-transcript")

    paper = grade_pdf(args.pdf, args.subject, out_dir=args.out, guide_path=args.guide,
                      transcript_path=args.from_transcript)
    print(f"{paper.subject}: {paper.score_100:g}/100  (raw {paper.total:g}/{paper.max_total:g})")
    print(f"Reports written under {args.out}/", file=sys.stderr)
    return 0
