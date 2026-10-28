package accounting

import (
	"testing"
	"time"
)

func TestValidationAcceptsStructuredDraft(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	draft := Draft{Type: "expense", Date: now.Format("2006-01-02 15:04"), Amount: "35"}
	if err := Validate(draft, now.Location()); err != nil {
		t.Fatalf("expected valid draft: %v", err)
	}
}

func TestValidationRejectsUnsafeAmount(t *testing.T) {
	now := time.Now()
	draft := Draft{Type: "expense", Date: now.Format("2006-01-02 15:04"), Amount: "1.234"}
	if err := Validate(draft, now.Location()); err == nil {
		t.Fatal("expected amount precision error")
	}
}

func TestValidationRejectsNonDecimalAmounts(t *testing.T) {
	now := time.Now()
	for _, amount := range []string{"NaN", "Inf", "1e2", "12abc"} {
		draft := Draft{Type: "expense", Date: now.Format("2006-01-02 15:04"), Amount: amount}
		if err := Validate(draft, now.Location()); err == nil {
			t.Fatalf("expected decimal validation error for %q", amount)
		}
	}
}

func TestMissingConfirmation(t *testing.T) {
	missing := MissingConfirmation(Draft{Amount: "10"})
	if len(missing) != 3 || missing[0] != "date" || missing[1] != "category" || missing[2] != "account" {
		t.Fatalf("unexpected missing fields: %#v", missing)
	}
}
