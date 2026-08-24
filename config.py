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
    # env_file is module-relative, not cwd-relative: contracts/verify_live.py runs
    # from another directory. Real env vars still win over the file.
    model_config = SettingsConfigDict(
        env_file=Path(__file__).parent / ".env",
        env_file_encoding="utf-8",
        extra="ignore",
        case_sensitive=False,
    )

    groq_api_key: SecretStr
    groq_model: str = "llama-3.3-70b-versatile"
    max_completion_tokens: int = Field(default=512, gt=0)

    rpc_url: str
    chain_id: int
    facilitator_url: str

    sbc_contract_address: str
    permit2_contract_address: str
    x402_proxy_address: str
    escrow_contract_address: str
    pay_to_address: str

    # SecretStr so a repr, log line, or traceback renders '**********'.
    wallet_key: SecretStr
    gateway_operator_key: SecretStr

    # int, not str: x402 puts amounts on the wire as decimal strings, and comparing
    # those as strings is a live bug ("9" > "1000"). str() only at serialisation.
    price_base_units: int = Field(gt=0)

    resource_url: str = "http://localhost:8000/infer"
    gateway_port: int = 8000

    settlement_gas_limit: int = Field(default=300_000, gt=0)
    receipt_timeout_seconds: float = Field(default=20.0, gt=0)
    receipt_poll_latency_seconds: float = Field(default=0.5, gt=0)
    explorer_base_url: str = "https://testnet.radiustech.xyz"

    @field_validator(*_ADDRESS_FIELDS)
    @classmethod
    def _validate_address(cls, value: str) -> str:
        if not Web3.is_address(value):
            raise ValueError(f"not a valid EVM address: {value!r}")
        return Web3.to_checksum_address(value)

    # Derived, never configured: a configured address that disagreed with its key
    # would produce signatures recovering to someone else.
    @computed_field
    @property
    def payer_address(self) -> str:
        return Account.from_key(self.wallet_key.get_secret_value()).address

    @computed_field
    @property
    def gateway_operator_address(self) -> str:
        return Account.from_key(self.gateway_operator_key.get_secret_value()).address


settings = Settings()
