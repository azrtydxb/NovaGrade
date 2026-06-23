from dataclasses import dataclass


@dataclass(frozen=True)
class Settings:
    vlm_base_url: str = "http://192.168.10.246:8003/v1"
    vlm_model: str = "qwen3-vl"
    ocr_base_url: str = "http://192.168.10.246:8004/v1"  # dots.ocr document transcriber
    ocr_model: str = "dots-ocr"
    grader_base_url: str = "http://192.168.10.246:8888/v1"
    grader_model: str = "qwen3.6-35b"
    render_dpi: int = 200
    request_timeout: float = 180.0
    max_retries: int = 3
    out_dir: str = "out"
    # kept modest: the hybrid runs 3 models on one box (~3 calls/page); high concurrency
    # can cause request timeouts on the memory-tight box
    vlm_concurrency: int = 2
    grader_concurrency: int = 8
    llm_seed: int = 0  # sent to vLLM for more reproducible LLM outputs
    # re-OCR attempts to match the paper's stated total. Default 1 = diagnostic only:
    # re-transcription doesn't fix the VLM's *systematic* mark mis-reads (measured), so the
    # extra passes mostly cost time. Raise to 2+ to opt into targeted re-transcription.
    max_transcribe_passes: int = 1


SETTINGS = Settings()
