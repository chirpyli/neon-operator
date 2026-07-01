package storagecontroller_test

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"oltp.molnett.org/neon-operator/specs/storagecontroller"
	"oltp.molnett.org/neon-operator/test/fixtures"
	testutils "oltp.molnett.org/neon-operator/test/utils"
)

func TestSpecs(t *testing.T) {
	cluster := fixtures.NewCluster("test-cluster", "neon")

	// Deterministic Ed25519 key for stable golden test output.
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	pubBytes, _ := x509.MarshalPKIXPublicKey(pub)
	publicKeyPEM := strings.ReplaceAll(
		strings.ReplaceAll(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})), "\n", ""),
		"\r", "")

	dummyToken := "eyJhbGciOiJFZERTQSJ9.dummy"

	cases := []struct {
		name string
		obj  any
	}{
		{"deployment", storagecontroller.Deployment(cluster, publicKeyPEM, dummyToken, dummyToken, dummyToken)},
		{"service", storagecontroller.Service(cluster)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testutils.AssertGolden(t, "testdata/"+tc.name+".yaml", tc.obj)
		})
	}
}
