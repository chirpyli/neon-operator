package utils

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwt"
	corev1 "k8s.io/api/core/v1"
)

// JWTManager handles JWT operations using keys from Kubernetes secrets
type JWTManager struct {
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
}

// NewJWTManagerFromSecret creates a JWTManager from a Kubernetes secret
func NewJWTManagerFromSecret(secret *corev1.Secret) (*JWTManager, error) {
	privKeyPEM, ok := secret.Data["private.pem"]
	if !ok {
		return nil, fmt.Errorf("private.pem not found in secret")
	}

	pubKeyPEM, ok := secret.Data["public.pem"]
	if !ok {
		return nil, fmt.Errorf("public.pem not found in secret")
	}

	// Parse private key
	privBlock, _ := pem.Decode(privKeyPEM)
	if privBlock == nil {
		return nil, fmt.Errorf("failed to decode private key PEM")
	}

	privKey, err := x509.ParsePKCS8PrivateKey(privBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}

	ed25519PrivKey, ok := privKey.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not Ed25519")
	}

	// Parse public key
	pubBlock, _ := pem.Decode(pubKeyPEM)
	if pubBlock == nil {
		return nil, fmt.Errorf("failed to decode public key PEM")
	}

	pubKey, err := x509.ParsePKIXPublicKey(pubBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse public key: %w", err)
	}

	ed25519PubKey, ok := pubKey.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public key is not Ed25519")
	}

	return &JWTManager{
		privateKey: ed25519PrivKey,
		publicKey:  ed25519PubKey,
	}, nil
}

// GenerateToken creates a new JWT token with the given claims
func (jm *JWTManager) GenerateToken(claims map[string]any) (string, error) {
	token := jwt.New()

	// Add claims to the token
	for key, value := range claims {
		if err := token.Set(key, value); err != nil {
			return "", fmt.Errorf("failed to set claim %s: %w", key, err)
		}
	}

	// Sign the token with Ed25519 private key
	signed, err := jwt.Sign(token, jwt.WithKey(jwa.EdDSA(), jm.privateKey))
	if err != nil {
		return "", fmt.Errorf("failed to sign token: %w", err)
	}

	return string(signed), nil
}

// VerifyToken verifies and parses a JWT token
func (jm *JWTManager) VerifyToken(tokenString string) (jwt.Token, error) {
	return jwt.Parse([]byte(tokenString), jwt.WithKey(jwa.EdDSA(), jm.publicKey))
}

// Scope values matching upstream neon libs/utils/src/auth.rs Scope enum serialization.
// Scope is a single-value enum with #[serde(rename_all = "lowercase")].
const (
	ScopeAdmin          = "admin"
	ScopeInfra          = "infra"
	ScopeGenerationsAPI = "generations_api"
	ScopePageServerAPI  = "pageserverapi"
	ScopeSafekeeperData = "safekeeperdata"
	ScopeTenant         = "tenant"
)

// GenerateScopeToken creates a long-lived, deterministic JWT with the given scope.
// Used for component-to-component authentication (e.g. PS → SC upcall, PS → SK WAL).
//
// DETERMINISM: This function MUST produce the exact same output for the same
// (clusterName, scope, secret) inputs across all invocations. If the token changes
// between reconciles, it triggers a chain reaction:
//
//	ConfigMap.Data changes → checksum in Deployment annotation changes →
//	Deployment Patch (RollingUpdate) → Owns-watch reconcile → repeat
//
// This cascade was observed producing 120+ Deployment revisions in minutes.
//
// Design decisions:
//   - Neon's JWT validation (libs/utils/src/auth.rs) sets required_spec_claims = []
//     and the Claims struct only has { tenant_id, scope, endpoint_id }. iat/iss/nbf
//     are not required and are intentionally omitted.
//   - exp is derived from SHA256(private_key + clusterName + scope) so it is
//     fully deterministic per (key, cluster, scope) tuple. Using time.Now() or any
//     real-time source would defeat determinism.
func (jm *JWTManager) GenerateScopeToken(clusterName, scope string, expireIn time.Duration) (string, error) {
	// Derive a stable "issued-at" time from the private key + cluster + scope.
	// SHA256 produces a deterministic 32-byte hash; we use bytes for the base epoch
	// offset to ensure token stability across all reconcile iterations.
	h := sha256.Sum256(append(append(jm.privateKey.Seed(), []byte(clusterName)...), []byte(scope)...))
	// Use first 4 bytes of hash modulo 3650 as day offset (0-3649 days ≈ 0-10 years).
	// Without modulo, the uint32 hash could cause exp to be millions of years in the future.
	dayOffset := int64(binary.BigEndian.Uint32(h[:4]) % 3650)
	stableBase := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(dayOffset) * 24 * time.Hour)

	claims := map[string]any{
		"sub":   clusterName,
		"scope": scope,
		"exp":   stableBase.Add(expireIn).Unix(),
		// NOTE: Do NOT add "iat" or "iss". Including any time-varying or non-deterministic
		// field would break ConfigMap stability and trigger cascading Deployment rolling restarts.
	}
	return jm.GenerateToken(claims)
}

// GenerateUpcallToken creates a token with "generations_api" scope for PS → SC upcall.
func GenerateUpcallToken(jm *JWTManager, clusterName string) (string, error) {
	return jm.GenerateScopeToken(clusterName, ScopeGenerationsAPI, TokenDefaultLifetime)
}

// GenerateSafekeeperToken creates a token with "safekeeperdata" scope for PS → SK WAL auth.
func GenerateSafekeeperToken(jm *JWTManager, clusterName string) (string, error) {
	return jm.GenerateScopeToken(clusterName, ScopeSafekeeperData, TokenDefaultLifetime)
}

// TokenDefaultLifetime 是组件认证 JWT 的默认有效期（365 天）。
// Token 会持久化到 JWT Secret 中，跨 reconcile 复用。
const TokenDefaultLifetime = 365 * 24 * time.Hour

type JWKResponse struct {
	Keys []*JWK `json:"keys"`
}

// JWK represents the JSON Web Key structure
type JWK struct {
	Use    string   `json:"use"`
	KeyOps []string `json:"key_ops"`
	Alg    string   `json:"alg"`
	Kid    string   `json:"kid"`
	Kty    string   `json:"kty"`
	Crv    string   `json:"crv"`
	X      string   `json:"x"`
}

// PublicKeyHex 返回 Ed25519 公钥的十六进制编码字符串。
// neon 组件通过 --public-key 参数期望这种格式。
func (jm *JWTManager) PublicKeyHex() string {
	return hex.EncodeToString(jm.publicKey)
}

func (jm *JWTManager) ToJWK() *JWK {
	// Encode the public key bytes using base64url (no padding)
	x := base64.RawURLEncoding.EncodeToString(jm.publicKey)

	return &JWK{
		Use:    "sig",
		KeyOps: []string{"verify"},
		Alg:    "EdDSA",
		Kid:    "neon-operator",
		Kty:    "OKP",
		Crv:    "Ed25519",
		X:      x,
	}
}
