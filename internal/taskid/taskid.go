// Package taskid validates identifiers for host eyes tasks.
package taskid

const (
	prefix    = "task-"
	hexDigits = 12
	length    = len(prefix) + hexDigits
)

// Valid reports whether value is "task-" followed by 12 lowercase hex digits.
func Valid(value string) bool {
	if len(value) != length || value[:len(prefix)] != prefix {
		return false
	}
	for i := len(prefix); i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
