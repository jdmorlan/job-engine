package ca_test

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jdmorlan/job-engine/internal/ca"
)

// A clock that is wrong surfaces as a certificate that looks expired, which
// sends people to look at the certificate. Saying so at enrolment costs a
// sentence; not saying it costs somebody an afternoon (D25).
func TestClockSkewIsRefusedWithBothTimes(t *testing.T) {
	far := time.Now().Add(-90 * time.Minute).UTC().Format(http.TimeFormat)

	err := ca.CheckClockSkew(far)
	if err == nil {
		t.Fatal("an hour and a half of skew was accepted")
	}
	for _, want := range []string{"clock", "here", "there"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestASmallSkewIsToleratedAndNoDateIsNotAnError(t *testing.T) {
	// Well inside the window: machines are never exactly in step, and refusing
	// over a second would make this unusable.
	if err := ca.CheckClockSkew(time.Now().Add(-30 * time.Second).UTC().Format(http.TimeFormat)); err != nil {
		t.Errorf("half a minute of skew was refused: %v", err)
	}
	// A server that sends no Date, or an unparseable one, is not a clock
	// problem and must not be reported as one.
	if err := ca.CheckClockSkew(""); err != nil {
		t.Errorf("a missing Date header was treated as skew: %v", err)
	}
	if err := ca.CheckClockSkew("not a date"); err != nil {
		t.Errorf("an unparseable Date header was treated as skew: %v", err)
	}
}

// Go's own message is true and unhelpful: when both ends were issued minutes
// ago by the same authority, the certificate is fine and the clocks are not.
func TestAnExpiryFailureSuggestsTheClocks(t *testing.T) {
	expired := x509.CertificateInvalidError{Reason: x509.Expired, Detail: "current time is after"}

	got := ca.ExplainHandshake(fmt.Errorf("dialing: %w", expired))
	if !strings.Contains(got.Error(), "clocks") {
		t.Errorf("an expiry failure did not mention the clocks: %v", got)
	}
	// The original is still there for anybody who wants it.
	var invalid x509.CertificateInvalidError
	if !errors.As(got, &invalid) {
		t.Error("the underlying certificate error was swallowed")
	}
}

// Everything else is passed through untouched. A connection refused is not a
// clock problem, and saying so would be noise on the most common failure there
// is.
func TestOtherErrorsArePassedThrough(t *testing.T) {
	plain := errors.New("connection refused")
	if got := ca.ExplainHandshake(plain); got != plain {
		t.Errorf("ExplainHandshake changed an unrelated error: %v", got)
	}
	if ca.ExplainHandshake(nil) != nil {
		t.Error("ExplainHandshake invented an error from nil")
	}

	// A certificate that is invalid for another reason is a real certificate
	// problem and must not be blamed on the clock.
	wrongHost := x509.CertificateInvalidError{Reason: x509.CANotAuthorizedForThisName}
	if strings.Contains(ca.ExplainHandshake(wrongHost).Error(), "clocks") {
		t.Error("a name mismatch was blamed on the clocks")
	}
}
