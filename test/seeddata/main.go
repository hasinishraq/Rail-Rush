// Command seeddata fills a running dev stack with realistic bookings for the
// test users that `app migrate up --seed=dev` creates (password
// "dev-password-123"). Every booking goes through the public API, exactly as
// the web app does it (admission -> select seats -> hold -> OTP -> pay), so the
// allocator, Kafka, the projector and Postgres all agree on the result.
//
//	go run ./test/seeddata
//
// A plan that cannot be booked (no train that day, class sold out, the user's
// daily limit reached on a re-run) is reported and skipped.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	base    = flag.String("base", "http://localhost:8000", "public API base URL")
	gateway = flag.String("gateway", "http://localhost:8090", "fake payment gateway URL")
	sms     = flag.String("sms", "http://localhost:8091", "fake SMS gateway URL")
)

const password = "dev-password-123"

type pax struct{ Name, Gender, Type string }

// plan is one booking: who, where, when (days from today), which class, who travels, and how it ends.
type plan struct {
	phone, from, to string
	days            int
	class, method   string
	passengers      []pax
	end             string // "paid", "cancelled" (paid then fully cancelled), "partial" (paid, last passenger cancelled)
}

var plans = []plan{
	{"+8801711111111", "DHK", "CTG", 1, "SNIGDHA", "BKASH", []pax{{"Ayesha Rahman", "FEMALE", "ADULT"}, {"Rafiq Rahman", "MALE", "ADULT"}}, "paid"},
	{"+8801811111112", "DHK", "CTG", 1, "S_CHAIR", "NAGAD", []pax{{"Karim Uddin", "MALE", "ADULT"}, {"Salma Begum", "FEMALE", "ADULT"}}, "paid"},
	{"+8801911111113", "DHK", "SYL", 2, "S_CHAIR", "ROCKET", []pax{{"Nusrat Jahan", "FEMALE", "ADULT"}, {"Imran Hossain", "MALE", "ADULT"}, {"Mitu Jahan", "FEMALE", "ADULT"}}, "paid"},
	{"+8801611111114", "DHK", "RAJ", 2, "SNIGDHA", "VISA", []pax{{"Tanvir Hasan", "MALE", "ADULT"}}, "paid"},
	{"+8801511111115", "DHK", "KHL", 3, "AC_S", "MASTERCARD", []pax{{"Farhana Akter", "FEMALE", "ADULT"}, {"Sabbir Ahmed", "MALE", "ADULT"}}, "paid"},
	{"+8801711111111", "DHK", "COX", 3, "S_CHAIR", "BKASH", []pax{{"Ayesha Rahman", "FEMALE", "ADULT"}, {"Nabila Rahman", "FEMALE", "CHILD"}}, "partial"},
	{"+8801811111112", "DHK", "RAJ", 4, "S_CHAIR", "BKASH", []pax{{"Karim Uddin", "MALE", "ADULT"}}, "cancelled"},
	{"+8801700000001", "DHK", "CTG", 3, "AC_S", "BKASH", []pax{{"Dev User", "MALE", "ADULT"}, {"Guest Passenger", "FEMALE", "ADULT"}}, "paid"},
	{"+8801611111114", "DHK", "SYL", 5, "SNIGDHA", "UPAY", []pax{{"Tanvir Hasan", "MALE", "ADULT"}, {"Rumana Hasan", "FEMALE", "ADULT"}}, "paid"},
}

func main() {
	flag.Parse()
	ok := 0
	tokens := map[string]string{}
	for i, p := range plans {
		fmt.Printf("[%d/%d] %s %s->%s +%dd %s x%d (%s): ", i+1, len(plans), p.phone, p.from, p.to, p.days, p.class, len(p.passengers), p.end)
		tok := tokens[p.phone]
		if tok == "" {
			r, err := call("POST", "/v1/auth/login", "", nil, map[string]any{"login": p.phone, "password": password, "device_id": "seeddata"})
			if err != nil || r.status != 200 {
				fmt.Printf("SKIP login failed (run migrate with --seed=dev first): %v %s\n", err, r.raw)
				continue
			}
			tok = r.body["access_token"].(string)
			tokens[p.phone] = tok
		}
		pnr, err := book(tok, p)
		if err != nil {
			fmt.Println("SKIP", err)
			continue
		}
		fmt.Println("OK PNR", pnr)
		ok++
	}
	fmt.Printf("%d of %d bookings created\n", ok, len(plans))
	if ok == 0 {
		os.Exit(1)
	}
}

