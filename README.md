# wael-exames — local exam grading

Grades scanned NESA exam PDFs on the DGX Spark using a two-stage local-LLM pipeline:
`pdftoppm` render → `qwen3-vl` (handwriting transcription, port 8003) →
`qwen3.6-35b` (grading, port 8888) → JSON + Markdown report.

```mermaid
flowchart LR
    pdf[("scanned<br/>exam PDF")] --> render["pdf_to_images<br/>pdftoppm"]
    render -->|"page images"| tr["transcriber<br/>qwen3-vl :8003"]
    tr -->|"TranscribedPaper"| gr["grader<br/>qwen3.6-35b :8888"]
    gr -->|"GradedPaper"| rep["report"]
    rep --> out[("results.json<br/>report.md<br/>transcript.json")]
```

Inputs live in `in/` (`English paper.pdf`, `Math paper.pdf`, `SET paper.pdf`); grades are
written to `out/`. Full design with more diagrams: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

---

## Testing it against the 3 PDFs

### 0. Prerequisites (one-time)

- **Local tools:** `uv` (Python 3.12 manager), plus `pdftoppm` (poppler) and `magick`
  (ImageMagick). On macOS: `brew install uv poppler imagemagick`.
- **Install deps:** from the repo root, `uv sync` (creates the 3.12 venv and installs
  pydantic/httpx/pytest from `uv.lock`).
- **DGX models must be up.** Both endpoints in `examgrader/config.py` must answer `200`:

      curl -s -o /dev/null -w "vlm:%{http_code}\n"    http://192.168.10.246:8003/v1/models
      curl -s -o /dev/null -w "grader:%{http_code}\n" http://192.168.10.246:8888/v1/models

  If either is not `200`, the vision/grader container is stopped — restart on the DGX
  (`ssh dgx-spark`, then `~/launch-qwen3-vl.sh` for the vision model; see the design spec
  for the grader). Each paper takes ~2 minutes to grade.

### 1. Grade all three at once

    ./grade_all.sh

This runs each paper and writes results under `out/`. (It just loops the single-paper
command below.)

### 2. Or grade one paper at a time

    uv run python grade.py "in/Math paper.pdf"    --subject Math
    uv run python grade.py "in/English paper.pdf" --subject English
    uv run python grade.py "in/SET paper.pdf"     --subject SET

`--subject` is optional (defaults to the file name); `--out DIR` changes the output folder
(default `out`).

### 3. Read the results

For each paper, three files appear in `out/`:

| File | What it is |
|------|-----------|
| `<paper>.report.md` | Human-readable: per-question marks, justification, ⚠ flags, total |
| `<paper>.results.json` | Same data as structured JSON (for downstream tooling) |
| `<paper>.transcript.json` | What the vision model *read* off each page (check OCR accuracy here) |

Quick look:

    cat "out/Math paper.report.md"

To sanity-check the handwriting reading, open the `*.transcript.json` and compare a few
answers against the scan — that isolates "did it read the page right" from "did it grade
right."

### 4. Run the unit tests (no DGX needed)

    uv run pytest

All LLM calls are mocked, so this runs offline and fast. Use it to confirm the code is
healthy before a live run.

---

## How it works

1. `examgrader/pdf_to_images.py` — `pdftoppm` renders pages; near-blank scans are dropped.
2. `examgrader/transcriber.py` — sends each page to the vision model, which returns
   structured `{question, max_marks, student_answer, read_confidence}` records. A single
   unreadable page or question is skipped, never the whole paper. `examgrader/markmap.py`
   reads the paper's stated total from the instructions page and the transcriber re-runs (up
   to `max_transcribe_passes`) to best match it — a mismatch is flagged, not hidden.
3. `examgrader/grader.py` — the `MarkScheme` interface grades each question: `LLMJudge` by
   default, or `GuideMarkScheme` with `--guide` (deterministic, marking-guide-driven). The
   reading stage is untouched either way.
4. `examgrader/report.py` — writes the per-question JSON + a readable Markdown report.

## Results (all three graded, regenerated 2026-06-22)

Every paper is scored on a normalized **0–100 scale** (`score_100 = 100 × awarded ÷
max_marks`), so grades are comparable across papers and can never exceed 100. The committed
grades under `out/` are from this run:

Every paper is scored on a normalized **0–100 scale** and is **reconciled** against the
paper's own stated total (read from the instructions page): if the detected marks don't match,
the report flags it. The committed grades under `out/` are from this run:

| Paper | Grade /100 | Raw | Stated total | Marks checksum |
|-------|-----------|-----|--------------|----------------|
| SET     | 70.0 | 70/100  | 100 | ✓ reconciles (100 = 100) |
| Math    | 76.8 | 43/56   | 100 | ⚠ under-read (56) |
| English | 66.8 | 125/187 | 100 | ⚠ over-read (187) |

The checksum is the key trust signal. **SET reconciles exactly**, so its denominator is the
real 100. Math and English don't — the vision model mis-reads their per-question marks (Math
under, English over). For papers with stated **section budgets** (English A/B/C/D), the report
and CLI also show a **per-section** breakdown, pinpointing where it's off:

```
Section A: stated 20, detected 36 (+16)
Section C: stated 40, detected 71 (+31)
```

