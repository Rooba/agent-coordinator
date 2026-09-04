package hostrunner

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTaskValidate(t *testing.T) {
	valid := Task{ID: "task-0123456789ab", Provider: "codex", Brief: "Inspect the page.", Timeout: MinTaskTimeout}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid task: %v", err)
	}

	tests := map[string]Task{
		"empty id":           {Provider: "codex", Brief: "brief"},
		"wrong id shape":     {ID: "task-123", Provider: "codex", Brief: "brief"},
		"uppercase id":       {ID: "task-0123456789AB", Provider: "codex", Brief: "brief"},
		"uppercase provider": {ID: valid.ID, Provider: "Codex", Brief: "brief"},
		"empty brief":        {ID: valid.ID, Provider: "codex", Brief: " \n"},
		"nul brief":          {ID: valid.ID, Provider: "codex", Brief: "bad\x00brief"},
		"large brief":        {ID: valid.ID, Provider: "codex", Brief: strings.Repeat("x", MaxBriefBytes+1)},
		"short timeout":      {ID: valid.ID, Provider: "codex", Brief: "brief", Timeout: MinTaskTimeout - time.Second},
		"fractional timeout": {ID: valid.ID, Provider: "codex", Brief: "brief", Timeout: MinTaskTimeout + time.Nanosecond},
		"large timeout":      {ID: valid.ID, Provider: "codex", Brief: "brief", Timeout: MaxTaskTimeout + time.Second},
	}
	for name, task := range tests {
		t.Run(name, func(t *testing.T) {
			if err := task.Validate(); !errors.Is(err, ErrInvalidTask) {
				t.Fatalf("got %v, want ErrInvalidTask", err)
			}
		})
	}
}

func TestReportValidate(t *testing.T) {
	valid := successReport()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid report: %v", err)
	}
	if encoded, _ := json.Marshal(valid); strings.Contains(string(encoded), `"error"`) {
		t.Fatalf("successful report retained an empty error: %s", encoded)
	}

	tests := map[string]Report{
		"status":        {Status: "maybe", Summary: "summary", Observations: []string{}, Actions: []string{}, Evidence: []string{}},
		"cancelled":     {Status: "cancelled", Summary: "cancelled", Observations: []string{}, Actions: []string{}, Evidence: []string{}, Error: "cancelled"},
		"timed out":     {Status: "timed_out", Summary: "timed out", Observations: []string{}, Actions: []string{}, Evidence: []string{}, Error: "timed out"},
		"success error": {Status: ReportSucceeded, Summary: "summary", Observations: []string{}, Actions: []string{}, Evidence: []string{}, Error: "bad"},
		"failure error": {Status: ReportFailed, Summary: "summary", Observations: []string{}, Actions: []string{}, Evidence: []string{}},
		"summary":       {Status: ReportSucceeded, Observations: []string{}, Actions: []string{}, Evidence: []string{}},
		"missing list":  {Status: ReportSucceeded, Summary: "summary", Actions: []string{}, Evidence: []string{}},
		"empty item":    {Status: ReportSucceeded, Summary: "summary", Observations: []string{""}, Actions: []string{}, Evidence: []string{}},
	}
	for name, report := range tests {
		t.Run(name, func(t *testing.T) {
			if err := report.Validate(); !errors.Is(err, ErrInvalidReport) {
				t.Fatalf("got %v, want ErrInvalidReport", err)
			}
		})
	}
}

func successReport() Report {
	return Report{
		Status:       ReportSucceeded,
		Summary:      "page loaded",
		Observations: []string{"title was visible"},
		Actions:      []string{"opened the page"},
		Evidence:     []string{"https://example.test"},
		Error:        "",
	}
}
