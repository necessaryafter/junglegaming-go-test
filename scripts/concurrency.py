#!/usr/bin/env python3
"""Exercise three running API processes; creates durable local test wallets."""

import argparse
import concurrent.futures
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


APIS = os.environ.get("TEST_API_URLS", "http://localhost:8081,http://localhost:8082,http://localhost:8083").split(",")
ISSUER = os.environ.get("TEST_OIDC_ISSUER", "http://localhost:8080/realms/jungle")


def call(url, token, method="GET", payload=None, key=None, timeout=15):
    headers = {"Authorization": "Bearer " + token}
    data = None

    if payload is not None:
        headers["Content-Type"] = "application/json"
        data = json.dumps(payload).encode()

    if key is not None:
        headers["Idempotency-Key"] = key

    request = urllib.request.Request(url, data=data, headers=headers, method=method)

    try:
        response = urllib.request.urlopen(request, timeout=timeout)
    except urllib.error.HTTPError as error:
        response = error

    with response:
        return response.status, json.load(response)


def token(client_id, deadline):
    data = urllib.parse.urlencode({
        "grant_type": "client_credentials",
        "client_id": client_id,
        "client_secret": os.environ.get(client_id.upper().replace("-", "_") + "_SECRET", client_id + "-local-secret"),
    }).encode()

    url = ISSUER + "/protocol/openid-connect/token"
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise RuntimeError("Keycloak did not become ready at " + ISSUER)

        try:
            with urllib.request.urlopen(url, data=data, timeout=min(10, remaining)) as response:
                return json.load(response)["access_token"]
        except urllib.error.HTTPError as error:
            status = error.code
            error.close()
            if status not in (404, 408, 429) and not 500 <= status <= 599:
                raise RuntimeError(
                    f"Keycloak authentication failed for {client_id}: HTTP {status}; "
                    "check the client credentials and realm configuration"
                ) from error
        except OSError:
            pass

        time.sleep(min(.5, max(0, deadline - time.monotonic())))


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
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--duplicates', type=int, default=50)
    parser.add_argument('--concurrency', type=int, default=50)
    parser.add_argument('--startup-timeout', type=int, default=180,
                        help='seconds to wait for Keycloak and the APIs (default: 180)')
    args = parser.parse_args()
    if not 2 <= args.duplicates <= 100000:
        parser.error('duplicates must be between 2 and 100000')
    if not 1 <= args.concurrency <= 256:
        parser.error('concurrency must be between 1 and 256')
    if args.startup_timeout <= 0:
        parser.error('startup-timeout must be positive')

    require(len(APIS) >= 3 and len(set(APIS)) >= 3, "provide at least three distinct process URLs")
    print("WAIT: Keycloak and three API processes", flush=True)
    deadline = time.monotonic() + args.startup_timeout
    internal = token("jungle-internal", deadline)
    provider = token("provider-a", deadline)

    for api in APIS:
        while True:
            remaining = deadline - time.monotonic()
            require(remaining > 0, api + " is not ready")
            try:
                status, _ = call(api + "/health/ready", internal, timeout=min(15, remaining))
                if status == 200:
                    break
            except (OSError, ValueError):
                pass
            time.sleep(min(.25, max(0, deadline - time.monotonic())))
    print("READY: three independent API processes", flush=True)

    wallet = open_wallet(internal)
    operation = bet(wallet)
    key = "concurrency:" + operation["externalTransactionId"]

    def duplicate(index):
        return call(APIS[index % len(APIS)] + "/wagering/transactions", provider, "POST", operation, key)

    print("WALLET: 100.00 BRL | one BET: 80.00 BRL", flush=True)
    print(f"SEND: {args.duplicates} copies | {args.concurrency} concurrent clients | 3 APIs", flush=True)
    results = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as executor:
        futures = [executor.submit(duplicate, index) for index in range(args.duplicates)]
        interval = max(1, args.duplicates // 5)
        for future in concurrent.futures.as_completed(futures):
            results.append(future.result())
            if len(results) % interval == 0 or len(results) == args.duplicates:
                print(f"RECEIVED: {len(results)}/{args.duplicates}", flush=True)

    require(all(status == 200 and result["status"] == "PROCESSED" for status, result in results), "duplicate processing failed")
    require(sum(result["idempotentReplay"] for _, result in results) == args.duplicates - 1, "unexpected replay count")
    require(len({result["transactionId"] for _, result in results}) == 1, "duplicate transaction IDs")
    verify_wallet(internal, wallet)
    print(f"PASS: 1 processed | {args.duplicates - 1} replays | 1 debit", flush=True)
    print("RECONCILIATION: balance 20.00 BRL | opening + debit = 2 entries", flush=True)

    print("RACE: two distinct BETs of 80.00 BRL against 100.00 BRL", flush=True)
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

    print("PASS: 1 processed | 1 INSUFFICIENT_BALANCE | balance 20.00 BRL", flush=True)
    print("PASS: both wallets reconcile; no duplicate financial movement", flush=True)
    print("Created test wallets:", wallet["id"], contested["id"])


if __name__ == "__main__":
    main()
