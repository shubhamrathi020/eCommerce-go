// Package domain is the Go port of the shared @ecom/contracts package: the data shapes both halves exchange and
// the pure business rules (pricing, coupons, serviceability, order transitions, permissions) that the browser and
// the server must agree on. Anything here must give exactly the same answer as the TypeScript original.
package domain

import (
	"strconv"
	"strings"
)

// Money is always integer minor units (paise for INR); never a float.
type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// INR builds a rupee amount from paise, rounding to a whole paisa.
func INR(amount float64) Money { return Money{Amount: jsRound(amount), Currency: "INR"} }

// INRInt builds a rupee amount from whole paise.
func INRInt(amount int64) Money { return Money{Amount: amount, Currency: "INR"} }

// jsRound mirrors JavaScript's Math.round: ties round toward +Infinity (Go's math.Round rounds them away from zero).
func jsRound(x float64) int64 {
	f := int64(x)
	if float64(f) > x {
		f--
	}
	if x-float64(f) >= 0.5 {
		return f + 1
	}
	return f
}

// FormatMoney formats paise as rupees the way Intl.NumberFormat('en-IN', INR) does: "₹1,299" for whole rupees
// and "₹1,299.50" otherwise, grouping the first three digits and then pairs (lakh style: ₹12,34,567).
func FormatMoney(m Money) string {
	neg := m.Amount < 0
	abs := m.Amount
	if neg {
		abs = -abs
	}
	rupees := abs / 100
	paise := abs % 100
	out := "₹" + groupIndian(strconv.FormatInt(rupees, 10))
	if paise != 0 {
		out += "." + leftPad(strconv.FormatInt(paise, 10), 2)
	}
	if neg {
		out = "-" + out
	}
	return out
}

func leftPad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return strings.Repeat("0", n-len(s)) + s
}

func groupIndian(digits string) string {
	if len(digits) <= 3 {
		return digits
	}
	head, tail := digits[:len(digits)-3], digits[len(digits)-3:]
	var parts []string
	for len(head) > 2 {
		parts = append([]string{head[len(head)-2:]}, parts...)
		head = head[:len(head)-2]
	}
	if head != "" {
		parts = append([]string{head}, parts...)
	}
	return strings.Join(append(parts, tail), ",")
}
