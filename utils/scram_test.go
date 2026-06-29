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
	"encoding/base64"
	"strings"
	"testing"
)

func TestSCRAMSHA256_Format(t *testing.T) {
	verifier, err := SCRAMSHA256([]byte("test-password"))
	if err != nil {
		t.Fatalf("SCRAMSHA256 failed: %v", err)
	}

	// Expected format: SCRAM-SHA-256$<iterations>:<salt>$<stored_key>:<server_key>
	// $-delimited parts: [SCRAM-SHA-256, <iter>:<salt>, <stored_key>:<server_key>]
	if !strings.HasPrefix(verifier, "SCRAM-SHA-256$") {
		t.Errorf("verifier does not start with expected prefix: %s", verifier)
	}

	parts := strings.SplitN(verifier, "$", 3)
	if len(parts) != 3 {
		t.Fatalf("expected 3 $-delimited parts, got %d: %s", len(parts), verifier)
	}

	if parts[0] != "SCRAM-SHA-256" {
		t.Errorf("expected scheme 'SCRAM-SHA-256', got %q", parts[0])
	}

	// parts[1] = "<iterations>:<base64_salt>"
	iterSalt := strings.SplitN(parts[1], ":", 2)
	if len(iterSalt) != 2 {
		t.Fatalf("expected 2 ':'-delimited parts in %q, got %d", parts[1], len(iterSalt))
	}
	if iterSalt[0] != "4096" {
		t.Errorf("expected iterations '4096', got %q", iterSalt[0])
	}

	// Validate salt is valid base64 (16 bytes decodes to 24 chars base64)
	salt, err := base64.StdEncoding.DecodeString(iterSalt[1])
	if err != nil {
		t.Fatalf("salt is not valid base64: %v", err)
	}
	if len(salt) != 16 {
		t.Errorf("expected 16-byte salt, got %d bytes", len(salt))
	}

	// parts[2] = "<base64_stored_key>:<base64_server_key>"
	keys := strings.SplitN(parts[2], ":", 2)
	if len(keys) != 2 {
		t.Fatalf("expected 2 ':'-delimited key parts in %q, got %d", parts[2], len(keys))
	}

	// Validate stored_key is valid base64 (32 bytes = SHA-256 output)
	storedKey, err := base64.StdEncoding.DecodeString(keys[0])
	if err != nil {
		t.Fatalf("stored_key is not valid base64: %v", err)
	}
	if len(storedKey) != 32 {
		t.Errorf("expected 32-byte stored_key, got %d bytes", len(storedKey))
	}

	// Validate server_key is valid base64 (32 bytes = SHA-256 output)
	serverKey, err := base64.StdEncoding.DecodeString(keys[1])
	if err != nil {
		t.Fatalf("server_key is not valid base64: %v", err)
	}
	if len(serverKey) != 32 {
		t.Errorf("expected 32-byte server_key, got %d bytes", len(serverKey))
	}
}

func TestSCRAMSHA256_Deterministic(t *testing.T) {
	// Same password + same salt should produce identical verifier
	salt := []byte("0123456789abcdef")
	password := []byte("my-secret-password")

	v1 := scramSHA256WithSalt(password, salt)
	v2 := scramSHA256WithSalt(password, salt)

	if v1 != v2 {
		t.Errorf("expected identical verifiers, got:\n  %s\n  %s", v1, v2)
	}
}

func TestSCRAMSHA256_DifferentPasswords(t *testing.T) {
	salt := []byte("0123456789abcdef")

	v1 := scramSHA256WithSalt([]byte("password1"), salt)
	v2 := scramSHA256WithSalt([]byte("password2"), salt)

	if v1 == v2 {
		t.Error("expected different verifiers for different passwords")
	}
}

func TestSCRAMSHA256_DifferentSalts(t *testing.T) {
	password := []byte("same-password")

	v1 := scramSHA256WithSalt(password, []byte("0123456789abcdef"))
	v2 := scramSHA256WithSalt(password, []byte("fedcba9876543210"))

	if v1 == v2 {
		t.Error("expected different verifiers for different salts")
	}
}

func TestSCRAMSHA256_ComputeCTLCompatible(t *testing.T) {
	// Verify the format is compatible with what compute_ctl expects.
	// compute_ctl checks if encrypted_password starts with "SCRAM-SHA-256"
	// (see pg_helpers.rs:143-168)
	verifier, err := SCRAMSHA256([]byte("compat-test"))
	if err != nil {
		t.Fatalf("SCRAMSHA256 failed: %v", err)
	}

	if !strings.HasPrefix(verifier, "SCRAM-SHA-256") {
		t.Errorf("verifier must start with 'SCRAM-SHA-256' for compute_ctl compatibility: %s", verifier)
	}
}
