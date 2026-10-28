package accounting

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var decimalAmountPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)

const (
	StateQueued            = "queued"
	StateProcessing        = "processing"
	StatePendingSubmission = "pending_submission"
	StateNeedsConfirmation = "needs_confirmation"
	StateCommitting        = "committing"
	StateCommitted         = "committed"
	StateRejected          = "rejected"
	StateUncertain         = "uncertain"
)

type Draft struct {
	Type         string   `json:"type"`
	Date         string   `json:"date"`
	Amount       string   `json:"amount"`
	CategoryName string   `json:"categoryName"`
	AccountName  string   `json:"accountName"`
	StoreName    string   `json:"storeName"`
	ProjectName  string   `json:"projectName"`
	MemberName   string   `json:"memberName"`
	Memo         string   `json:"memo"`
	Confidence   float64  `json:"confidence"`
	NeedsConfirm bool     `json:"needsConfirmation"`
	Warnings     []string `json:"warnings,omitempty"`
}

func EncodeDraft(d Draft) (string, error) { b, err := json.Marshal(d); return string(b), err }

func DecodeDraft(raw string) (Draft, error) {
	var d Draft
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return Draft{}, err
	}
	return d, nil
}

func Validate(d Draft, location *time.Location) error {
	if d.Type != "expense" && d.Type != "income" {
		return errors.New("type must be expense or income")
	}
	amountText := strings.TrimSpace(d.Amount)
	if !decimalAmountPattern.MatchString(amountText) {
		return errors.New("amount must be a decimal number")
	}
	amount, err := strconv.ParseFloat(amountText, 64)
	if err != nil || amount <= 0 {
		return errors.New("amount must be greater than zero")
	}
	if strings.Contains(amountText, ".") && len(strings.SplitN(amountText, ".", 2)[1]) > 2 {
		return errors.New("amount supports at most two decimals")
	}
	if _, err := time.ParseInLocation("2006-01-02 15:04", d.Date, location); err != nil {
		return fmt.Errorf("invalid date: %w", err)
	}
	return nil
}

func MissingConfirmation(d Draft) []string {
	missing := make([]string, 0, 4)
	if d.Date == "" {
		missing = append(missing, "date")
	}
	if d.Amount == "" {
		missing = append(missing, "amount")
	}
	if d.CategoryName == "" {
		missing = append(missing, "category")
	}
	if d.AccountName == "" {
		missing = append(missing, "account")
	}
	return missing
}
