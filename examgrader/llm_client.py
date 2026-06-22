import base64
import json
import time

import httpx


def extract_json(text: str):
    """Return the first JSON object/array found in a model reply."""
    text = text.strip()
    if text.startswith("```"):
        # drop opening fence (``` or ```json) and closing fence
        text = text[3:]
        if text[:4].lower() == "json":
            text = text[4:]
        if "```" in text:
            text = text[: text.index("```")]
        text = text.strip()
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        pass
    # fall back: scan for the first balanced { } or [ ]
    starts = [i for i in (text.find("{"), text.find("[")) if i != -1]
    if not starts:
        raise ValueError(f"No JSON found in reply: {text[:200]!r}")
    start = min(starts)
    open_ch = text[start]
    close_ch = "}" if open_ch == "{" else "]"
    depth = 0
    for i in range(start, len(text)):
        if text[i] == open_ch:
            depth += 1
        elif text[i] == close_ch:
            depth -= 1
            if depth == 0:
                return json.loads(text[start : i + 1])
    raise ValueError(f"Unbalanced JSON in reply: {text[:200]!r}")


def text_part(text: str) -> dict:
    return {"type": "text", "text": text}


def image_part(png_path: str) -> dict:
    with open(png_path, "rb") as f:
        data = base64.b64encode(f.read()).decode()
    return {"type": "image_url", "image_url": {"url": f"data:image/png;base64,{data}"}}


class LLMClient:
    def __init__(self, base_url: str, model: str, timeout: float = 180.0,
                 max_retries: int = 3, seed: int | None = None):
        self.base_url = base_url.rstrip("/")
        self.model = model
        self.timeout = timeout
        self.max_retries = max_retries
        self.seed = seed

    def chat_json(self, content, *, max_tokens: int = 1500, temperature: float = 0.0):
        payload = {
            "model": self.model,
            "messages": [{"role": "user", "content": content}],
            "max_tokens": max_tokens,
            "temperature": temperature,
        }
        if self.seed is not None:
            payload["seed"] = self.seed
        last_err = None
        for attempt in range(self.max_retries):
            try:
                r = httpx.post(
                    f"{self.base_url}/chat/completions", json=payload, timeout=self.timeout
                )
                r.raise_for_status()
                reply = r.json()["choices"][0]["message"]["content"]
                return extract_json(reply)
            except Exception as e:  # noqa: BLE001 - retry on any failure
                last_err = e
                time.sleep(0.5 * (attempt + 1))
        raise RuntimeError(
            f"LLM call to {self.base_url} failed after {self.max_retries} attempts: {last_err}"
        )
