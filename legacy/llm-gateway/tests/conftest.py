import os
from pathlib import Path

# Reuse the container's throwaway config so tests and the CI smoke test cannot
# drift apart. Real env vars beat .env in pydantic-settings, so loading this before
# config is imported keeps the suite off the live chain and needs no secrets.
for raw in (Path(__file__).parent.parent / ".env.ci").read_text().splitlines():
    line = raw.strip()
    if line and not line.startswith("#"):
        key, value = line.split("=", 1)
        os.environ[key] = value
