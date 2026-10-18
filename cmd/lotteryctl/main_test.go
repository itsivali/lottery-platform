package main

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		args    []string
		want    string
		wantErr bool
	}{
		{[]string{"migrate", "up"}, "migrate up", false},
		{[]string{"migrate", "down"}, "migrate down", false},
		{[]string{"migrate"}, "", true},
		{[]string{"migrate", "sideways"}, "", true},
		{[]string{"bogus"}, "", true},
		{nil, "", true},
	}
	for _, tc := range tests {
		got, err := parse(tc.args)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("parse(%v) = %q, %v; want %q, err=%v", tc.args, got, err, tc.want, tc.wantErr)
		}
	}
}
