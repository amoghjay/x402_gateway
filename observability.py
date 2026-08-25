import structlog
from prometheus_client import Counter, Histogram

log = structlog.get_logger()


def configure_logging() -> None:
    structlog.configure(
        processors=[
            structlog.contextvars.merge_contextvars,
            structlog.processors.add_log_level,
            structlog.processors.TimeStamper(fmt="iso"),
            structlog.processors.dict_tracebacks,
            structlog.processors.JSONRenderer(),
        ],
        logger_factory=structlog.PrintLoggerFactory(),
        cache_logger_on_first_use=True,
    )


PAYMENT_REQUIRED = Counter("x402_payment_required_total", "402 challenges issued")

VALIDATIONS = Counter(
    "x402_validations_total", "Free validation outcomes", ["scheme", "outcome"]
)
SETTLEMENTS = Counter(
    "x402_settlements_total", "Settlement outcomes", ["scheme", "outcome"]
)
LLM_CALLS = Counter("x402_llm_calls_total", "Provider calls", ["outcome"])

PHASE_SECONDS = Histogram(
    "x402_phase_duration_seconds",
    "Duration of each request phase",
    ["phase"],
    buckets=(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30),
)

# Only Path B reports gas: on Path A the facilitator submits the tx and we never see
# a receipt. Losing this telemetry is part of what renting the rail costs.
SETTLEMENT_GAS = Histogram(
    "x402_settlement_gas_used",
    "Gas burned per on-chain settlement",
    ["scheme"],
    buckets=(30_000, 60_000, 90_000, 120_000, 150_000, 200_000, 300_000),
)
