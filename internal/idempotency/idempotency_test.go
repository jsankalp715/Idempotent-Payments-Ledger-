package idempotency

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestValidateKey(t *testing.T) {
	valid := []string{
		"a",
		"3f2b8c1e-6f1d-4c55-9d0e-8a1b2c3d4e5f",
		"01J9ZKX6V4Q2N8M5T7R3W1Y0PA",
		"order-42:attempt~1",
		strings.Repeat("k", MaxKeyLength),
	}
	for _, k := range valid {
		if err := ValidateKey(k); err != nil {
			t.Errorf("ValidateKey(%q) = %v, want nil", k, err)
		}
	}
	invalid := []string{
		"",
		strings.Repeat("k", MaxKeyLength+1),
		"has space",
		"tab\tinside",
		"new\nline",
		"ünïcode",
		"\x00",
	}
	for _, k := range invalid {
		if err := ValidateKey(k); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("ValidateKey(%q) = %v, want ErrInvalidKey", k, err)
		}
	}
}

type transferShape struct {
	From     string `json:"from_account"`
	To       string `json:"to_account"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

func mustFingerprint(t *testing.T, method, path string, v any) []byte {
	t.Helper()
	fp, err := Fingerprint(method, path, v)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if len(fp) != 32 {
		t.Fatalf("fingerprint length %d, want 32", len(fp))
	}
	return fp
}

func TestFingerprintIsDeterministic(t *testing.T) {
	req := transferShape{From: "a", To: "b", Amount: 100, Currency: "USD"}
	first := mustFingerprint(t, "POST", "/v1/transfers", req)
	for i := 0; i < 100; i++ {
		if !bytes.Equal(first, mustFingerprint(t, "POST", "/v1/transfers", req)) {
			t.Fatal("fingerprint changed between calls")
		}
	}
}

func TestFingerprintIgnoresJSONFormatting(t *testing.T) {
	// Clients may serialize the same request differently; hashing the parsed
	// request makes those encodings equivalent.
	bodies := []string{
		`{"from_account":"a","to_account":"b","amount":100,"currency":"USD"}`,
		`{"currency":"USD","amount":100,"to_account":"b","from_account":"a"}`,
		"{\n  \"from_account\": \"a\",\n  \"to_account\": \"b\",\n  \"amount\": 100,\n  \"currency\": \"USD\"\n}",
		`{"from_account":"a","to_account":"b","amount":1e2,"currency":"USD"}`,
	}
	var want []byte
	for i, body := range bodies {
		var v struct {
			From     string  `json:"from_account"`
			To       string  `json:"to_account"`
			Amount   float64 `json:"amount"`
			Currency string  `json:"currency"`
		}
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatalf("body %d: %v", i, err)
		}
		fp := mustFingerprint(t, "POST", "/v1/transfers", transferShape{From: v.From, To: v.To, Amount: int64(v.Amount), Currency: v.Currency})
		if want == nil {
			want = fp
		} else if !bytes.Equal(fp, want) {
			t.Fatalf("body %d hashed differently", i)
		}
	}
}

func TestFingerprintDistinguishesRequests(t *testing.T) {
	base := transferShape{From: "a", To: "b", Amount: 100, Currency: "USD"}
	baseFP := mustFingerprint(t, "POST", "/v1/transfers", base)
	variants := map[string][]byte{
		"amount":   mustFingerprint(t, "POST", "/v1/transfers", transferShape{From: "a", To: "b", Amount: 101, Currency: "USD"}),
		"currency": mustFingerprint(t, "POST", "/v1/transfers", transferShape{From: "a", To: "b", Amount: 100, Currency: "EUR"}),
		"from":     mustFingerprint(t, "POST", "/v1/transfers", transferShape{From: "c", To: "b", Amount: 100, Currency: "USD"}),
		"swapped":  mustFingerprint(t, "POST", "/v1/transfers", transferShape{From: "b", To: "a", Amount: 100, Currency: "USD"}),
		"path":     mustFingerprint(t, "POST", "/v1/accounts", base),
		"method":   mustFingerprint(t, "PUT", "/v1/transfers", base),
	}
	seen := map[string]string{string(baseFP): "base"}
	for name, fp := range variants {
		if prev, dup := seen[string(fp)]; dup {
			t.Errorf("variant %q hashes like %q", name, prev)
		}
		seen[string(fp)] = name
	}
}

func TestFingerprintPartsAreUnambiguous(t *testing.T) {
	// Without length prefixes, method "POST" + path "/x" and method "POST/" +
	// path "x" would concatenate to the same bytes.
	a := mustFingerprint(t, "POST", "/x", struct{}{})
	b := mustFingerprint(t, "POST/", "x", struct{}{})
	if bytes.Equal(a, b) {
		t.Fatal("different method/path splits produced the same fingerprint")
	}
}

func TestFingerprintRejectsUnencodableInput(t *testing.T) {
	if _, err := Fingerprint("POST", "/x", map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("expected an encoding error")
	}
}

func TestLockIDIsStableAndSpread(t *testing.T) {
	first, second := LockID("k"), LockID("k")
	if first != second {
		t.Fatal("LockID is not deterministic")
	}
	seen := map[int64]bool{}
	for i := 0; i < 10000; i++ {
		seen[LockID(fmt.Sprintf("key-%d", i))] = true
	}
	if len(seen) != 10000 {
		t.Fatalf("LockID collides too often: %d distinct ids for 10000 keys", len(seen))
	}
}
