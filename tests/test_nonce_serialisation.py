"""_send used to read the "pending" nonce per request, so concurrent sends picked
the same number and one was silently dropped."""

import threading
from types import SimpleNamespace

import pytest

import payment

ADDRESS = "0x000000000000000000000000000000000000dEaD"
CHAIN_NONCE = 5


class _FakeFn:
    def build_transaction(self, tx):
        return tx


class _FakeAccount:
    address = ADDRESS

    def sign_transaction(self, tx):
        return SimpleNamespace(raw_transaction=tx["nonce"])


class _FakeEth:
    """Reports a nonce that never advances — what a real node does for txs that are
    broadcast but not yet mined, which is exactly when the race bit."""

    gas_price = 1

    def __init__(self, sent, fail_once=False):
        self.sent = sent
        self.fail_once = fail_once

    def get_transaction_count(self, address, block):
        return CHAIN_NONCE

    def send_raw_transaction(self, raw):
        if self.fail_once:
            self.fail_once = False
            raise RuntimeError("broadcast rejected")
        self.sent.append(raw)
        return b"\x00"

    def wait_for_transaction_receipt(self, tx_hash, timeout, poll_latency):
        return SimpleNamespace(status=1, transactionHash=tx_hash)


@pytest.fixture(autouse=True)
def _reset(monkeypatch):
    payment._next_nonce.clear()
    yield
    payment._next_nonce.clear()


def _install(monkeypatch, sent, fail_once=False):
    monkeypatch.setattr(payment, "_w3", SimpleNamespace(eth=_FakeEth(sent, fail_once)))


def test_concurrent_sends_get_unique_contiguous_nonces(monkeypatch):
    sent = []
    _install(monkeypatch, sent)

    threads = [threading.Thread(target=payment._send, args=(_FakeAccount(), _FakeFn())) for _ in range(16)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    assert len(sent) == 16
    assert len(set(sent)) == 16, f"duplicate nonces assigned: {sorted(sent)}"
    assert sorted(sent) == list(range(CHAIN_NONCE, CHAIN_NONCE + 16))


def test_failed_broadcast_does_not_leave_a_nonce_gap(monkeypatch):
    sent = []
    _install(monkeypatch, sent, fail_once=True)

    with pytest.raises(RuntimeError):
        payment._send(_FakeAccount(), _FakeFn())

    assert ADDRESS not in payment._next_nonce

    payment._send(_FakeAccount(), _FakeFn())
    assert sent == [CHAIN_NONCE], "nonce should be reused, not skipped"
