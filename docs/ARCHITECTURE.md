# Architecture — wael-exames

A local pipeline that grades scanned NESA exam PDFs entirely on the DGX Spark. It has two
model-backed stages separated by a typed boundary: **read** (a vision model transcribes
printed questions + handwritten answers) then **judge** (a reasoning model grades each
question). The judge sits behind a `MarkScheme` interface so the POC's LLM-as-judge can be
swapped for an official marking guide without touching the reading stage.

## 1. System context

```mermaid
flowchart LR
    user(["Teacher / operator"]) -->|"grade.py paper.pdf"| cli["CLI<br/>grade_pdf()"]

    subgraph local["Local machine — Python 3.12 (uv)"]
        cli --> render["pdf_to_images<br/>pdftoppm + magick"]
        cli --> tr["transcriber"]
        cli --> gr["grader (LLMJudge)"]
        cli --> rep["report"]
    end

    subgraph dgx["DGX Spark — 192.168.10.246"]
        vlm[["qwen3-vl<br/>port 8003"]]
        reason[["qwen3.6-35b<br/>port 8888"]]
    end

    tr -->|"page image"| vlm
    gr -->|"one question"| reason
    rep --> out[("out/*.results.json<br/>out/*.report.md<br/>out/*.transcript.json")]
```

The two models are pre-existing OpenAI-compatible vLLM endpoints on the DGX. Nothing leaves
the local network.

## 2. Module structure

```mermaid
flowchart TD
    cli["cli.py<br/>grade_pdf / main"]
    pdf["pdf_to_images.py"]
    tr["transcriber.py"]
    gr["grader.py"]
    rep["report.py"]
    llm["llm_client.py"]
    par["parallel.py"]
    sch["schemas.py"]
    cfg["config.py"]

    cli --> pdf
    cli --> tr
    cli --> gr
    cli --> rep
    tr --> llm
    tr --> par
    tr --> sch
    tr --> cfg
    gr --> llm
    gr --> par
    gr --> sch
    gr --> cfg
    rep --> sch
    pdf --> cfg
    cli --> cfg

    classDef io fill:#e8f0fe,stroke:#4a86e8
    class llm,pdf io
```

Each module has one responsibility:

| Module | Responsibility |
|---|---|
| `config.py` | Endpoints, model names, render DPI, concurrency limits (frozen `Settings`) |
| `schemas.py` | Pydantic models that cross every stage boundary |
| `pdf_to_images.py` | Render pages with `pdftoppm`; drop near-blank scans (`magick` mean) |
| `llm_client.py` | OpenAI-compatible HTTP client: retry + JSON extraction + image/text parts |
| `parallel.py` | `map_ordered` — order-preserving thread-pool fan-out |
| `transcriber.py` | Page image → `TranscribedPaper` (vision model) |
| `grader.py` | `MarkScheme` interface + `LLMJudge`; `TranscribedPaper` → `GradedPaper` |
| `report.py` | `GradedPaper` → JSON + Markdown |
| `cli.py` | Orchestrates the pipeline; persists the transcript |

## 3. Pipeline sequence

```mermaid
sequenceDiagram
    actor U as Operator
    participant CLI as cli.grade_pdf
    participant PDF as pdf_to_images
    participant TR as transcriber
    participant VLM as qwen3-vl :8003
    participant GR as grader
    participant LLM as qwen3.6-35b :8888
    participant REP as report

    U->>CLI: grade_pdf(pdf, subject)
    CLI->>PDF: content_pages(pdf)
    PDF-->>CLI: page PNGs (blanks dropped)
    CLI->>TR: transcribe_paper(pages)
    par concurrent pages (vlm_concurrency)
        TR->>VLM: page image + TRANSCRIBE_PROMPT
        VLM-->>TR: questions JSON
    end
    TR-->>CLI: TranscribedPaper
    CLI->>CLI: persist transcript.json
    CLI->>GR: grade_paper(transcript)
    par concurrent questions (grader_concurrency)
        GR->>LLM: question + max marks
        LLM-->>GR: awarded + justification
    end
    GR-->>CLI: GradedPaper
    CLI->>REP: write_report(paper)
    REP-->>U: results.json + report.md
```