Targeted re-transcription of off-budget sections is available (`max_transcribe_passes > 1`)
but **off by default**: measured, it doesn't fix the VLM's *systematic* mark mis-reads
(re-reading reproduces them), so it mostly costs time. The diagnostic is the value. The
normalized `/100` keeps scores in range, but a flagged denominator is unreliable — the real
cure is a **marking guide** (`--guide`), which supplies the canonical marks. Treat
un-reconciled scores as a demonstration; LLM-judge grades also vary run-to-run.

## Performance

Transcription and grading both run their model calls concurrently (thread pool;
`vlm_concurrency` and `grader_concurrency` in `config.py`). Grader calls parallelize ~3.4×;
the vision model is GPU-bound on the single GB10 and scales ~2×, so transcription is the
dominant cost (~6 s/page). A full paper runs in roughly 75 s–2 min depending on page count.
Lowering `render_dpi` (200→150) is the cheapest further speedup if OCR accuracy holds.

## Flags in the report

- `blank_answer` — the student left it blank (a legitimate 0; informational only).
- `low_read_confidence` — handwriting was present but hard to read; **gets a ⚠** — check the
  scan against the transcript.
- `grading_failed` — the grader call errored for that question (scored 0); **gets a ⚠**.

The ⚠ marker fires only on review-worthy flags, not on blank answers.

## Marking guide (deterministic, accurate grading)

Grading runs behind a small `MarkScheme` interface. By default it uses **`LLMJudge`** (the
reasoning model decides the answer itself — flexible but non-deterministic). Pass `--guide`
to grade against an official **marking guide** instead:

    uv run python grade.py "in/Math paper.pdf" --guide "in/Math.guide.json"

The guide is a per-subject JSON file keyed by `question_no` with the authoritative answer
*and* marks per question (see the working example in [`in/Math.guide.json`](in/Math.guide.json)):

```json
{
  "1a": { "max_marks": 1, "answer": "False", "match": "exact_ci" },
  "3a": { "max_marks": 2, "accept": ["Circumference", "perimeter"], "match": "set" },
  "D1": { "max_marks": 15, "rubric": "content 6, grammar 5, structure 4", "match": "rubric" }
}
```

- `exact` / `exact_ci` / `set` → deterministic string compare (no LLM, reproducible) for
  objective questions.
- `rubric` → the LLM awards marks **bounded by the rubric**, for open-ended answers.
- Questions not in the guide fall back to `LLMJudge`.

Because the guide carries the canonical `max_marks`, it also pulls the denominator back to the
paper's true total. Full detail + diagrams:
[`docs/ARCHITECTURE.md` §7](docs/ARCHITECTURE.md#7-grading-strategy-the-markscheme-interface).

### Scaffold a full guide

Authoring a complete guide is just filling in answers. Generate a template from a transcript —
every question pre-listed with its marks, a default `match`, a blank `answer`/`rubric`, and the
student's transcribed answer as a `_student_answer` hint:

    uv run python scaffold_guide.py "out/Math paper.transcript.json"
    # -> in/Math paper.guide.template.json

Pre-scaffolded templates for the sample papers are in `in/*.guide.template.json`. Fill in the
authoritative answers (and set `match`/`accept`/`rubric` per question), rename to
`in/<subject>.guide.json`, and grade with `--guide`. Questions ≥ 5 marks default to the
`rubric` match type; the `_student_answer` field is an authoring hint and is ignored by the grader.

### Reproducible grades

LLM-judged grading varies slightly between runs (vLLM at `temperature=0` is not
bitwise-deterministic). Two levers make grading reproducible:

- **A marking guide** — objective (`exact`/`exact_ci`/`set`) questions are graded by string
  compare and are identical every run; a *complete* guide is fully deterministic.
- **`--from-transcript`** — re-grade a saved `*.transcript.json` without re-running OCR, so
  grading sees the exact same input each time (and it's much faster):

      uv run python grade.py --from-transcript "out/Math paper.transcript.json" --guide "in/Math.guide.json"

## Known limitations (POC)

- **Mark attribution is noisy.** The vision model reads each question's "(N marks)" label
  imperfectly, so the raw denominator drifts from the paper's true 100 (e.g. English 141).
  This is now contained — `max_total` is derived from the paper and the headline grade is
  normalized to `/100`, so totals can no longer exceed 100 — but the denominator is only as
  accurate as the OCR. A marking guide (`--guide`) supplies the canonical marks and fixes it.
- **LLM-judge grading is not bitwise-deterministic** (vLLM batching at `temperature=0`). Use a
  marking guide for deterministic objective grading, and `--from-transcript` to fix the OCR
  input — see [Reproducible grades](#reproducible-grades). The `rubric` and fallback LLM paths
  remain best-effort.
- The vision model scales only ~2× concurrently (memory-bound at its current util); more
  speed needs lower DPI or more VRAM headroom for batching.
- LLM-judge grading is best-effort; production should use the official marking guide via a
  `MarkScheme` implementation.

## Notes

- Inputs are in `in/`, grades in `out/` — both committed to this repo, including the raw
  rendered page scans (`out/*_pages/`). ⚠️ These scans and the source PDFs contain pupils'
  names; this repo is public, so that personal data is committed publicly by request.
- Endpoints, model names, and render DPI live in `examgrader/config.py`.
- DGX serving details (the `qwen3-vl` container, memory tuning) are in
  `docs/superpowers/specs/2026-06-22-exam-grading-framework-design.md`.
