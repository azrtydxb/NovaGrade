import argparse
import os
import sys

from examgrader.config import SETTINGS
from examgrader.dots_transcriber import ocr_page, transcribe_paper_hybrid
from examgrader.grader import GuideMarkScheme, LLMJudge, grade_paper, guide_coverage
from examgrader.llm_client import LLMClient
from examgrader.markmap import extract_mark_map_from_text, reconcile, section_reconcile
from examgrader.pdf_to_images import render_pdf
from examgrader.report import write_report
from examgrader.schemas import GradedPaper, TranscribedPaper


def _grader_client():
    return LLMClient(
        SETTINGS.grader_base_url, SETTINGS.grader_model, SETTINGS.request_timeout,
        SETTINGS.max_retries, SETTINGS.llm_seed,
    )


def _ocr_client():
    return LLMClient(
        SETTINGS.ocr_base_url, SETTINGS.ocr_model, SETTINGS.request_timeout,
        SETTINGS.max_retries, SETTINGS.llm_seed,
    )


def _vlm_client():
    return LLMClient(
        SETTINGS.vlm_base_url, SETTINGS.vlm_model, SETTINGS.request_timeout,
        SETTINGS.max_retries, SETTINGS.llm_seed,
    )


def grade_pdf(pdf_path=None, subject=None, *, out_dir=None, guide_path=None,
              transcript_path=None, ocr_client=None, vlm_client=None,
              grader_client=None) -> GradedPaper:
    out_dir = out_dir or SETTINGS.out_dir
    os.makedirs(out_dir, exist_ok=True)
    grader_client = grader_client or _grader_client()

    if transcript_path:
        # re-grade an existing transcript: no render, no OCR (reproducible + fast)
        transcript = TranscribedPaper.model_validate_json(open(transcript_path).read())
        subject = subject or transcript.subject
    else:
        # Hybrid transcription: dots.ocr reads printed questions + marks; qwen3-vl reads the
        # student's answers (incl. circled options); the grader model merges the two.
        ocr_client = ocr_client or _ocr_client()
        vlm_client = vlm_client or _vlm_client()
        stem = os.path.splitext(os.path.basename(pdf_path))[0]
        subject = subject or stem
        # render ALL pages — do NOT pre-filter "blank" pages: sparse exam pages (big
        # rough-work whitespace) were being wrongly dropped, losing ~half the questions.
        # Genuinely empty pages just yield no questions from the transcriber.
        pages = render_pdf(pdf_path, os.path.join(out_dir, f"{stem}_pages"))
        # read the stated mark distribution from the OCR'd first page
        mark_map = extract_mark_map_from_text(
            grader_client, ocr_page(ocr_client, pages[0])) if pages else {}
        transcript = transcribe_paper_hybrid(
            ocr_client, vlm_client, grader_client, pages, subject,
            os.path.basename(pdf_path), mark_map,
        )
        transcript.expected_total = mark_map.get("total")
        rec = reconcile(mark_map, transcript)
        if rec["expected_total"] is not None and not rec["ok"]:
            print(f"[reconcile] stated total {rec['expected_total']:g} but detected "
                  f"{rec['detected_total']:g} (Δ {rec['difference']:+g}) — marks may be "
                  "mis-read on some questions", file=sys.stderr)
        for row in section_reconcile(mark_map, transcript):
            if not row["ok"]:
                print(f"[reconcile]   Section {row['section']}: stated {row['expected']:g}, "
                      f"detected {row['detected']:g} (Δ {row['difference']:+g})", file=sys.stderr)
        with open(os.path.join(out_dir, f"{stem}.transcript.json"), "w") as f:
            f.write(transcript.model_dump_json(indent=2))

    # Fail loudly instead of writing a meaningless 0/0 report: an empty transcript means
    # the vision model returned nothing (e.g. unreachable endpoint or a non-exam PDF).
    if not transcript.questions:
        raise RuntimeError(
            f"no questions transcribed from {pdf_path or transcript_path!r} — "
            "is the vision model reachable and the PDF a real exam?"
        )

    # With a marking guide: deterministic objective grading; guide-matched questions carry
    # the guide's authoritative max_marks, so max_total (derived in grade_paper) reflects the
    # true paper total for a complete guide and stays correct for a partial one.
    # Without: the LLM-judge decides answers (non-deterministic, marks read by the VLM).
    if guide_path:
        scheme = GuideMarkScheme.from_file(
            guide_path, fallback=LLMJudge(grader_client), client=grader_client
        )
        uncovered, unused = guide_coverage(scheme.guide, transcript)
        if uncovered:
            print(f"[guide] {len(uncovered)} question(s) not in the guide → LLM-judge "
                  f"fallback: {', '.join(uncovered[:10])}{'…' if len(uncovered) > 10 else ''}",
                  file=sys.stderr)
        if unused:
            print(f"[guide] {len(unused)} guide entr(ies) not seen in the paper: "
                  f"{', '.join(unused[:10])}{'…' if len(unused) > 10 else ''}", file=sys.stderr)
    else:
        scheme = LLMJudge(grader_client)
    # denominator = detected marks (the scale the awards are on); reconciliation makes that
    # converge to the stated total rather than overriding it (which would let scores pass 100)
    paper = grade_paper(scheme, transcript)
    paper.expected_total = transcript.expected_total  # carry the stated total for the report

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
