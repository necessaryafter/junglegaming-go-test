"""Provision local IAM identities and isolated signing material; never print secrets."""
import base64
import configparser
import json
import os
from pathlib import Path
import tempfile

import boto3
from botocore.exceptions import ClientError
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

ENDPOINT = os.environ.get("SQS_ENDPOINT", "http://localstack:4566")
REGION = os.environ.get("AWS_DEFAULT_REGION", "us-east-1")
DIRECTORY = Path(__file__).parent
OWNER = 65532
sqs = boto3.client("sqs", endpoint_url=ENDPOINT, region_name=REGION)
iam = boto3.client("iam", endpoint_url=ENDPOINT, region_name=REGION)


def atomic_write(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    os.chmod(path.parent, 0o700)
    os.chown(path.parent, OWNER, OWNER)
    with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as temporary:
        temporary.write(data)
        name = temporary.name
    try:
        os.chmod(name, 0o600)
        os.chown(name, OWNER, OWNER)
        os.replace(name, path)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def user(name, directory, policy):
    try:
        iam.get_user(UserName=name)
    except ClientError as error:
        if error.response["Error"]["Code"] != "NoSuchEntity":
            raise
        iam.create_user(UserName=name)
    inline = iam.list_user_policies(UserName=name)["PolicyNames"]
    attached = iam.list_attached_user_policies(UserName=name)["AttachedPolicies"]
    groups = iam.list_groups_for_user(UserName=name)["Groups"]
    if any(name != "jungle-access" for name in inline) or attached or groups:
        raise ValueError("unexpected grants on a local SQS identity")
    iam.put_user_policy(UserName=name, PolicyName="jungle-access", PolicyDocument=policy)
    path = directory / "credentials"
    existing = iam.list_access_keys(UserName=name)["AccessKeyMetadata"]
    parser = configparser.ConfigParser()
    if path.exists():
        parser.read(path)
        saved = parser.get("default", "aws_access_key_id")
        if any(key["AccessKeyId"] == saved and key["Status"] == "Active" for key in existing):
            return
    # Only local identities owned by this bootstrap are reconciled.
    for key in existing:
        iam.delete_access_key(UserName=name, AccessKeyId=key["AccessKeyId"])
    key = iam.create_access_key(UserName=name)["AccessKey"]
    content = f"[default]\naws_access_key_id = {key['AccessKeyId']}\naws_secret_access_key = {key['SecretAccessKey']}\n"
    atomic_write(path, content.encode())


def policy(file, arns):
    document = (DIRECTORY / file).read_text()
    for name, value in arns.items():
        document = document.replace("${" + name + "}", value)
    json.loads(document)
    return document


arns = {}
for variable, queue in {
    "REQUESTS_ARN": "wager-transactions.fifo",
    "REQUESTS_DLQ_ARN": "wager-transactions-dlq.fifo",
    "EVENTS_ARN": "wager-events.fifo",
    "EVENTS_DLQ_ARN": "wager-events-dlq.fifo",
}.items():
    url = sqs.get_queue_url(QueueName=queue)["QueueUrl"]
    arns[variable] = sqs.get_queue_attributes(QueueUrl=url, AttributeNames=["QueueArn"])["Attributes"]["QueueArn"]

runtime = Path("/runtime")
user("jungle-runtime", runtime, policy("runtime-policy.json", arns))
registry = []
for provider in ("provider-a", "provider-b"):
    directory = Path("/" + provider)
    user(provider, directory, policy("producer-policy.json", arns))
    path = directory / "signing.pem"
    if path.exists():
        private = serialization.load_pem_private_key(path.read_bytes(), password=None)
        if not isinstance(private, Ed25519PrivateKey):
            raise ValueError("expected an existing Ed25519 signing key")
    else:
        private = Ed25519PrivateKey.generate()
        atomic_write(path, private.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()))
    public = private.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
    registry.append({
        "providerId": provider,
        "key": {
            "kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig",
            "kid": provider + "-v1", "x": base64.urlsafe_b64encode(public).decode().rstrip("="),
        },
    })
atomic_write(runtime / "producers.json", json.dumps(registry, indent=2).encode())
print("SQS identities, permissions and verification keys provisioned.")
