package payments

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"time"
)

// webhookPayload is the body the mock PSP posts back to our webhook endpoint and
// that the endpoint parses. Fields are the SPEC contract: psp_ref + status.
type webhookPayload struct {
	PSPRef string `json:"psp_ref"`
	Status string `json:"status"` // "success" | "failure"
}

// Webhook status literals (the PSP callback vocabulary, distinct from the
// PENDING/PROCESSING/... payment statuses).
const (
	pspSuccess = "success"
	pspFailure = "failure"
)

const (
	// jitterMinMs..jitterMaxMs is the async confirmation delay window (SPEC:
	// 300–800ms).
	jitterMinMs = 300
	jitterMaxMs = 800
	// successPercent is the mock approval rate (SPEC: 90%).
	successPercent = 90
)

// PSP is the in-process mock payment service provider. Trigger hands it a
// psp_ref + amount; after a short jitter it POSTs a signed confirmation webhook
// back to our own endpoint, simulating a real provider's async callback.
type PSP struct {
	webhookURL string
	secret     string
	client     *http.Client
	log        *slog.Logger
	rng        *rand.Rand
}

// NewPSP constructs the mock PSP. The webhook URL and signing secret come from
// config (GORIDE_PSP_WEBHOOK_URL / GORIDE_PSP_SECRET).
func NewPSP(webhookURL, secret string, log *slog.Logger) *PSP {
	return &PSP{
		webhookURL: webhookURL,
		secret:     secret,
		client:     &http.Client{Timeout: 5 * time.Second},
		log:        log,
		rng:        rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Schedule simulates the provider processing a charge: after a 300–800ms jitter
// it decides the outcome and POSTs a signed webhook. It returns immediately; the
// callback runs on a detached goroutine so the triggering request is not
// blocked.
func (p *PSP) Schedule(pspRef string, amount int) {
	delay := time.Duration(jitterMinMs+p.rng.Intn(jitterMaxMs-jitterMinMs+1)) * time.Millisecond
	roll := p.rng.Intn(100)
	go func() {
		time.Sleep(delay)
		status := pspOutcome(amount, roll)
		if err := p.postWebhook(pspRef, status); err != nil {
			p.log.Warn("psp: post webhook failed", "error", err, "psp_ref", pspRef)
		}
	}()
}

// postWebhook signs and POSTs the confirmation body to the configured endpoint.
func (p *PSP) postWebhook(pspRef, status string) error {
	body, err := json.Marshal(webhookPayload{PSPRef: pspRef, Status: status})
	if err != nil {
		return fmt.Errorf("psp: marshal webhook: %w", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, p.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("psp: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-PSP-Signature", Sign(p.secret, body))

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("psp: do request: %w", err)
	}
	_ = resp.Body.Close()
	return nil
}

// ---- pure helpers (unit-tested) ----

// pspOutcome decides a charge's outcome deterministically for tests and
// pseudo-randomly otherwise. Test hook: any amount whose last two paise digits
// are 13 (amount % 100 == 13) always fails, giving tests a reliable failure
// path. Otherwise a 0–99 roll below successPercent (90) succeeds.
func pspOutcome(amount, roll int) string {
	if amount%100 == 13 {
		return pspFailure
	}
	if roll < successPercent {
		return pspSuccess
	}
	return pspFailure
}

// Sign returns the hex-encoded HMAC-SHA256 of body under secret — the value the
// PSP puts in X-PSP-Signature.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether sig (hex HMAC-SHA256) authenticates body under secret,
// using a constant-time comparison to avoid timing leaks.
func Verify(secret string, body []byte, sig string) bool {
	expected := Sign(secret, body)
	return hmac.Equal([]byte(expected), []byte(sig))
}
