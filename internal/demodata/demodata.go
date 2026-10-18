// Package demodata describes the fictional payment gateways, merchants and customers that
// cmd/seed writes as history and cmd/traffic drives live, so both tell the same story.
package demodata

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// Merchant is a seeded merchant: Median is the typical ticket in rupees.
type Merchant struct {
	ID, Name, MCC string
	Median        float64
	Fixed         []float64 // subscription-style fixed prices, if any
}

// Tenant is a seeded payment gateway. Weight is its share of traffic.
type Tenant struct {
	ID, Name, Prefix string
	FeeBps           int
	Weight           float64
	Merchants        []Merchant
}

var (
	grocery     = Merchant{"freshbasket", "FreshBasket Grocery", "5411", 640, nil}
	tiffin      = Merchant{"tiffinbox", "TiffinBox Foods", "5812", 360, nil}
	volt        = Merchant{"voltmart", "VoltMart Electronics", "5732", 4200, nil}
	yatra       = Merchant{"yatrago", "YatraGo Travel", "4722", 3400, nil}
	streamly    = Merchant{"streamly", "Streamly OTT", "4899", 0, []float64{149, 199, 499, 1499}}
	pharma      = Merchant{"carepharma", "CarePharma", "5912", 520, nil}
	fuel        = Merchant{"fuelpoint", "FuelPoint", "5541", 900, nil}
	learn       = Merchant{"learnly", "Learnly Academy", "8299", 2600, nil}
	bills       = Merchant{"brightbills", "BrightBills Utilities", "4900", 1450, nil}
	threads     = Merchant{"urbanthreads", "UrbanThreads Fashion", "5651", 1800, nil}
	cafe        = Merchant{"cafecraft", "CafeCraft", "5814", 230, nil}
	playarena   = Merchant{"playarena", "PlayArena Games", "5816", 0, []float64{49, 99, 199, 399}}
	homenest    = Merchant{"homenest", "HomeNest Furnishing", "5712", 6800, nil}
	quickride   = Merchant{"quickride", "QuickRide Mobility", "4121", 180, nil}
	kiranaplus  = Merchant{"kiranaplus", "KiranaPlus", "5411", 420, nil}
	medicheck   = Merchant{"medicheck", "MediCheck Labs", "8071", 1250, nil}
	insuresmart = Merchant{"insuresmart", "InsureSmart", "6300", 5200, nil}
)

// Tenants are the fictional payment gateways used in demos.
var Tenants = []Tenant{
	{"pg_kestrel", "Kestrel Pay", "kestrel", 18, 0.32, []Merchant{grocery, tiffin, volt, streamly, quickride, cafe}},
	{"pg_bluefin", "Bluefin Payments", "bluefin", 20, 0.25, []Merchant{yatra, threads, homenest, insuresmart, kiranaplus}},
	{"pg_saffron", "Saffron Checkout", "saffron", 15, 0.19, []Merchant{pharma, medicheck, cafe, grocery, bills}},
	{"pg_orbit", "Orbit PG", "orbitpg", 22, 0.15, []Merchant{learn, playarena, streamly, fuel}},
	{"pg_monsoon", "Monsoon Commerce", "monsoon", 25, 0.09, []Merchant{fuel, bills, kiranaplus, quickride}},
}

// PickTenant chooses a tenant by traffic share.
func PickTenant(r *rand.Rand) Tenant {
	x := r.Float64()
	for _, t := range Tenants {
		if x -= t.Weight; x <= 0 {
			return t
		}
	}
	return Tenants[0]
}

// Amount draws a realistic ticket size in paise for m.
func Amount(r *rand.Rand, m Merchant) int64 {
	if len(m.Fixed) > 0 {
		return int64(m.Fixed[r.IntN(len(m.Fixed))] * 100)
	}
	v := m.Median * math.Exp(r.NormFloat64()*0.75)
	if v < 10 {
		v = 10 + r.Float64()*20
	}
	if v > 95000 {
		v = 95000
	}
	if r.IntN(3) == 0 {
		return int64(v * 100) // paise-precise basket totals
	}
	return int64(math.Round(v)) * 100
}

