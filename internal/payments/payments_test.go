package payments

import "testing"

// TestSignVerifyRoundTrip checks that a signature produced by Sign verifies, and
// that any tampering with the body or the signature is rejected.
func TestSignVerifyRoundTrip(t *testing.T) {
	const secret = "test-secret"
	body := []byte(`{"psp_ref":"abc-123","status":"success"}`)

	sig := Sign(secret, body)
	if !Verify(secret, body, sig) {
		t.Fatal("valid signature failed to verify")
	}

	// Tampered body.
	tampered := []byte(`{"psp_ref":"abc-123","status":"failure"}`)
	if Verify(secret, tampered, sig) {
		t.Fatal("signature verified against a tampered body")
	}

	// Tampered signature.
	if Verify(secret, body, sig+"00") {
		t.Fatal("tampered signature verified")
	}
	if len(sig) > 0 && Verify(secret, body, "deadbeef") {
		t.Fatal("bogus signature verified")
	}

	// Wrong secret.
	if Verify("other-secret", body, sig) {
		t.Fatal("signature verified under the wrong secret")
	}
}

// TestShouldApplyWebhook is the webhook idempotency decision: only a PROCESSING
// payment is actionable; every terminal (or not-yet-processing) state is a
// no-op replay.
func TestShouldApplyWebhook(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{StatusProcessing, true},
		{StatusSucceeded, false},
		{StatusFailed, false},
		{StatusPending, false},
	}
	for _, tc := range tests {
		if got := shouldApplyWebhook(tc.status); got != tc.want {
			t.Errorf("shouldApplyWebhook(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// TestCanTrigger is the retry-count gating: PENDING is always payable; FAILED is
// payable only while retry_count is below the cap; PROCESSING/SUCCEEDED never.
func TestCanTrigger(t *testing.T) {
	tests := []struct {
		status     string
		retryCount int
		want       bool
	}{
		{StatusPending, 0, true},
		{StatusFailed, 0, true},
		{StatusFailed, 2, true},
		{StatusFailed, 3, false}, // cap reached
		{StatusFailed, 4, false},
		{StatusProcessing, 0, false},
		{StatusSucceeded, 0, false},
	}
	for _, tc := range tests {
		if got := canTrigger(tc.status, tc.retryCount); got != tc.want {
			t.Errorf("canTrigger(%q, %d) = %v, want %v", tc.status, tc.retryCount, got, tc.want)
		}
	}
}

// TestPSPOutcome checks the deterministic failure hook (amount ending in 13
// always fails) and the roll-based 90% success split.
func TestPSPOutcome(t *testing.T) {
	// Test hook: amount % 100 == 13 always fails, regardless of roll.
	if got := pspOutcome(11300+13, 0); got != pspFailure { // 11313 -> %100 == 13
		t.Errorf("amount ending in 13 with roll 0 = %q, want failure", got)
	}
	if got := pspOutcome(1013, 99); got != pspFailure {
		t.Errorf("amount ending in 13 with roll 99 = %q, want failure", got)
	}

	// Non-13 amounts: roll < 90 succeeds, roll >= 90 fails.
	if got := pspOutcome(20000, 0); got != pspSuccess {
		t.Errorf("roll 0 = %q, want success", got)
	}
	if got := pspOutcome(20000, 89); got != pspSuccess {
		t.Errorf("roll 89 = %q, want success", got)
	}
	if got := pspOutcome(20000, 90); got != pspFailure {
		t.Errorf("roll 90 = %q, want failure", got)
	}
	if got := pspOutcome(20000, 99); got != pspFailure {
		t.Errorf("roll 99 = %q, want failure", got)
	}
}