The transcript is persisted **before** grading, so grading can be re-run without paying for
OCR again.

## 4. Data model (stage interfaces)

```mermaid
classDiagram
    class TranscribedQuestion {
        +Optional~str~ section
        +str question_no
        +float max_marks
        +str question_text
        +str student_answer
        +float read_confidence
    }
    class TranscribedPaper {
        +str subject
        +str source_pdf
        +list~TranscribedQuestion~ questions
    }
    class GradedQuestion {
        +str question_no
        +Optional~str~ section
        +float max_marks
        +float awarded_marks
        +str student_answer
        +str justification
        +float grade_confidence
        +list~str~ flags
    }
    class GradedPaper {
        +str subject
        +str source_pdf
        +list~GradedQuestion~ questions
        +dict section_totals
        +float total
        +float max_total
    }
    TranscribedPaper "1" *-- "many" TranscribedQuestion
    GradedPaper "1" *-- "many" GradedQuestion
    TranscribedQuestion ..> GradedQuestion : graded into
```

## 5. Concurrency model

Both stages fan their model calls out through `parallel.map_ordered`, which runs a
`ThreadPoolExecutor` and returns results in input order. Per-item isolation is preserved:
a failed page or question is skipped, never the whole paper.

```mermaid
flowchart TD
    subgraph trans["transcribe_paper (vlm_concurrency = 4)"]
        p1["page 1"] --> map1{{"map_ordered<br/>thread pool"}}
        p2["page 2"] --> map1
        p3["page N"] --> map1
        map1 --> q1["questions[]"]
    end
    subgraph grade["grade_paper (grader_concurrency = 8)"]
        g1["question 1"] --> map2{{"map_ordered<br/>thread pool"}}
        g2["question 2"] --> map2
        g3["question M"] --> map2
        map2 --> r1["GradedQuestion[]"]
    end
    q1 --> grade
```

Measured speedups: grader calls ~3.4×, vision calls ~2.0× (the vision model is GPU-bound on
the single GB10, so transcription dominates wall-clock).

## 6. Grading & flag semantics

`LLMJudge.grade_question` clamps awarded marks to `[0, max_marks]` and attaches a flag:

```mermaid
flowchart TD
    q["TranscribedQuestion"] --> blank{"answer blank?"}
    blank -->|"yes"| bflag["flag: blank_answer<br/>(legitimate 0, no review)"]
    blank -->|"no"| conf{"read_confidence < 0.5?"}
    conf -->|"yes"| lflag["flag: low_read_confidence<br/>(needs human, gets warning)"]
    conf -->|"no"| ok["no flag"]
    call["grader LLM call"] -->|"error"| gflag["flag: grading_failed<br/>(scored 0, gets warning)"]
```

The report's warning marker fires only on review-worthy flags (`low_read_confidence`,
`grading_failed`) — never on a blank answer.

## 7. Deployment (DGX Spark)

Four vLLM containers share the GB10's 121 GB unified memory. The grader was tuned down to
util 0.6 and the vision model added at util 0.20 so all four coexist.

```mermaid
flowchart LR
    subgraph gb10["DGX Spark — GB10, 121 GB unified"]
        n1["vllm-node<br/>qwen3.6-35b<br/>:8888 · util 0.6"]
        n2["vllm-qwen3-vl<br/>Qwen3-VL-30B-A3B-AWQ<br/>:8003 · util 0.20"]
        n3["vllm-embed-bge-m3<br/>:8890 (NovaMem)"]
        n4["vllm-rerank-v2m3<br/>:8889 (NovaMem)"]
    end
    grader["grader stage"] --> n1
    transcriber["transcriber stage"] --> n2
```

Full serving details (launch scripts, memory tuning, rollback container) are in
[`docs/superpowers/specs/2026-06-22-exam-grading-framework-design.md`](superpowers/specs/2026-06-22-exam-grading-framework-design.md).
