import base64
import json

import requests

from payment import sign_permit2_payment

GATEWAY_URL = "http://localhost:8000/infer"


def build_x_payment_header(requirements: dict, signature: str, authorization: dict) -> str:
    envelope = {
        "x402Version": 2,
        "resource": requirements["resource"],
        "accepted": requirements["accepts"][0],
        "payload": {
            "signature": signature,
            "permit2Authorization": authorization,
        },
    }
    return base64.b64encode(json.dumps(envelope).encode()).decode()


def main():
    prompt = "In one sentence, what is photosynthesis?"

    # 1. POST with no payment -> expect 402 + requirements
    resp = requests.post(GATEWAY_URL, json={"prompt": prompt})
    print(f"[1] no payment -> {resp.status_code}")
    assert resp.status_code == 402
    requirements = resp.json()
    price = int(requirements["accepts"][0]["amount"])
    print(f"    price required: {price} base units")

    # 2. Sign a Permit2 payment for the required amount
    signature, authorization = sign_permit2_payment(amount=price)
    x_payment = build_x_payment_header(requirements, signature, authorization)

    # 3. Retry with X-PAYMENT header -> expect 200 + completion
    resp = requests.post(
        GATEWAY_URL, json={"prompt": prompt}, headers={"X-PAYMENT": x_payment}
    )
    print(f"[2] with payment -> {resp.status_code}")
    assert resp.status_code == 200
    print(f"    completion: {resp.json()['completion']}")
    print(f"    settlement tx: {resp.headers.get('X-PAYMENT-RESPONSE')}")

    # 4. Replay the SAME payload a third time -> expect 409, no second inference
    resp = requests.post(
        GATEWAY_URL, json={"prompt": prompt}, headers={"X-PAYMENT": x_payment}
    )
    print(f"[3] replay same payment -> {resp.status_code}")
    assert resp.status_code == 409
    print(f"    body: {resp.json()}")


if __name__ == "__main__":
    main()
