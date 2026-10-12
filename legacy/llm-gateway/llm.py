from groq import Groq

from config import settings

client = Groq(api_key=settings.groq_api_key.get_secret_value())


class InferenceError(RuntimeError):
    """Provider call failed — the gateway must not settle. See REPORT.md §10.3."""


def call_llm(prompt: str) -> str:
    try:
        r = client.chat.completions.create(
            model=settings.groq_model,
            messages=[{"role": "user", "content": prompt}],
            temperature=0,
            # Price is fixed, so provider cost must be bounded.
            max_completion_tokens=settings.max_completion_tokens,
        )
    except Exception as exc:
        raise InferenceError(f"{type(exc).__name__}: {exc}") from exc

    content = r.choices[0].message.content if r.choices else None
    if not content or not content.strip():
        raise InferenceError("model returned an empty completion")
    return content.strip()
