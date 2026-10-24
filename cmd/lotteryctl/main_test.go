package main

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		args    []string
		want    command
		wantErr bool
	}{
		{[]string{"migrate", "up"}, command{name: "migrate up"}, false},
		{[]string{"migrate", "down"}, command{name: "migrate down"}, false},
		{[]string{"migrate"}, command{}, true},
		{[]string{"migrate", "sideways"}, command{}, true},
		{[]string{"payments", "reconcile"}, command{name: "payments reconcile"}, false},
		{[]string{"payments", "reverse", "-receipt", "TJK1AB2CD3", "-reason", "dispute upheld", "-operator", "willis"},
			command{name: "payments reverse", receipt: "TJK1AB2CD3", reason: "dispute upheld", operator: "operator:willis"}, false},
		{[]string{"payments", "reverse", "-receipt", "R", "-operator", "willis"}, command{}, true}, // no reason
		{[]string{"payments", "reverse", "-receipt", "R", "-reason", "x"}, command{}, true},        // no operator
		{[]string{"payments", "reverse", "-reason", "x", "-operator", "willis"}, command{}, true},  // no receipt
		{[]string{"payments", "reverse", "-receipt", "R", "-reason", "x", "-operator", "w", "extra"}, command{}, true},
		{[]string{"payments", "resolve", "-payment", "0192f000-0000-7000-8000-000000000001", "-receipt", "TJK1", "-amount", "150", "-operator", "willis"},
			command{name: "payments resolve", payment: "0192f000-0000-7000-8000-000000000001", receipt: "TJK1", amount: 150, operator: "operator:willis"}, false},
		{[]string{"payments", "resolve", "-payment", "p", "-receipt", "R", "-operator", "w"}, command{}, true},                   // no amount
		{[]string{"payments", "resolve", "-payment", "p", "-receipt", "R", "-amount", "abc", "-operator", "w"}, command{}, true}, // bad amount
		{[]string{"payments", "refund"}, command{}, true},
		{[]string{"payments", "suspense-credit", "-receipt", "MCC0000001", "-user", "u-1", "-operator", "willis"},
			command{name: "payments suspense-credit", receipt: "MCC0000001", user: "u-1", operator: "operator:willis"}, false},
		{[]string{"payments", "suspense-credit", "-receipt", "R", "-operator", "w"}, command{}, true}, // no user
		{[]string{"payments", "suspense-refund", "-receipt", "R", "-refund-ref", "TJK9", "-operator", "w"},
			command{name: "payments suspense-refund", receipt: "R", refundRef: "TJK9", operator: "operator:w"}, false},
		{[]string{"payments", "suspense-refund", "-receipt", "R", "-operator", "w"}, command{}, true}, // no refund reference
		{[]string{"payments", "c2b-recheck", "-receipt", "R", "-operator", "w"}, command{name: "payments c2b-recheck", receipt: "R", operator: "operator:w"}, false},
		{[]string{"payments", "c2b-recheck", "-receipt", "R", "-operator", "w", "-amount", "5"}, command{}, true}, // flag it does not take
		{[]string{"payments", "c2b-register"}, command{name: "payments c2b-register"}, false},
		{[]string{"payments", "c2b-register", "-receipt", "R"}, command{}, true},
		{[]string{"payments", "c2b-simulate", "-amount", "50", "-msisdn", "254708374149", "-account", "0712345678"},
			command{name: "payments c2b-simulate", amount: 50, msisdn: "254708374149", account: "0712345678"}, false},
		{[]string{"payments", "c2b-simulate", "-amount", "-5", "-msisdn", "254708374149", "-account", "x"}, command{}, true},
		{[]string{"payments", "reconcile", "-operator", "w"}, command{}, true},
		{[]string{"bogus"}, command{}, true},
		{nil, command{}, true},
	}
	for _, tc := range tests {
		got, err := parse(tc.args)
		if (err != nil) != tc.wantErr || (err == nil && !reflect.DeepEqual(got, tc.want)) {
			t.Errorf("parse(%v) = %+v, %v; want %+v, err=%v", tc.args, got, err, tc.want, tc.wantErr)
		}
	}
}
