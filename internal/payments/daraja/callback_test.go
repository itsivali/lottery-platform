package daraja_test

import (
	"errors"
	"testing"

	"github.com/itsivali/lottery-platform/internal/payments/daraja"
)

const paidCallback = `{"Body":{"stkCallback":{"MerchantRequestID":"m-1","CheckoutRequestID":"ws_CO_1","ResultCode":0,
"ResultDesc":"The service request is processed successfully.","CallbackMetadata":{"Item":[
{"Name":"Amount","Value":150.00},{"Name":"MpesaReceiptNumber","Value":"TJK1AB2CD3"},{"Name":"Balance"},
{"Name":"TransactionDate","Value":20261022093112},{"Name":"PhoneNumber","Value":254712345678}]}}}}`

func TestParseSTKCallback_ReadsAPaidCallback(t *testing.T) {
	cb, err := daraja.ParseSTKCallback([]byte(paidCallback))
	if err != nil {
		t.Fatal(err)
	}
	want := daraja.STKCallback{
		MerchantRequestID: "m-1", CheckoutRequestID: "ws_CO_1", ResultCode: 0,
		ResultDesc: "The service request is processed successfully.",
		AmountKES:  150, Receipt: "TJK1AB2CD3", MSISDN: "254712345678",
	}
	if cb != want {
		t.Fatalf("cb = %+v\nwant %+v", cb, want)
	}
	if !cb.Paid() {
		t.Fatal("not paid")
	}
}

func TestParseSTKCallback_AFailedCallbackHasNoMetadata(t *testing.T) {
	cb, err := daraja.ParseSTKCallback([]byte(`{"Body":{"stkCallback":{"MerchantRequestID":"m","CheckoutRequestID":"c","ResultCode":1032,"ResultDesc":"Request cancelled by user"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cb.Paid() || cb.ResultCode != 1032 || cb.CheckoutRequestID != "c" || cb.AmountKES != 0 {
		t.Fatalf("cb = %+v", cb)
	}
}

func TestParseSTKCallback_AMaskedOrMissingPhoneIsLeftEmpty(t *testing.T) {
	body := `{"Body":{"stkCallback":{"MerchantRequestID":"m","CheckoutRequestID":"c","ResultCode":0,"ResultDesc":"ok","CallbackMetadata":{"Item":[
{"Name":"Amount","Value":10},{"Name":"MpesaReceiptNumber","Value":"R1"},{"Name":"PhoneNumber","Value":"2547 ***** 678"}]}}}}`
	cb, err := daraja.ParseSTKCallback([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if cb.MSISDN != "" || !cb.Paid() {
		t.Fatalf("cb = %+v", cb)
	}
}

func TestParseSTKCallback_RefusesMalformedOrInconsistentBodies(t *testing.T) {
	for name, body := range map[string]string{
		"not json":           `<xml/>`,
		"no checkout id":     `{"Body":{"stkCallback":{"ResultCode":0}}}`,
		"no result code":     `{"Body":{"stkCallback":{"CheckoutRequestID":"c"}}}`,
		"paid without items": `{"Body":{"stkCallback":{"CheckoutRequestID":"c","ResultCode":0}}}`,
		"paid no receipt":    `{"Body":{"stkCallback":{"CheckoutRequestID":"c","ResultCode":0,"CallbackMetadata":{"Item":[{"Name":"Amount","Value":10}]}}}}`,
		"paid no amount":     `{"Body":{"stkCallback":{"CheckoutRequestID":"c","ResultCode":0,"CallbackMetadata":{"Item":[{"Name":"MpesaReceiptNumber","Value":"R"}]}}}}`,
		"fractional amount":  `{"Body":{"stkCallback":{"CheckoutRequestID":"c","ResultCode":0,"CallbackMetadata":{"Item":[{"Name":"Amount","Value":10.5},{"Name":"MpesaReceiptNumber","Value":"R"}]}}}}`,
		"zero amount":        `{"Body":{"stkCallback":{"CheckoutRequestID":"c","ResultCode":0,"CallbackMetadata":{"Item":[{"Name":"Amount","Value":0},{"Name":"MpesaReceiptNumber","Value":"R"}]}}}}`,
		"string amount":      `{"Body":{"stkCallback":{"CheckoutRequestID":"c","ResultCode":0,"CallbackMetadata":{"Item":[{"Name":"Amount","Value":"10"},{"Name":"MpesaReceiptNumber","Value":"R"}]}}}}`,
	} {
		if _, err := daraja.ParseSTKCallback([]byte(body)); !errors.Is(err, daraja.ErrBadCallback) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
