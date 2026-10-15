// Package sqsidentity creates isolated signing material for integration fixtures.
// Production packages must not import this package.
package sqsidentity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	jose "github.com/go-jose/go-jose/v4"

	"jungle/internal/platform/config"
	"jungle/internal/platform/messageauth"
)

func Configure(t *testing.T) {
	t.Helper()

	directory := t.TempDir()
	var registry []messageauth.ProducerKey

	for _, provider := range []string{"provider-a", "provider-b"} {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}

		encoded, err := x509.MarshalPKCS8PrivateKey(private)
		if err != nil {
			t.Fatal(err)
		}

		data := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
		if err := os.WriteFile(filepath.Join(directory, provider+".pem"), data, 0600); err != nil {
			t.Fatal(err)
		}

		registry = append(registry, messageauth.ProducerKey{
			ProviderID: provider,
			Key:        jose.JSONWebKey{Key: public, KeyID: provider + "-v1", Algorithm: string(jose.EdDSA), Use: "sig"},
		})
	}

	encoded, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(directory, "producers.json")
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SQS_PRODUCER_KEYS_FILE", path)
	t.Setenv("TEST_SQS_SIGNING_KEYS", directory)
}

func Verifier(t *testing.T) *messageauth.Verifier {
	t.Helper()

	verifier, err := messageauth.New(config.Config{ProducerKeysFile: os.Getenv("SQS_PRODUCER_KEYS_FILE")})
	if err != nil {
		t.Fatal(err)
	}

	return verifier
}

func Signature(t *testing.T, provider string, body string) string {
	t.Helper()

	directory := os.Getenv("TEST_SQS_SIGNING_KEYS")
	if directory == "" {
		t.Fatal("Configure must initialize the fixture's signing keys")
	}

	key, err := messageauth.ReadPrivateKey(filepath.Join(directory, provider+".pem"))
	if err != nil {
		t.Fatal(err)
	}

	signature, err := messageauth.Sign([]byte(body), provider+"-v1", key)
	if err != nil {
		t.Fatal(err)
	}

	return signature
}
