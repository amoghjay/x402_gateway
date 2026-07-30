import os

from dotenv import load_dotenv
from groq import Groq

load_dotenv()

client = Groq(api_key=os.environ["GROQ_API_KEY"])

MODEL = "llama-3.3-70b-versatile"


class InferenceError(RuntimeError):
    """The upstream model call failed, so the caller must not be charged.

    This used to be a caught-and-swallowed exception that returned a canned
    string, which meant a provider outage still produced HTTP 200 and still took
    the payer's money. Failing loudly is what lets the gateway settle only after
    a real completion exists. See REPORT.md §10.3.
    """


def call_llm(prompt: str) -> str:
    try:
        r = client.chat.completions.create(
            model=MODEL,
            messages=[{"role": "user", "content": prompt}],
            temperature=0,
        )
    except Exception as exc:
        raise InferenceError(f"{type(exc).__name__}: {exc}") from exc

    content = r.choices[0].message.content if r.choices else None
    if not content or not content.strip():
        raise InferenceError("model returned an empty completion")
    return content.strip()
