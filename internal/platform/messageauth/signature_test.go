package messageauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	jose "github.com/go-jose/go-jose/v4"
)

func signingMaterial(t *testing.T) (ed25519.PrivateKey, ProducerKey) {
	t.Helper()

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	return private, ProducerKey{
		ProviderID: "provider-a",
		Key:        jose.JSONWebKey{Key: public, KeyID: "provider-a-v1", Algorithm: string(jose.EdDSA), Use: "sig"},
	}
}

func verifierFor(t *testing.T, entries ...ProducerKey) *Verifier {
	t.Helper()

	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}

	verifier, err := parseKeys(data)
	if err != nil {
		t.Fatal(err)
	}

	return verifier
}

func TestDurableMessageIdentity(t *testing.T) {
	private, entry := signingMaterial(t)
	body := `{"occurredAt":"2000-01-01T00:00:00Z","data":{"providerId":"provider-b","money":{"amount":"25.00"}}}`
	signature, err := Sign([]byte(body), entry.Key.KeyID, private)
	if err != nil {
		t.Fatal(err)
	}

	// Reloading public keys simulates another process after arbitrary queue delay.
	verifier := verifierFor(t, entry)
	principal, err := verifier.Verify(body, signature)
	if err != nil {
		t.Fatal(err)
	}

	if principal.ProviderID != "provider-a" {
		t.Fatal("body providerId replaced the verified producer identity")
	}

	if principal.Internal {
		t.Fatal("producer signature granted internal wallet permissions")
	}

	if strings.Split(signature, ".")[1] != "" {
		t.Fatal("signature duplicated the financial payload")
	}
}

func TestSignatureRejectsTamperingAndUntrustedKeys(t *testing.T) {
	private, entry := signingMaterial(t)
	otherPrivate, _ := signingMaterial(t)
	body := `{"messageId":"message","data":{"providerId":"provider-a"}}`
	valid, err := Sign([]byte(body), entry.Key.KeyID, private)
	if err != nil {
		t.Fatal(err)
	}

	wrongKey, err := Sign([]byte(body), entry.Key.KeyID, otherPrivate)
	if err != nil {
		t.Fatal(err)
	}

	unknownKey, err := Sign([]byte(body), "unknown", private)
	if err != nil {
		t.Fatal(err)
	}

	verifier := verifierFor(t, entry)
	tests := []struct {
		name      string
		body      string
		signature string
	}{
		{name: "missing signature", body: body},
		{name: "malformed signature", body: body, signature: "not-a-JWS"},
		{name: "different private key", body: body, signature: wrongKey},
		{name: "unknown key ID", body: body, signature: unknownKey},
		{name: "changed business payload", body: strings.Replace(body, "provider-a", "provider-b", 1), signature: valid},
		{name: "changed whitespace", body: body + "\n", signature: valid},
		{name: "changed envelope messageId", body: strings.Replace(body, "message\"", "another\"", 1), signature: valid},
		{name: "oversized signature", body: body, signature: strings.Repeat("x", 2049)},
		{name: "oversized body", body: strings.Repeat("x", 262145), signature: valid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := verifier.Verify(tt.body, tt.signature); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("verification = %v, want unauthorized", err)
			}
		})
	}
}

func TestSignatureRejectsOtherAlgorithmsAndTypes(t *testing.T) {
	private, entry := signingMaterial(t)
	body := []byte("message")
	verifier := verifierFor(t, entry)
	tests := []struct {
		name string
		alg  jose.SignatureAlgorithm
		key  any
		typ  jose.ContentType
	}{
		{name: "symmetric algorithm", alg: jose.HS256, key: []byte(strings.Repeat("x", 32)), typ: signatureType},
		{name: "JWT instead of message signature", alg: jose.EdDSA, key: private, typ: "JWT"},
		{name: "missing type", alg: jose.EdDSA, key: private},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := (&jose.SignerOptions{}).WithType(tt.typ).WithHeader(jose.HeaderKey("kid"), entry.Key.KeyID)
			signer, err := jose.NewSigner(jose.SigningKey{Algorithm: tt.alg, Key: tt.key}, options)
			if err != nil {
				t.Fatal(err)
			}

			signed, err := signer.Sign(body)
			if err != nil {
				t.Fatal(err)
			}

			signature, err := signed.DetachedCompactSerialize()
			if err != nil {
				t.Fatal(err)
			}

			if _, err := verifier.Verify(string(body), signature); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("verification = %v, want unauthorized", err)
			}
		})
	}
}

func TestRegistryFailsClosed(t *testing.T) {
	private, valid := signingMaterial(t)
	tests := []struct {
		name   string
		change func(*ProducerKey)
	}{
		{name: "missing provider", change: func(k *ProducerKey) { k.ProviderID = "" }},
		{name: "missing key ID", change: func(k *ProducerKey) { k.Key.KeyID = "" }},
		{name: "wrong key algorithm", change: func(k *ProducerKey) { k.Key.Algorithm = "HS256" }},
		{name: "wrong key use", change: func(k *ProducerKey) { k.Key.Use = "enc" }},
		{name: "private key in runtime registry", change: func(k *ProducerKey) { k.Key.Key = private }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := valid
			tt.change(&entry)
			data, err := json.Marshal([]ProducerKey{entry})
			if err != nil {
				t.Fatal(err)
			}

			if _, err := parseKeys(data); err == nil {
				t.Fatal("invalid registry was accepted")
			}
		})
	}

	for _, data := range []string{"[]", "null", "not JSON"} {
		if _, err := parseKeys([]byte(data)); err == nil {
			t.Fatalf("invalid registry %q was accepted", data)
		}
	}

	other := valid
	other.ProviderID = "provider-b"
	other.Key.KeyID = "provider-b-v1"

	for _, entries := range [][]ProducerKey{{valid, valid}, {valid, other}} {
		data, err := json.Marshal(entries)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := parseKeys(data); err == nil {
			t.Fatal("duplicate key identity or shared signing key was accepted")
		}
	}
}
