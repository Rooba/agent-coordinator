package hostrunner

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxTaskIDBytes   = 17
	MaxProviderBytes = 64
	MaxBriefBytes    = 64 << 10
	MinTaskTimeout   = 5 * time.Minute
	MaxTaskTimeout   = 30 * time.Minute
	MaxReportBytes   = 1 << 20

	maxSummaryBytes = 16 << 10
	maxReportItems  = 64
	maxItemBytes    = 8 << 10
)

var (
	ErrInvalidTask   = errors.New("invalid host task")
	ErrInvalidReport = errors.New("invalid host report")
)

// Task is the execution-only projection of the frozen task.launch wire body:
// ID <- task_id, Provider <- runtime, and Timeout <- deadline_s. The broker
// retains scope and reply_to; they must never enter a provider invocation.
type Task struct {
	ID       string
	Provider string
	Brief    string
	Timeout  time.Duration
}

func (t Task) Validate() error {
	switch {
	case !validTaskID(t.ID):
		return fmt.Errorf("%w: task_id must be task- plus 12 lowercase hex digits", ErrInvalidTask)
	case !validIdentifier(t.Provider, MaxProviderBytes):
		return fmt.Errorf("%w: unsafe runtime", ErrInvalidTask)
	case !validText(t.Brief, MaxBriefBytes) || strings.TrimSpace(t.Brief) == "":
		return fmt.Errorf("%w: brief must be non-empty UTF-8 of at most %d bytes", ErrInvalidTask, MaxBriefBytes)
	case t.Timeout != 0 && (t.Timeout < MinTaskTimeout || t.Timeout > MaxTaskTimeout || t.Timeout%time.Second != 0):
		return fmt.Errorf("%w: timeout must be zero or between %s and %s", ErrInvalidTask, MinTaskTimeout, MaxTaskTimeout)
	default:
		return nil
	}
}

func validTaskID(value string) bool {
	if len(value) != MaxTaskIDBytes || !strings.HasPrefix(value, "task-") {
		return false
	}
	for _, char := range value[len("task-"):] {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func validIdentifier(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for index, char := range value {
		alphanumeric := char >= 'a' && char <= 'z' || char >= '0' && char <= '9'
		if alphanumeric || index > 0 && char == '-' {
			continue
		}
		return false
	}
	return true
}

const (
	ReportSucceeded = "succeeded"
	ReportFailed    = "failed"
)

// Report is the schema-validated payload the broker wraps as task.result.
// Runner errors, cancellation, and timeouts instead become task.failed.
type Report struct {
	Status       string   `json:"status"`
	Summary      string   `json:"summary"`
	Observations []string `json:"observations"`
	Actions      []string `json:"actions"`
	Evidence     []string `json:"evidence"`
	Error        string   `json:"error,omitempty"`
}

func (r Report) Validate() error {
	if r.Status != ReportSucceeded && r.Status != ReportFailed {
		return fmt.Errorf("%w: unsupported status %q", ErrInvalidReport, r.Status)
	}
	if r.Status == ReportSucceeded && r.Error != "" || r.Status == ReportFailed && strings.TrimSpace(r.Error) == "" {
		return fmt.Errorf("%w: status and error disagree", ErrInvalidReport)
	}
	if !validText(r.Summary, maxSummaryBytes) || strings.TrimSpace(r.Summary) == "" || !validText(r.Error, maxSummaryBytes) {
		return fmt.Errorf("%w: invalid summary or error", ErrInvalidReport)
	}
	if !validItems(r.Observations) || !validItems(r.Actions) || !validItems(r.Evidence) {
		return fmt.Errorf("%w: invalid report list", ErrInvalidReport)
	}
	return nil
}

func validItems(values []string) bool {
	if values == nil || len(values) > maxReportItems {
		return false
	}
	for _, value := range values {
		if !validText(value, maxItemBytes) || strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}

func validText(value string, max int) bool {
	return len(value) <= max && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

const reportSchema = `{"type":"object","properties":{"status":{"type":"string","enum":["succeeded","failed"]},"summary":{"type":"string","maxLength":16384},"observations":{"type":"array","maxItems":64,"items":{"type":"string","minLength":1,"maxLength":8192}},"actions":{"type":"array","maxItems":64,"items":{"type":"string","minLength":1,"maxLength":8192}},"evidence":{"type":"array","maxItems":64,"items":{"type":"string","minLength":1,"maxLength":8192}},"error":{"type":"string","maxLength":16384}},"required":["status","summary","observations","actions","evidence"],"additionalProperties":false}`