func book(tok string, p plan) (string, error) {
	date := time.Now().AddDate(0, 0, p.days).Format("2006-01-02")
	r, err := call("GET", fmt.Sprintf("/v1/search?from=%s&to=%s&date=%s", p.from, p.to, date), "", nil, nil)
	if err != nil || r.status != 200 {
		return "", fmt.Errorf("search: %v %s", err, r.raw)
	}
	// The first train that sells this class for the trip.
	var journey int64
	fromIdx, toIdx := -1, -1
	for _, tr := range list(r.body["trains"]) {
		t := tr.(map[string]any)
		for _, c := range list(t["classes"]) {
			cm := c.(map[string]any)
			if cm["class_code"] == p.class && cm["status"] == "AVAILABLE" && num(cm["count"]) >= len(p.passengers) && journey == 0 {
				journey = int64(num(t["journey_id"]))
				fromIdx = num(t["from"].(map[string]any)["idx"])
				toIdx = num(t["to"].(map[string]any)["idx"])
			}
		}
	}
	if journey == 0 {
		return "", fmt.Errorf("no train with %s seats on sale on %s", p.class, date)
	}

	r, err = call("GET", fmt.Sprintf("/v1/journeys/%d", journey), "", nil, nil)
	if err != nil || r.status != 200 {
		return "", fmt.Errorf("journey: %v %s", err, r.raw)
	}
	r, err = call("GET", fmt.Sprintf("/v1/layouts/%d", num(r.body["layout_id"])), "", nil, nil)
	if err != nil || r.status != 200 {
		return "", fmt.Errorf("layout: %v %s", err, r.raw)
	}
	layout := r.body

	adm := ""
	for i := 0; i < 20 && adm == ""; i++ {
		r, err = call("POST", "/v1/admission", tok, nil, map[string]any{"journey_id": journey})
		if err != nil || r.status != 200 {
			return "", fmt.Errorf("admission: %v %s", err, r.raw)
		}
		adm, _ = r.body["admission_token"].(string)
		if adm == "" {
			time.Sleep(time.Second)
		}
	}
	if adm == "" {
		return "", fmt.Errorf("not admitted")
	}
	hdr := func(extra ...string) map[string]string {
		h := map[string]string{"X-Admission-Token": adm}
		for i := 0; i+1 < len(extra); i += 2 {
			h[extra[i]] = extra[i+1]
		}
		return h
	}

	r, err = call("GET", fmt.Sprintf("/v1/journeys/%d/seatmap", journey), tok, nil, nil)
	if err != nil || r.status != 200 {
		return "", fmt.Errorf("seat map: %v %s", err, r.raw)
	}
	free := freeSeats(r.body, layout, p.class, fromIdx, toIdx)
	// Spread bookings over the coach instead of always taking seat 1.
	if n := len(free); n > len(p.passengers)*4 {
		off := int(journey%7) * 3 % (n - len(p.passengers))
		free = free[off:]
	}
	sel := map[string]any{"from_idx": fromIdx, "to_idx": toIdx, "class_code": p.class, "quota": "ONLINE"}
	locked := 0
	for _, seat := range free {
		if locked == len(p.passengers) {
			break
		}
		r, err = call("PUT", fmt.Sprintf("/v1/journeys/%d/selection/%s", journey, seat), tok, hdr(), sel)
		if r = settle(tok, r); err == nil && r.status == 200 {
			locked = len(list(r.body["seats"]))
			continue
		}
		if code(r) != "SEAT_TAKEN" {
			call("DELETE", fmt.Sprintf("/v1/journeys/%d/selection/*", journey), tok, hdr(), nil)
			return "", fmt.Errorf("select %s: %v %s", seat, err, r.raw)
		}
	}
	if locked != len(p.passengers) {
		call("DELETE", fmt.Sprintf("/v1/journeys/%d/selection/*", journey), tok, hdr(), nil)
		return "", fmt.Errorf("could lock only %d seats", locked)
	}

	paxIn := make([]map[string]any, len(p.passengers))
	for i, x := range p.passengers {
		paxIn[i] = map[string]any{"name": x.Name, "gender": x.Gender, "type": x.Type}
	}
	r, err = call("POST", "/v1/holds", tok, hdr("Idempotency-Key", uuid.NewString()), map[string]any{"journey_id": journey, "passengers": paxIn})
	if r = settle(tok, r); err != nil || r.status != 201 {
		call("DELETE", fmt.Sprintf("/v1/journeys/%d/selection/*", journey), tok, hdr(), nil)
		return "", fmt.Errorf("hold: %v %s", err, r.raw)
	}
	pnr, _ := r.body["pnr"].(string)
	phone := p.phone

	otp, err := lastOTP(phone, pnr)
	if err != nil {
		return pnr, err
	}
	r, err = call("POST", "/v1/holds/"+pnr+"/otp/verify", tok, nil, map[string]any{"otp": otp})
	if err != nil || r.status != 200 {
		return pnr, fmt.Errorf("otp: %v %s", err, r.raw)
	}
	r, err = call("POST", "/v1/payments", tok, map[string]string{"Idempotency-Key": uuid.NewString()},
		map[string]any{"pnr": pnr, "method": p.method, "return_url": "http://localhost:8088/pay/return"})
	if err != nil || r.status != 201 {
		return pnr, fmt.Errorf("payment: %v %s", err, r.raw)
	}
	redirect, _ := r.body["redirect_url"].(string)
	order := redirect[strings.LastIndex(redirect, "/")+1:]
	if i := strings.IndexByte(order, '?'); i >= 0 {
		order = order[:i]
	}
	if r, err = rawCall("POST", *gateway+"/orders/"+order+"/pay?outcome=success", "", nil, nil); err != nil || r.status != 200 {
		return pnr, fmt.Errorf("gateway: %v %s", err, r.raw)
	}
	if err := waitStatus(tok, pnr, "CONFIRMED"); err != nil {
		return pnr, err
	}

	switch p.end {
	case "cancelled", "partial":
		body := map[string]any{"reason": "change of plans"}
		want := "CANCELLED"
		if p.end == "partial" {
			body["passenger_seqs"] = []int{len(p.passengers)}
			want = "PARTIALLY_CANCELLED"
		}
		r, err = call("POST", "/v1/bookings/"+pnr+"/cancel", tok, map[string]string{"Idempotency-Key": uuid.NewString()}, body)
		if r = settle(tok, r); err != nil || r.status != 200 {
			return pnr, fmt.Errorf("cancel: %v %s", err, r.raw)
		}
		if err := waitStatus(tok, pnr, want); err != nil {
			return pnr, err
		}
	}
	return pnr, nil
}

