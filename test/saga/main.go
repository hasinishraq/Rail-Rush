// Command saga drives the timer workflows against a stack started with short
// timers (spec §25 tests 4 and 10):
//
//	HOLD_TTL=45s SELECT_IDLE_TTL=10s docker compose -f deploy/docker-compose.yml up -d
//	go run ./test/saga --journey 81620261022
//
//	W6   a cart nobody touches is released after SELECT_IDLE_TTL; passenger
//	     details then get EMPTY_CART
//	W9   an unpaid hold expires; the booking turns EXPIRED
//	W10  payment captured just after the hold expired: original seat taken,
//	     so the on-time payer gets the nearest seat (Confirmed{late, seats_changed});
//	     a payment captured too late with the seat gone gets CONFIRM_FAILED and a full refund
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
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
	journey = flag.Int64("journey", 0, "an open journey (required)")
	class   = flag.String("class", "S_CHAIR", "class code")
	coach   = flag.String("coach", "JHA", "coach to use (pick one nobody else is testing on)")
	holdTTL = flag.Duration("hold-ttl", 45*time.Second, "HOLD_TTL of the running allocators")
	idleTTL = flag.Duration("idle-ttl", 10*time.Second, "SELECT_IDLE_TTL of the running allocators")
)

type resp struct {
	status int
	body   map[string]any
	raw    []byte
}

func call(method, u, token string, hdr map[string]string, in any) resp {
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
		fail("%s %s: %v", method, u, err)
	}
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	out := resp{status: r.StatusCode, raw: raw}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "FAIL: "+f+"\n", a...)
	os.Exit(1)
}

func ok(name string, r resp, want int, code string) resp {
	got := ""
	if e, _ := r.body["error"].(map[string]any); e != nil {
		got, _ = e["code"].(string)
	}
	if r.status != want || got != code {
		fail("%s: %d %s, want %d %s: %s", name, r.status, got, want, code, r.raw)
	}
	fmt.Printf("ok   %-46s %d %s\n", name, r.status, code)
	return r
}

type user struct{ phone, tok, adm string }

func newUser() *user {
	u := &user{phone: fmt.Sprintf("+8801%d%08d", 3+rand.IntN(7), rand.IntN(100_000_000))}
	call("POST", *base+"/v1/auth/register", "", map[string]string{"Idempotency-Key": uuid.NewString()},
		map[string]any{"phone": u.phone, "name": "Saga Test", "password": "correct-horse-battery"})
	r := call("POST", *base+"/v1/auth/login", "", nil, map[string]any{"login": u.phone, "password": "correct-horse-battery", "device_id": "saga"})
	u.tok, _ = r.body["access_token"].(string)
	for i := 0; i < 20 && u.adm == ""; i++ {
		r = call("POST", *base+"/v1/admission", u.tok, nil, map[string]any{"journey_id": *journey})
		u.adm, _ = r.body["admission_token"].(string)
		if u.adm == "" {
			time.Sleep(time.Second)
		}
	}
	if u.tok == "" || u.adm == "" {
		fail("cannot set up user: %s", r.raw)
	}
	return u
}

func (u *user) h(idem bool) map[string]string {
	h := map[string]string{"X-Admission-Token": u.adm}
	if idem {
		h["Idempotency-Key"] = uuid.NewString()
	}
	return h
}

func (u *user) sel(seat string) resp {
	return call("PUT", fmt.Sprintf("%s/v1/journeys/%d/selection/%s", *base, *journey, seat), u.tok, u.h(false),
		map[string]any{"from_idx": 0, "to_idx": 1, "class_code": *class})
}

var otpRe = regexp.MustCompile(`OTP for PNR [0-9]+ is ([0-9]{6})`)

// holdAndVerify selects seat, submits passenger details and verifies the OTP.
func (u *user) holdAndVerify(seat string) (pnr string, expires time.Time) {
	ok("select "+seat, u.sel(seat), 200, "")
	r := ok("passenger details", call("POST", *base+"/v1/holds", u.tok, u.h(true),
		map[string]any{"journey_id": *journey, "passengers": []map[string]any{{"name": "Saga", "gender": "OTHER"}}}), 201, "")
	pnr = r.body["pnr"].(string)
	expires, _ = time.Parse(time.RFC3339Nano, r.body["expires_at"].(string))
	for i := 0; i < 30; i++ {
		s := call("GET", *sms+"/last?phone="+url.QueryEscape(u.phone), "", nil, nil)
		if m := otpRe.FindStringSubmatch(fmt.Sprint(s.body["text"])); m != nil && strings.Contains(fmt.Sprint(s.body["text"]), pnr) {
			ok("verify OTP", call("POST", *base+"/v1/holds/"+pnr+"/otp/verify", u.tok, nil, map[string]any{"otp": m[1]}), 200, "")
			return pnr, expires
		}
		time.Sleep(time.Second)
	}
	fail("no OTP for %s", pnr)
	return
}

