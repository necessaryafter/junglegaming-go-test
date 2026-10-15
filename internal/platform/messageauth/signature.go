// Package messageauth verifies durable producer signatures independently of JWT lifetimes.
package messageauth

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	jose "github.com/go-jose/go-jose/v4"

	"jungle/internal/platform/config"
	"jungle/internal/platform/identity"
)

const Attribute = "MessageSignature"

const signatureType = "jungle-sqs-v1"

var ErrUnauthorized = errors.New("messageauth: invalid producer signature")

// ProducerKey binds an Ed25519 verification key to one authorized producer.
// Key IDs remain available for queued messages during signing-key rotation.
type ProducerKey struct {
	ProviderID string          `json:"providerId"`
	Key        jose.JSONWebKey `json:"key"`
}

type Verifier struct {
	keys map[string]ProducerKey
}

func New(cfg config.Config) (*Verifier, error) {
	data, err := os.ReadFile(cfg.ProducerKeysFile)
	if err != nil {
		return nil, fmt.Errorf("read SQS producer keys: %w", err)
	}

	return parseKeys(data)
}

func parseKeys(data []byte) (*Verifier, error) {
	var keys []ProducerKey
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, fmt.Errorf("decode SQS producer keys: %w", err)
	}

	if len(keys) == 0 {
		return nil, errors.New("messageauth: no producer keys configured")
	}

	verifier := &Verifier{keys: make(map[string]ProducerKey, len(keys))}
	publicKeys := make(map[string]bool, len(keys))

	for _, entry := range keys {
		if strings.TrimSpace(entry.ProviderID) == "" || len(entry.ProviderID) > 200 {
			return nil, errors.New("messageauth: invalid provider identity")
		}

		if entry.Key.KeyID == "" || len(entry.Key.KeyID) > 200 {
			return nil, errors.New("messageauth: invalid key ID")
		}

		public, ok := entry.Key.Key.(ed25519.PublicKey)
		if !ok {
			return nil, errors.New("messageauth: expected an Ed25519 public key")
		}

		if entry.Key.Algorithm != string(jose.EdDSA) {
			return nil, errors.New("messageauth: key algorithm must be EdDSA")
		}

		if entry.Key.Use != "sig" {
			return nil, errors.New("messageauth: key use must be sig")
		}

		if _, exists := verifier.keys[entry.Key.KeyID]; exists {
			return nil, errors.New("messageauth: duplicate key ID")
		}

		if publicKeys[string(public)] {
			return nil, errors.New("messageauth: signing keys must be distinct")
		}

		publicKeys[string(public)] = true
		verifier.keys[entry.Key.KeyID] = entry
	}

	return verifier, nil
}

// Verify authenticates the exact queued bytes. It accepts only detached compact
// JWS with a protected key ID, type and EdDSA algorithm; it never fetches keys
// from message headers. Replays remain subject to the application's inbox.
func (v *Verifier) Verify(body string, signature string) (identity.Principal, error) {
	if len(body) > 262144 {
		return identity.Principal{}, ErrUnauthorized
	}

	if signature == "" || len(signature) > 2048 {
		return identity.Principal{}, ErrUnauthorized
	}

	parts := strings.Split(signature, ".")
	if len(parts) != 3 {
		return identity.Principal{}, ErrUnauthorized
	}

	if parts[1] != "" {
		return identity.Principal{}, ErrUnauthorized
	}

	signed, err := jose.ParseDetached(signature, []byte(body), []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil {
		return identity.Principal{}, errors.Join(ErrUnauthorized, err)
	}

	if len(signed.Signatures) != 1 {
		return identity.Principal{}, ErrUnauthorized
	}

	header := signed.Signatures[0].Protected
	if header.ExtraHeaders[jose.HeaderType] != signatureType {
		return identity.Principal{}, ErrUnauthorized
	}

	entry, exists := v.keys[header.KeyID]
	if !exists {
		return identity.Principal{}, ErrUnauthorized
	}

	if err := signed.DetachedVerify([]byte(body), entry.Key.Key); err != nil {
		return identity.Principal{}, errors.Join(ErrUnauthorized, err)
	}

	return identity.Principal{ProviderID: entry.ProviderID}, nil
}

// Sign produces a detached JWS; it is a message signature, not an access token.
func Sign(body []byte, keyID string, key ed25519.PrivateKey) (string, error) {
	if keyID == "" || len(keyID) > 200 {
		return "", errors.New("messageauth: invalid signing key ID")
	}

	if len(key) != ed25519.PrivateKeySize {
		return "", errors.New("messageauth: invalid Ed25519 private key")
	}

	if len(body) > 262144 {
		return "", errors.New("messageauth: message exceeds size limit")
	}

	options := (&jose.SignerOptions{}).WithType(signatureType).WithHeader(jose.HeaderKey("kid"), keyID)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: key}, options)
	if err != nil {
		return "", fmt.Errorf("create message signer: %w", err)
	}

	signed, err := signer.Sign(body)
	if err != nil {
		return "", fmt.Errorf("sign message: %w", err)
	}

	return signed.DetachedCompactSerialize()
}

// ReadPrivateKey loads a PKCS#8 Ed25519 PEM file without retaining its bytes.
func ReadPrivateKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}

	block, rest := pem.Decode(data)
	if block == nil {
		return nil, errors.New("messageauth: invalid private key PEM")
	}

	if block.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("messageauth: expected one PKCS#8 private key")
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse signing key: %w", err)
	}

	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("messageauth: signing key must be Ed25519")
	}

	return key, nil
}
