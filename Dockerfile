FROM python:3.12-slim

# No Foundry in the image: payment.py reads the committed
# contracts/abi/InferenceEscrow.json. See contracts/sync-abi.sh.
WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY config.py observability.py payment.py llm.py gateway.py ./
COPY contracts/abi ./contracts/abi

# Config comes from the environment; .dockerignore keeps every .env out of the image.
RUN adduser --system --no-create-home --uid 10001 gateway
USER gateway

EXPOSE 8000
CMD ["uvicorn", "gateway:app", "--host", "0.0.0.0", "--port", "8000"]
