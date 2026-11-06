package output

import (
	"encoding/json"
	"strconv"
	"time"
)

// The API leaves optional fields out or null; a table shows them as "-".

func Text(value *string) string {
	if value == nil || *value == "" {
		return "-"
	}
	return *value
}

func Int(value *int32) string {
	if value == nil {
		return "-"
	}
	return strconv.Itoa(int(*value))
}

// Number keeps every digit the API sent (money, prices): it is never parsed into a float.
func Number(value *json.Number) string {
	if value == nil {
		return "-"
	}
	return value.String()
}

func Time(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return value.Format(time.RFC3339)
}

// Word is an enum value of the API (a status, a state) as text.
func Word[T ~string](value *T) string {
	if value == nil {
		return "-"
	}
	return string(*value)
}
