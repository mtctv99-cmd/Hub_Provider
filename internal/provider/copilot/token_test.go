package copilot

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

func testTokenURL(host, path string, exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp)))
	return fmt.Sprintf("wss://%s%s?access_token=%s.%s.signature", host, path, header, payload)
}

func testCapturedTokenURL(host, oid, tid, email string, exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"oid":"%s","tid":"%s","preferred_username":"%s"}`, exp, oid, tid, email)))
	path := fmt.Sprintf("/m365Copilot/Chathub/%s@%s", oid, tid)
	return fmt.Sprintf("wss://%s%s?access_token=%s.%s.signature", host, path, header, payload)
}

func TestValidateCapturedToken(t *testing.T) {
	oid := "11111111-2222-3333-4444-555555555555"
	tid := "66666666-7777-8888-9999-000000000000"
	future := time.Now().Add(time.Hour).Unix()
	valid := testCapturedTokenURL("substrate.office.com", oid, tid, "user@example.com", future)
	expiry, email, err := ValidateCapturedToken(valid, 30)
	if err != nil {
		t.Fatalf("expected valid token: %v", err)
	}
	if email != "user@example.com" {
		t.Fatalf("expected email user@example.com, got %s", email)
	}
	if expiry.Before(time.Now()) {
		t.Fatalf("expected future expiry, got %v", expiry)
	}

	// Case insensitive match
	upperValid := testCapturedTokenURL("substrate.office.com", strings.ToUpper(oid), strings.ToUpper(tid), "user@example.com", future)
	if _, _, err := ValidateCapturedToken(upperValid, 30); err != nil {
		t.Fatalf("expected case-insensitive OID/TID match: %v", err)
	}

	// Test fallback to upn
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	upnPayload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"oid":"%s","tid":"%s","upn":"upnuser@example.com"}`, future, oid, tid)))
	upnURL := fmt.Sprintf("wss://substrate.office.com/m365Copilot/Chathub/%s@%s?access_token=%s.%s.signature", oid, tid, header, upnPayload)
	_, upnEmail, err := ValidateCapturedToken(upnURL, 30)
	if err != nil || upnEmail != "upnuser@example.com" {
		t.Fatalf("expected upn fallback email, got %s, err: %v", upnEmail, err)
	}

	// Test fallback to unique_name and padded base64
	uniqPayload := base64.URLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"oid":"%s","tid":"%s","unique_name":"uniq@example.com"}`, future, oid, tid)))
	uniqURL := fmt.Sprintf("wss://substrate.office.com/m365Copilot/Chathub/%s@%s?access_token=%s.%s.signature", oid, tid, header, uniqPayload)
	_, uniqEmail, err := ValidateCapturedToken(uniqURL, 30)
	if err != nil || uniqEmail != "uniq@example.com" {
		t.Fatalf("expected unique_name fallback email, got %s, err: %v", uniqEmail, err)
	}

	// Mismatched OID
	mismatchedOID := testCapturedTokenURL("substrate.office.com", "22222222-2222-3333-4444-555555555555", tid, "user@example.com", future)
	// Replace path to have original oid so claims don't match
	mismatchedOID = strings.Replace(mismatchedOID, "22222222-2222-3333-4444-555555555555@", oid+"@", 1)
	if _, _, err := ValidateCapturedToken(mismatchedOID, 30); err == nil {
		t.Fatal("expected failure on mismatched OID")
	}

	// Mismatched TID
	mismatchedTID := testCapturedTokenURL("substrate.office.com", oid, "77777777-7777-8888-9999-000000000000", "user@example.com", future)
	mismatchedTID = strings.Replace(mismatchedTID, "@77777777-7777-8888-9999-000000000000", "@"+tid, 1)
	if _, _, err := ValidateCapturedToken(mismatchedTID, 30); err == nil {
		t.Fatal("expected failure on mismatched TID")
	}

	// Missing user email
	noEmailPayload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"oid":"%s","tid":"%s"}`, future, oid, tid)))
	noEmailURL := fmt.Sprintf("wss://substrate.office.com/m365Copilot/Chathub/%s@%s?access_token=%s.%s.signature", oid, tid, header, noEmailPayload)
	if _, _, err := ValidateCapturedToken(noEmailURL, 30); err == nil {
		t.Fatal("expected failure on missing user email")
	}

	// Invalid email format
	badEmailPayload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"oid":"%s","tid":"%s","preferred_username":"not-an-email"}`, future, oid, tid)))
	badEmailURL := fmt.Sprintf("wss://substrate.office.com/m365Copilot/Chathub/%s@%s?access_token=%s.%s.signature", oid, tid, header, badEmailPayload)
	if _, _, err := ValidateCapturedToken(badEmailURL, 30); err == nil {
		t.Fatal("expected failure on invalid email format")
	}
}

func TestValidateTokenURL(t *testing.T) {
	path := "/m365Copilot/Chathub/11111111-2222-3333-4444-555555555555@66666666-7777-8888-9999-000000000000"
	future := time.Now().Add(time.Hour).Unix()
	valid := testTokenURL("substrate.office.com", path, future)
	if _, err := ValidateTokenURL(valid, 30); err != nil {
		t.Fatal(err)
	}
	valid2 := testTokenURL("substrate.svc.cloud.microsoft", path, future)
	if _, err := ValidateTokenURL(valid2, 30); err != nil {
		t.Fatal(err)
	}
	invalid := []string{
		strings.Replace(valid, "wss://", "ws://", 1),
		strings.Replace(valid, "substrate.office.com", "substrate.office.com.evil.test", 1),
		strings.Replace(valid, "substrate.office.com", "substrate.office.com:8443", 1),
		strings.Replace(valid, path, "/m365Copilot/Chathub", 1),
		strings.Replace(valid, "wss://", "wss://user@", 1),
		valid + "#fragment",
		valid + "&access_token=duplicate",
		valid + "\n",
		testTokenURL("substrate.office.com", path, time.Now().Add(-time.Hour).Unix()),
	}
	for _, candidate := range invalid {
		if _, err := ValidateTokenURL(candidate, 30); err == nil {
			t.Fatalf("accepted invalid Copilot URL of length %d", len(candidate))
		}
	}
}