func (u *user) pay(pnr string) (orderID string) {
	r := ok("create payment (Nagad)", call("POST", *base+"/v1/payments", u.tok, map[string]string{"Idempotency-Key": uuid.NewString()},
		map[string]any{"pnr": pnr, "method": "NAGAD", "return_url": "http://localhost/return"}), 201, "")
	redirect := r.body["redirect_url"].(string)
	return redirect[strings.LastIndex(redirect, "/")+1:]
}

func (u *user) waitStatus(pnr string, want ...string) map[string]any {
	var r resp
	for i := 0; i < 60; i++ {
		r = call("GET", *base+"/v1/bookings/"+pnr, u.tok, nil, nil)
		for _, w := range want {
			if r.body["status"] == w {
				fmt.Printf("ok   %-46s %s\n", "booking "+pnr, w)
				return r.body
			}
		}
		time.Sleep(time.Second)
	}
	fail("booking %s: status %v, want %v: %s", pnr, r.body["status"], want, r.raw)
	return nil
}

func until(t time.Time) {
	if d := time.Until(t); d > 0 {
		fmt.Printf("     waiting %v\n", d.Round(time.Second))
		time.Sleep(d)
	}
}

func main() {
	flag.Parse()
	if *journey == 0 {
		fail("--journey is required")
	}
	seat := func(n int) string { return fmt.Sprintf("%s-%d", *coach, n) }

	// W6 — lock expiry.
	a := newUser()
	ok("W6 select", a.sel(seat(1)), 200, "")
	until(time.Now().Add(*idleTTL + 2*time.Second))
	ok("W6 cart released after idle TTL", call("GET", fmt.Sprintf("%s/v1/journeys/%d/selection", *base, *journey), a.tok, nil, nil), 404, "NOT_FOUND")
	ok("W6 passenger details -> EMPTY_CART", call("POST", *base+"/v1/holds", a.tok, a.h(true),
		map[string]any{"journey_id": *journey, "passengers": []map[string]any{{"name": "Late", "gender": "MALE"}}}), 409, "EMPTY_CART")
	b := newUser()
	time.Sleep(time.Second) // the release reaches the seat mirrors with the next delta
	ok("W6 seat free for someone else", b.sel(seat(1)), 200, "")
	call("DELETE", fmt.Sprintf("%s/v1/journeys/%d/selection/*", *base, *journey), b.tok, b.h(false), nil)

	// W9 — hold expiry.
	c := newUser()
	pnr9, exp9 := c.holdAndVerify(seat(2))
	until(exp9.Add(2 * time.Second))
	c.waitStatus(pnr9, "EXPIRED")

	// W10a — paid just after expiry (within the 30 s grace), original seat taken.
	d := newUser()
	pnrA, expA := d.holdAndVerify(seat(3))
	orderA := d.pay(pnrA)
	until(expA.Add(time.Second))
	e := newUser()
	time.Sleep(time.Second)
	ok("W10 someone else takes the released seat", e.sel(seat(3)), 200, "")
	ok("W10 pay 1 s after expiry", call("POST", *gateway+"/orders/"+orderA+"/pay?outcome=success", "", nil, nil), 200, "")
	bk := d.waitStatus(pnrA, "CONFIRMED", "CONFIRM_FAILED")
	pax := bk["passengers"].([]any)[0].(map[string]any)
	got := pax["seat"].(map[string]any)["seat_id"]
	if bk["status"] != "CONFIRMED" || got == seat(3) {
		fail("W10 on-time payer: status %v seat %v (want CONFIRMED on another seat): %v", bk["status"], got, bk)
	}
	fmt.Printf("     on-time payer moved from %s to %v (seats_changed=%v)\n", seat(3), got, bk["seats_changed"])

	// W10b — paid long after expiry with the seat gone: refund.
	f := newUser()
	pnrB, expB := f.holdAndVerify(seat(5))
	orderB := f.pay(pnrB)
	until(expB.Add(35 * time.Second))
	g := newUser()
	ok("W10 someone else takes the released seat", g.sel(seat(5)), 200, "")
	ok("W10 pay 35 s after expiry", call("POST", *gateway+"/orders/"+orderB+"/pay?outcome=success", "", nil, nil), 200, "")
	// Not on time and the original seat taken: ConfirmFailed and a full refund,
	// even though other seats are free (§6.3).
	bk = f.waitStatus(pnrB, "CONFIRM_FAILED")
	for i := 0; i < 30; i++ {
		bk = call("GET", *base+"/v1/bookings/"+pnrB, f.tok, nil, nil).body
		if rs, _ := bk["refunds"].([]any); len(rs) > 0 {
			fmt.Printf("ok   %-46s %v\n", "W10 full refund", rs[0])
			for _, u := range []*user{e, g} {
				call("DELETE", fmt.Sprintf("%s/v1/journeys/%d/selection/*", *base, *journey), u.tok, u.h(false), nil)
			}
			fmt.Println("PASS")
			return
		}
		time.Sleep(time.Second)
	}
	fail("W10 no refund for %s: %v", pnrB, bk)
}
