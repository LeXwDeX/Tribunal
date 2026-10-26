package errclass

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type codedError struct{ code string }

func (e codedError) Error() string     { return "token=SECRET " + e.code }
func (e codedError) ErrorCode() string { return e.code }

func TestOfReturnsFixedClassesWithoutErrorText(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"timeout", fmt.Errorf("query SECRET: %w", context.DeadlineExceeded), Timeout},
		{"canceled", fmt.Errorf("SECRET: %w", context.Canceled), Canceled},
		{"panic", ErrPanic, Panic},
		{"coded", fmt.Errorf("wrap SECRET: %w", codedError{code: "gitlab_rejected"}), "gitlab_rejected"},
		{"unsafe code", codedError{code: "Bad Code SECRET"}, Internal},
		{"plain", errors.New("payload=RAW-BODY token=SECRET"), Internal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Of(tc.err)
			if got != tc.want {
				t.Fatalf("Of = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "SECRET") || strings.Contains(got, "RAW") {
				t.Fatalf("class leaked error text: %q", got)
			}
		})
	}
}
