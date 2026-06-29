/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"golang.org/x/crypto/pbkdf2"
)

const (
	// scramIterations matches Neon upstream's SCRAM_DEFAULT_ITERATIONS.
	scramIterations = 4096
	// scramSaltLen matches Neon upstream's SCRAM_DEFAULT_SALT_LEN (16 bytes).
	scramSaltLen = 16
)

// SCRAMSHA256 encrypts a plaintext password into a SCRAM-SHA-256 verifier string.
//
// The output format is compatible with PostgreSQL's scram-sha-256 authentication
// and can be used directly as the encrypted_password field in compute_ctl's
// Role spec:
//
//	SCRAM-SHA-256$4096:<base64_salt>$<base64_stored_key>:<base64_server_key>
//
// Algorithm (RFC 7677 / RFC 5802):
//  1. Generate random 16-byte salt
//  2. saltedPassword = PBKDF2-HMAC-SHA256(password, salt, 4096)
//  3. ClientKey = HMAC-SHA256(saltedPassword, "Client Key")
//  4. StoredKey = SHA256(ClientKey)
//  5. ServerKey = HMAC-SHA256(saltedPassword, "Server Key")
//
// Note: This implementation does NOT perform SASLprep (RFC 4013) on the password.
// PostgreSQL handles SASLprep on the server side when processing SCRAM verifiers.
// For ASCII passwords (the common case), this is a no-op.
func SCRAMSHA256(password []byte) (string, error) {
	// 1. Generate random salt
	salt := make([]byte, scramSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("scram: generate salt: %w", err)
	}

	return scramSHA256WithSalt(password, salt), nil
}

// scramSHA256WithSalt performs the SCRAM-SHA-256 key derivation with a given salt.
// Exported for deterministic testing.
func scramSHA256WithSalt(password, salt []byte) string {
	// 2. PBKDF2: saltedPassword = Hi(password, salt, 4096)
	saltedPassword := pbkdf2.Key(password, salt, scramIterations, sha256.Size, sha256.New)

	// 3. ClientKey = HMAC-SHA256(saltedPassword, "Client Key")
	clientKey := hmacSHA256(saltedPassword, []byte("Client Key"))

	// 4. StoredKey = SHA256(ClientKey)
	storedKey := sha256Hash(clientKey)

	// 5. ServerKey = HMAC-SHA256(saltedPassword, "Server Key")
	serverKey := hmacSHA256(saltedPassword, []byte("Server Key"))

	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s",
		scramIterations,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(storedKey),
		base64.StdEncoding.EncodeToString(serverKey),
	)
}

// hmacSHA256 computes HMAC-SHA256(key, msg).
func hmacSHA256(key, msg []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(msg)
	return mac.Sum(nil)
}

// sha256Hash computes SHA-256(data).
func sha256Hash(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}
