package mountmanager

import (
	"reflect"
	"testing"
)

func TestWinFspCaseSensitivityArgs(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{name: "binary default", want: nil},
		{name: "case sensitive", value: "true", want: []string{"-winfspCaseSensitive=true"}},
		{name: "case insensitive", value: "false", want: []string{"-winfspCaseSensitive=false"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := winFspCaseSensitivityArgs(func(string) string { return tt.value })
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("args = %v, want %v", got, tt.want)
			}
		})
	}
}
