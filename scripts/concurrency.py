#!/usr/bin/env python3
"""Exercise three running API processes; creates durable local test wallets."""

import concurrent.futures
import json
import os
import urllib.error
import urllib.parse
import urllib.request
import uuid


APIS = os.environ.get("TEST_API_URLS", "http://localhost:8081,http://localhost:8082,http://localhost:8083").split(",")
ISSUER = os.environ.get("TEST_OIDC_ISSUER", "http://localhost:8080/realms/jungle")


def call(url, token, method="GET", payload=None, key=None):
    headers = {"Authorization": "Bearer " + token}
    data = None

    if payload is not None:
        headers["Content-Type"] = "application/json"
        data = json.dumps(payload).encode()

    if key is not None:
        headers["Idempotency-Key"] = key

    request = urllib.request.Request(url, data=data, headers=headers, method=method)

    try:
        response = urllib.request.urlopen(request, timeout=15)
    except urllib.error.HTTPError as error:
        response = error

    with response:
        return response.status, json.load(response)
    return None


def token(client_id):
    data = urllib.parse.urlencode({
        "grant_type": "client_credentials",
        "client_id": client_id,
        "client_secret": os.environ.get(client_id.upper().replace("-", "_") + "_SECRET", client_id + "-local-secret"),
    }).encode()

    with urllib.request.urlopen(ISSUER + "/protocol/openid-connect/token", data=data, timeout=10) as response:
        return json.load(response)["access_token"]
    return None


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def open_wallet(internal):
    status, wallet = call(APIS[0] + "/wallets", internal, "POST", {
        "playerId": str(uuid.uuid4()),
        "initialBalance": {"amount": "100.00", "currency": "BRL"},
    })
    require(status == 201, "wallet creation failed: " + str(status))

    return wallet


def bet(wallet):
    return {
        "providerId": "provider-a",
        "externalTransactionId": str(uuid.uuid4()),
        "playerId": wallet["playerId"],
        "walletId": wallet["id"],
        "roundId": "concurrency-round",
        "gameId": "concurrency-game",
        "kind": "BET",
        "money": {"amount": "80.00", "currency": "BRL"},
    }


def verify_wallet(internal, wallet):
    status, reconciliation = call(APIS[0] + "/wallets/" + wallet["id"] + "/reconciliation", internal, "POST")
    require(status == 200, "reconciliation failed")
    require(reconciliation["consistent"], "ledger is inconsistent")
    require(reconciliation["storedBalance"]["amount"] == "20.00", "unexpected final balance")
    require(reconciliation["checkedEntries"] == 2, "expected opening and one debit")


def main():
    require(len(APIS) >= 3 and len(set(APIS)) >= 3, "provide at least three distinct process URLs")
    internal = token("jungle-internal")
    provider = token("provider-a")

    for api in APIS:
        status, _ = call(api + "/health/ready", internal)
        require(status == 200, api + " is not ready")

    wallet = open_wallet(internal)
    operation = bet(wallet)
    key = "concurrency:" + operation["externalTransactionId"]

    def duplicate(index):
        return call(APIS[index % len(APIS)] + "/wagering/transactions", provider, "POST", operation, key)

    with concurrent.futures.ThreadPoolExecutor(max_workers=50) as executor:
        results = list(executor.map(duplicate, range(50)))

    require(all(status == 200 and result["status"] == "PROCESSED" for status, result in results), "duplicate processing failed")
    require(sum(result["idempotentReplay"] for _, result in results) == 49, "expected exactly 49 replays")
    require(len({result["transactionId"] for _, result in results}) == 1, "duplicate transaction IDs")
    verify_wallet(internal, wallet)

    contested = open_wallet(internal)
    operations = [bet(contested), bet(contested)]

    def submit(index):
        operation = operations[index]
        return call(APIS[index] + "/wagering/transactions", provider, "POST", operation, "concurrency:" + operation["externalTransactionId"])

    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as executor:
        results = list(executor.map(submit, range(2)))

    require(sorted(status for status, _ in results) == [200, 422], "expected one processed and one rejected bet")
    require(any(result.get("failureCode") == "INSUFFICIENT_BALANCE" for _, result in results), "missing balance rejection")
    verify_wallet(internal, contested)

    print("PASS: three API processes, 50 duplicates, two competing bets, final balances 20.00, consistent ledgers")
    print("Created test wallets:", wallet["id"], contested["id"])


if __name__ == "__main__":
    main()
