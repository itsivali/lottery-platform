package daraja

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
)

// STKCallback is the result Daraja posts to the callback URL. It is a claim, not proof: anyone who learns the URL
// can post one, so callers confirm it with STKQuery before moving money and take the amount only from it.
type STKCallback struct {
	MerchantRequestID string
	CheckoutRequestID string
	ResultCode        int
	ResultDesc        string
	// Set only on a paid callback.
	AmountKES int64
	Receipt   string
	// MSISDN is the payer as Daraja reports it; empty when absent or masked (Safaricom masks numbers on some
	// products). The push target, which is the phone that approved the prompt, is the number to rely on.
	MSISDN string
}

// Paid reports whether the callback claims a completed payment.
func (c STKCallback) Paid() bool { return c.ResultCode == 0 }

// ParseSTKCallback reads and checks a callback body. A paid callback must carry a positive whole amount and a receipt.
func ParseSTKCallback(body []byte) (STKCallback, error) {
	var env struct {
		Body struct {
			STK struct {
				MerchantRequestID string          `json:"MerchantRequestID"`
				CheckoutRequestID string          `json:"CheckoutRequestID"`
				ResultCode        json.RawMessage `json:"ResultCode"`
				ResultDesc        string          `json:"ResultDesc"`
				Metadata          *struct {
					Items []struct {
						Name  string          `json:"Name"`
						Value json.RawMessage `json:"Value"`
					} `json:"Item"`
				} `json:"CallbackMetadata"`
			} `json:"stkCallback"`
		} `json:"Body"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&env); err != nil {
		return STKCallback{}, fmt.Errorf("%w: not json", ErrBadCallback)
	}
	s := env.Body.STK
	code, ok := resultCode(s.ResultCode)
	if s.CheckoutRequestID == "" || !ok {
		return STKCallback{}, fmt.Errorf("%w: missing checkout id or result code", ErrBadCallback)
	}
	cb := STKCallback{MerchantRequestID: s.MerchantRequestID, CheckoutRequestID: s.CheckoutRequestID, ResultCode: code, ResultDesc: s.ResultDesc}
	if !cb.Paid() {
		return cb, nil
	}
	if s.Metadata == nil {
		return STKCallback{}, fmt.Errorf("%w: paid without metadata", ErrBadCallback)
	}
	for _, it := range s.Metadata.Items {
		switch it.Name {
		case "Amount":
			var f float64
			if json.Unmarshal(it.Value, &f) != nil || f != math.Trunc(f) || f <= 0 || f > MaxSTKAmountKES {
				return STKCallback{}, fmt.Errorf("%w: amount is not a positive whole number of shillings", ErrBadCallback)
			}
			cb.AmountKES = int64(f)
		case "MpesaReceiptNumber":
			_ = json.Unmarshal(it.Value, &cb.Receipt)
		case "PhoneNumber":
			var n json.Number
			if json.Unmarshal(it.Value, &n) == nil && msisdnRE.MatchString(n.String()) {
				cb.MSISDN = n.String()
			}
		}
	}
	if cb.AmountKES == 0 || cb.Receipt == "" {
		return STKCallback{}, fmt.Errorf("%w: paid without amount or receipt", ErrBadCallback)
	}
	return cb, nil
}
