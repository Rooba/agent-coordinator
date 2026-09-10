package taskid

import "testing"

func TestValid(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		value string
		want  bool
	}{
		"valid":           {value: "task-0123456789ab", want: true},
		"empty":           {},
		"wrong prefix":    {value: "job--0123456789ab"},
		"too short":       {value: "task-0123456789a"},
		"too long":        {value: "task-0123456789abc"},
		"uppercase hex":   {value: "task-0123456789AB"},
		"non-hex":         {value: "task-0123456789ag"},
		"multi-byte rune": {value: "task-0123456789aé"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := Valid(test.value); got != test.want {
				t.Fatalf("Valid(%q) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}