// hourly is the share of a day's UPI traffic per IST hour: quiet nights, a lunch peak and an evening peak.
var hourly = [24]float64{0.9, 0.5, 0.3, 0.2, 0.2, 0.4, 1.2, 2.6, 3.9, 4.8, 5.4, 5.9,
	6.6, 6.3, 5.5, 5.1, 5.0, 5.4, 6.1, 6.9, 7.3, 6.6, 4.6, 2.6}

// HourWeight is the relative traffic at local hour h (the mean over a day is 1).
func HourWeight(h int) float64 {
	var sum float64
	for _, w := range hourly {
		sum += w
	}
	return hourly[h%24] * 24 / sum
}

// DayWeight lifts weekends and paydays slightly.
func DayWeight(t time.Time) float64 {
	w := 1.0
	if wd := t.Weekday(); wd == time.Saturday || wd == time.Sunday {
		w *= 1.12
	}
	if d := t.Day(); d <= 3 {
		w *= 1.08
	}
	return w
}

var first = []string{"aarav", "vivaan", "aditya", "vihaan", "arjun", "sai", "reyansh", "ayaan", "krishna", "ishaan",
	"ananya", "diya", "aadhya", "saanvi", "pari", "myra", "anika", "kavya", "riya", "isha", "rahul", "priya",
	"neha", "amit", "sneha", "vikram", "pooja", "rohit", "meera", "karan", "deepa", "suresh", "lakshmi", "farhan",
	"zoya", "gurpreet", "harpreet", "joseph", "maria", "tenzin"}
var last = []string{"sharma", "verma", "iyer", "nair", "reddy", "patel", "shah", "gupta", "das", "singh", "khan",
	"menon", "rao", "joshi", "kulkarni", "mehta", "bose", "pillai", "chopra", "agarwal"}
var handles = []string{"okicici", "okaxis", "oksbi", "okhdfcbank", "ybl", "ibl", "axl", "paytm", "apl", "upi"}

// PayerVPA makes a plausible customer VPA.
func PayerVPA(r *rand.Rand) string {
	f, l, h := first[r.IntN(len(first))], last[r.IntN(len(last))], handles[r.IntN(len(handles))]
	switch r.IntN(4) {
	case 0:
		return fmt.Sprintf("%s.%s@%s", f, l, h)
	case 1:
		return fmt.Sprintf("%s%d@%s", f, 10+r.IntN(990), h)
	case 2:
		return fmt.Sprintf("9%09d@%s", r.IntN(1_000_000_000), h)
	}
	return fmt.Sprintf("%s%s@%s", f, l[:1], h)
}

// Decline is an NPCI response code with its customer-facing reason.
type Decline struct {
	Code, Reason string
	Technical    bool
	Weight       float64
}

// Declines is the decline mix seen on UPI: mostly customer-side business declines (BD), with a
// smaller share of technical declines (TD) at the banks.
var Declines = []Decline{
	{"Z9", "insufficient funds in payer account", false, 34},
	{"ZM", "incorrect UPI PIN", false, 21},
	{"ZA", "payer declined the request", false, 7},
	{"Z8", "per-transaction limit exceeded at remitter", false, 5},
	{"K1", "suspected fraud, declined by remitter", false, 2},
	{"YC", "do not honour (remitter)", false, 2},
	{"ZH", "invalid VPA", false, 3},
	{"U16", "risk threshold exceeded", false, 1},
	{"U30", "debit failed at remitter bank", true, 9},
	{"U28", "PSP or bank not available", true, 4},
	{"U90", "remitter bank deemed high response time", true, 4},
	{"XY", "remitter CBS offline", true, 3},
	{"BT", "beneficiary bank unavailable", true, 3},
	{"U09", "remitter bank timeout", true, 2},
}

// PickDecline draws a technical (TD) or a business (BD) decline.
func PickDecline(r *rand.Rand, technical bool) Decline {
	var sum float64
	for _, d := range Declines {
		if d.Technical == technical {
			sum += d.Weight
		}
	}
	x := r.Float64() * sum
	for _, d := range Declines {
		if d.Technical != technical {
			continue
		}
		if x -= d.Weight; x <= 0 {
			return d
		}
	}
	return Declines[0]
}