func waitStatus(tok, pnr, want string) error {
	var status any
	for i := 0; i < 30; i++ {
		r, err := call("GET", "/v1/bookings/"+pnr, tok, nil, nil)
		if err == nil && r.body["status"] == want {
			return nil
		}
		status = r.body["status"]
		time.Sleep(time.Second)
	}
	return fmt.Errorf("booking %s is %v, want %s", pnr, status, want)
}

// ---- HTTP helpers (same shapes as test/smoke)

type resp struct {
	status int
	body   map[string]any
	raw    []byte
}

func call(method, path, token string, hdr map[string]string, in any) (resp, error) {
	return rawCall(method, *base+path, token, hdr, in)
}

func rawCall(method, u, token string, hdr map[string]string, in any) (resp, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, u, body)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return resp{}, err
	}
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	out := resp{status: r.StatusCode, raw: raw}
	_ = json.Unmarshal(raw, &out.body)
	return out, nil
}

// settle follows a 202 to its result through /v1/requests/{id}.
func settle(tok string, r resp) resp {
	if r.status != 202 {
		return r
	}
	id, _ := r.body["request_id"].(string)
	for i := 0; i < 30; i++ {
		time.Sleep(time.Second)
		p, _ := call("GET", "/v1/requests/"+id, tok, nil, nil)
		if p.body["state"] == "DONE" {
			raw, _ := json.Marshal(p.body["body"])
			out := resp{status: num(p.body["status_code"]), raw: raw}
			_ = json.Unmarshal(raw, &out.body)
			return out
		}
	}
	return r
}

func code(r resp) string {
	if e, ok := r.body["error"].(map[string]any); ok {
		c, _ := e["code"].(string)
		return c
	}
	return ""
}

func list(v any) []any { l, _ := v.([]any); return l }
func num(v any) int    { f, _ := v.(float64); return int(f) }

// freeSeats decodes the seat map exactly as a client does (§11.6).
func freeSeats(sm map[string]any, layout map[string]any, class string, fromIdx, toIdx int) []string {
	raw, _ := base64.StdEncoding.DecodeString(sm["seats"].(string))
	pools, _ := base64.StdEncoding.DecodeString(sm["seat_pool"].(string))
	pool := -1
	for i, p := range list(sm["pools"]) {
		if pm := p.(map[string]any); pm["class_code"] == class && pm["quota"] == "ONLINE" {
			pool = i
		}
	}
	m := (uint64(1)<<(toIdx-fromIdx) - 1) << fromIdx
	ids := map[int]string{}
	for _, c := range list(layout["coaches"]) {
		for _, s := range list(c.(map[string]any)["seats"]) {
			sm := s.(map[string]any)
			ids[num(sm["seat_idx"])] = sm["seat_id"].(string)
		}
	}
	var out []string
	for i := 0; i < len(raw)/8 && i < len(pools); i++ {
		if int(pools[i]) == pool && binary.LittleEndian.Uint64(raw[i*8:])&m == 0 {
			out = append(out, ids[i])
		}
	}
	return out
}

var otpRe = regexp.MustCompile(`OTP for PNR ([0-9]+) is ([0-9]{6})`)

func lastOTP(phone, pnr string) (string, error) {
	for i := 0; i < 30; i++ {
		r, err := rawCall("GET", *sms+"/last?phone="+url.QueryEscape(phone), "", nil, nil)
		if err == nil && r.status == 200 {
			if m := otpRe.FindStringSubmatch(fmt.Sprint(r.body["text"])); m != nil && m[1] == pnr {
				return m[2], nil
			}
		}
		time.Sleep(time.Second)
	}
	return "", fmt.Errorf("no OTP SMS for PNR %s", pnr)
}
