"""Typed configuration for the gateway, validated once at startup.

Two things used to be true and are no longer:

1. Config was read as `os.environ["X"]` at module import. A missing variable raised
   `KeyError: 'X'` — naming only whichever variable happened to be read first, and
   raising before uvicorn bound its port, so the failure looked like a crash rather
   than a misconfiguration. Pydantic reports every problem at once, with field names
   and expected types.

2. Several values were not configurable at all: the resource URL was hardcoded into
   the payment protocol itself, and the gas limit, receipt timeout, and explorer URL
   were literals inside functions.

Values here deliberately match what the code did before, literal for literal. This
module moves configuration; it does not retune it.
"""

from pathlib import Path

from eth_account import Account
from pydantic import Field, SecretStr, computed_field, field_validator
from pydantic_settings import BaseSettings, SettingsConfigDict
from web3 import Web3

_ADDRESS_FIELDS = (
    "sbc_contract_address",
    "permit2_contract_address",
    "x402_proxy_address",
    "escrow_contract_address",
    "pay_to_address",
)


class Settings(BaseSettings):
    # env_file is resolved relative to this file, not the process cwd: scripts here
    # run from several directories (contracts/verify_live.py especially) and a
    # cwd-relative lookup silently finds nothing. Real environment variables still
    # win over the file, which is how the container is configured.
    model_config = SettingsConfigDict(
        env_file=Path(__file__).parent / ".env",
        env_file_encoding="utf-8",
        extra="ignore",
        case_sensitive=False,
    )

    # --- Provider -----------------------------------------------------------
    groq_api_key: SecretStr
    groq_model: str = "llama-3.3-70b-versatile"

    # --- Chain and facilitator ----------------------------------------------
    rpc_url: str
    chain_id: int
    facilitator_url: str

    # --- Contracts ----------------------------------------------------------
    sbc_contract_address: str
    permit2_contract_address: str
    x402_proxy_address: str
    escrow_contract_address: str
    pay_to_address: str

    # --- Wallets ------------------------------------------------------------
    # SecretStr, so a settings repr, a traceback, or a structured log line renders
    # these as '**********' instead of a spendable key. Reading one is an explicit
    # .get_secret_value() call, which is easy to grep for in review.
    wallet_key: SecretStr
    gateway_operator_key: SecretStr

    # --- Pricing ------------------------------------------------------------
    # int, not str. x402 puts amounts on the wire as decimal strings because JSON has
    # no integer of this width, but comparing them as strings is a live bug: "9" is
    # greater than "1000" lexicographically. Typed as an int here and serialised with
    # str() at the single point where it enters a JSON body.
    price_base_units: int = Field(gt=0)

    # --- Previously hardcoded in source -------------------------------------
    # Was a module-level literal in payment.py, baked into the payment protocol's
    # `resource` field — so the advertised resource could not follow the deployment.
    resource_url: str = "http://localhost:8000/infer"
    gateway_port: int = 8000

    settlement_gas_limit: int = Field(default=300_000, gt=0)
    receipt_timeout_seconds: float = Field(default=60.0, gt=0)
    receipt_poll_latency_seconds: float = Field(default=0.1, gt=0)
    explorer_base_url: str = "https://testnet.radiustech.xyz"

    @field_validator(*_ADDRESS_FIELDS)
    @classmethod
    def _validate_address(cls, value: str) -> str:
        """Reject a malformed address at startup rather than inside the first
        transaction, where it surfaces as an opaque encoding error. Normalising to
        EIP-55 also makes the value that goes on the wire canonical however it was
        typed into .env."""
        if not Web3.is_address(value):
            raise ValueError(f"not a valid EVM address: {value!r}")
        return Web3.to_checksum_address(value)

    @computed_field
    @property
    def payer_address(self) -> str:
        """Derived, never configured: a PAYER_ADDRESS that disagreed with WALLET_KEY
        would produce signatures that recover to someone else."""
        return Account.from_key(self.wallet_key.get_secret_value()).address

    @computed_field
    @property
    def gateway_operator_address(self) -> str:
        """The address the escrow requires as `auth.settler`. Derived from the
        operator key for the same reason as payer_address."""
        return Account.from_key(self.gateway_operator_key.get_secret_value()).address


settings = Settings()
