// Command smoke drives the v4.0 happy path end to end against a running stack
// (W2, W3, W4, W5, W7, W8, W13):
//
//	register two users -> login -> refresh reuse -> search -> admission ->
//	seat map -> select / double click / unselect (user A) -> SEAT_TAKEN for
//	user B -> passenger details (hold + OTP) -> payment refused before OTP ->
//	OTP from the fake SMS gateway -> pay -> CONFIRMED -> ticket -> partial
//	cancel -> refund -> user B can select the cancelled seat.
//
//	go run ./test/smoke --from DHK --to CTG --class SNIGDHA
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
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
	base     = flag.String("base", "http://localhost:8000", "public API base URL")
	gateway  = flag.String("gateway", "http://localhost:8090", "fake payment gateway URL")
	sms      = flag.String("sms", "http://localhost:8091", "fake SMS gateway URL")
	internal = flag.String("internal", "http://localhost:9091", "internal endpoints of the auth pod")
	journey  = flag.Int64("journey", 0, "journey id (0 = first train found by search)")
	from     = flag.String("from", "DHK", "from station")
	to       = flag.String("to", "CTG", "to station")
	date     = flag.String("date", time.Now().AddDate(0, 0, 2).Format("2006-01-02"), "travel date (YYYY-MM-DD)")
	class    = flag.String("class", "S_CHAIR", "class code")
	intToken = flag.String("internal-token", os.Getenv("INTERNAL_TOKEN"), "X-Internal-Token (default $INTERNAL_TOKEN)")
)

func main() {
	flag.Parse()
	if *intToken == "" {
		fmt.Fprintln(os.Stderr, "FAIL: --internal-token or INTERNAL_TOKEN is required")
		os.Exit(1)
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

type resp struct {
	status int
	body   map[string]any
	raw    []byte
}

func call(method, url, token string, hdr map[string]string, in any) (resp, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, body)
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

func idem() map[string]string { return map[string]string{"Idempotency-Key": uuid.NewString()} }

func step(name string, r resp, err error, want ...int) error {
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	for _, w := range want {
		if r.status == w {
			fmt.Printf("ok   %-34s %d\n", name, r.status)
			return nil
		}
	}
	return fmt.Errorf("%s: status %d: %s", name, r.status, r.raw)
}

func code(r resp) string {
	if e, ok := r.body["error"].(map[string]any); ok {
		c, _ := e["code"].(string)
		return c
	}
	return ""
}

func expectCode(name string, r resp, err error, status int, c string) error {
	if err := step(name, r, err, status); err != nil {
		return err
	}
	if got := code(r); got != c {
		return fmt.Errorf("%s: code %q, want %q: %s", name, got, c, r.raw)
	}
	return nil
}

type user struct {
	phone, tok, adm string
}

func (u *user) admHdr(extra map[string]string) map[string]string {
	h := map[string]string{"X-Admission-Token": u.adm}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func newUser(name string) (*user, error) {
	u := &user{phone: fmt.Sprintf("+8801%d%08d", 3+rand.IntN(7), rand.IntN(100_000_000))}
	r, err := call("POST", *base+"/v1/auth/register", "", idem(), map[string]any{
		"phone": u.phone, "name": name, "nid": fmt.Sprintf("%010d", rand.Int64N(10_000_000_000)), "password": "correct-horse-battery"})
	if err := step("register "+name, r, err, 201); err != nil {
		return nil, err
	}
	r, err = call("POST", *base+"/v1/auth/login", "", nil, map[string]any{"login": u.phone, "password": "correct-horse-battery", "device_id": "smoke"})
	if err := step("login "+name, r, err, 200); err != nil {
		return nil, err
	}
	u.tok = r.body["access_token"].(string)
	return u, nil
}

func (u *user) admit() error {
	for i := 0; i < 20; i++ {
		r, err := call("POST", *base+"/v1/admission", u.tok, nil, map[string]any{"journey_id": *journey})
		if err := step("admission", r, err, 200); err != nil {
			return err
		}
		if a, _ := r.body["admission_token"].(string); a != "" {
			u.adm = a
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("not admitted")
}

// settle follows a 202 to its result through /v1/requests/{id}.
func settle(u *user, r resp) resp {
	if r.status != 202 {
		return r
	}
	id, _ := r.body["request_id"].(string)
	for i := 0; i < 30; i++ {
		time.Sleep(time.Second)
		p, _ := call("GET", *base+"/v1/requests/"+id, u.tok, nil, nil)
		if p.body["state"] == "DONE" {
			raw, _ := json.Marshal(p.body["body"])
			out := resp{status: int(p.body["status_code"].(float64)), raw: raw}
			_ = json.Unmarshal(raw, &out.body)
			return out
		}
	}
	return r
}

func cartSeats(r resp) []string {
	var out []string
	seats, _ := r.body["seats"].([]any)
	for _, s := range seats {
		out = append(out, s.(map[string]any)["seat_id"].(string))
	}
	return out
}

// freeSeats decodes the seat map exactly as a client does (§11.6).
func freeSeats(sm map[string]any, layout map[string]any, class string, fromIdx, toIdx int) []string {
	raw, _ := base64.StdEncoding.DecodeString(sm["seats"].(string))
	pools, _ := base64.StdEncoding.DecodeString(sm["seat_pool"].(string))
	pool := -1
	for i, p := range sm["pools"].([]any) {
		if pm := p.(map[string]any); pm["class_code"] == class {
			pool = i
		}
	}
	m := (uint64(1)<<(toIdx-fromIdx) - 1) << fromIdx
	ids := map[int]string{}
	for _, c := range layout["coaches"].([]any) {
		for _, s := range c.(map[string]any)["seats"].([]any) {
			sm := s.(map[string]any)
			ids[int(sm["seat_idx"].(float64))] = sm["seat_id"].(string)
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

var otpRe = regexp.MustCompile(`OTP for PNR [0-9]+ is ([0-9]{6})`)

func lastOTP(phone string) (string, error) {
	for i := 0; i < 30; i++ {
		r, err := call("GET", *sms+"/last?phone="+url.QueryEscape(phone), "", nil, nil)
		if err == nil && r.status == 200 {
			if m := otpRe.FindStringSubmatch(fmt.Sprint(r.body["text"])); m != nil {
				return m[1], nil
			}
		}
		time.Sleep(time.Second)
	}
	return "", fmt.Errorf("no OTP SMS for %s", phone)
}

func run() error {
	a, err := newUser("Ayesha Rahman")
	if err != nil {
		return err
	}
	b, err := newUser("Karim Uddin")
	if err != nil {
		return err
	}

	r, err := call("POST", *base+"/v1/auth/login", "", nil, map[string]any{"login": a.phone, "password": "correct-horse-battery", "device_id": "smoke"})
	if err := step("login again", r, err, 200); err != nil {
		return err
	}
	refresh := r.body["refresh_token"].(string)
	r, err = call("POST", *base+"/v1/auth/refresh", "", nil, map[string]any{"refresh_token": refresh, "device_id": "smoke"})
	if err := step("refresh", r, err, 200); err != nil {
		return err
	}
	r, err = call("POST", *base+"/v1/auth/refresh", "", nil, map[string]any{"refresh_token": refresh, "device_id": "smoke"})
	if err := expectCode("refresh reuse", r, err, 401, "REFRESH_REUSED"); err != nil {
		return err
	}

	r, err = call("GET", fmt.Sprintf("%s/v1/search?from=%s&to=%s&date=%s&class=%s", *base, *from, *to, *date, *class), "", nil, nil)
	if err := step("search", r, err, 200); err != nil {
		return err
	}
	fromIdx, toIdx := -1, -1
	trains, _ := r.body["trains"].([]any)
	for _, tr := range trains {
		t := tr.(map[string]any)
		if *journey == 0 || int64(t["journey_id"].(float64)) == *journey {
			*journey = int64(t["journey_id"].(float64))
			fromIdx = int(t["from"].(map[string]any)["idx"].(float64))
			toIdx = int(t["to"].(map[string]any)["idx"].(float64))
			fmt.Printf("     train %s, journey %d, classes %v\n", t["train_name"], *journey, t["classes"])
			break
		}
	}
	if fromIdx < 0 {
		return fmt.Errorf("no train %s to %s on %s: %.300s", *from, *to, *date, r.raw)
	}
	r, err = call("GET", fmt.Sprintf("%s/v1/journeys/%d", *base, *journey), "", nil, nil)
	if err := step("journey", r, err, 200); err != nil {
		return err
	}
	r, err = call("GET", fmt.Sprintf("%s/v1/layouts/%d", *base, int(r.body["layout_id"].(float64))), "", nil, nil)
	if err := step("layout", r, err, 200); err != nil {
		return err
	}
	layout := r.body

	if err := a.admit(); err != nil {
		return err
	}
	if err := b.admit(); err != nil {
		return err
	}
	r, err = call("GET", fmt.Sprintf("%s/v1/journeys/%d/seatmap", *base, *journey), a.tok, nil, nil)
	if err := step("seat map (no admission needed)", r, err, 200, 409); err != nil {
		return err
	}
	if r.status == 409 {
		return fmt.Errorf("booking window not open; run: docker compose -f deploy/docker-compose.yml run --rm migrate migrate up --seed=dev --open-in=1m")
	}
	free := freeSeats(r.body, layout, *class, fromIdx, toIdx)
	if len(free) < 3 {
		return fmt.Errorf("only %d free seats", len(free))
	}
	s1, s2 := free[0], free[1]
	sel := map[string]any{"from_idx": fromIdx, "to_idx": toIdx, "class_code": *class}
	selURL := func(seat string) string { return fmt.Sprintf("%s/v1/journeys/%d/selection/%s", *base, *journey, seat) }

	r, err = call("PUT", selURL(s1), a.tok, a.admHdr(nil), sel)
	if err := step("A selects "+s1, settle(a, r), err, 200); err != nil {
		return err
	}
	r, err = call("PUT", selURL(s2), a.tok, a.admHdr(nil), sel)
	r = settle(a, r)
	if err := step("A selects "+s2, r, err, 200); err != nil {
		return err
	}
	if got := cartSeats(r); len(got) != 2 {
		return fmt.Errorf("cart %v, want 2 seats", got)
	}
	r, err = call("PUT", selURL(s1), a.tok, a.admHdr(nil), sel)
	if err := step("A double-clicks "+s1, settle(a, r), err, 200); err != nil {
		return err
	}
	r, err = call("PUT", selURL(s1), b.tok, b.admHdr(nil), sel)
	if err := expectCode("B selects A's seat", settle(b, r), err, 409, "SEAT_TAKEN"); err != nil {
		return err
	}
	r, err = call("DELETE", selURL(s1), b.tok, b.admHdr(nil), nil)
	if r = settle(b, r); step("B cannot release A's seat", r, err, 200) != nil || len(cartSeats(r)) != 0 {
		return fmt.Errorf("B released someone else's seat: %s", r.raw)
	}
	r, err = call("DELETE", selURL(s2), a.tok, a.admHdr(nil), nil)
	if r = settle(a, r); step("A unselects "+s2, r, err, 200) != nil || len(cartSeats(r)) != 1 {
		return fmt.Errorf("unselect: %s", r.raw)
	}
	r, err = call("PUT", selURL(s2), a.tok, a.admHdr(nil), sel)
	if err := step("A selects "+s2+" again", settle(a, r), err, 200); err != nil {
		return err
	}
	r, err = call("GET", fmt.Sprintf("%s/v1/journeys/%d/selection", *base, *journey), a.tok, nil, nil)
	if err := step("A's cart", r, err, 200); err != nil {
		return err
	}
	fmt.Printf("     cart %v expires %v\n", cartSeats(r), r.body["expires_at"])

	pax := []map[string]any{{"name": "Ayesha Rahman", "gender": "FEMALE"}, {"name": "Rafiq Rahman", "gender": "MALE", "type": "CHILD"}}
	r, err = call("POST", *base+"/v1/holds", a.tok, a.admHdr(idem()), map[string]any{"journey_id": *journey, "passengers": pax[:1]})
	if err := expectCode("hold with too few passengers", r, err, 422, "PASSENGER_COUNT_MISMATCH"); err != nil {
		return err
	}
	holdHdr := a.admHdr(idem())
	holdBody := map[string]any{"journey_id": *journey, "passengers": pax}
	r, err = call("POST", *base+"/v1/holds", a.tok, holdHdr, holdBody)
	r = settle(a, r)
	if err := step("passenger details -> hold", r, err, 201); err != nil {
		return err
	}
	pnr, _ := r.body["pnr"].(string)
	if r.body["status"] != "PENDING_OTP" || pnr == "" {
		return fmt.Errorf("hold: %s", r.raw)
	}
	fmt.Printf("     PNR %s amount %v (fare %v + charge %v), otp sent to %v\n", pnr, r.body["amount"], r.body["fare"],
		r.body["service_charge"], r.body["otp"].(map[string]any)["sent_to"])
	r, err = call("POST", *base+"/v1/holds", a.tok, holdHdr, holdBody)
	if r = settle(a, r); step("hold retry (same key)", r, err, 201) != nil || r.body["pnr"] != pnr {
		return fmt.Errorf("retry did not return PNR %s: %s", pnr, r.raw)
	}
	r, err = call("POST", *base+"/v1/holds", a.tok, a.admHdr(idem()), holdBody)
	if err := expectCode("new hold with a used admission", r, err, 403, "ADMISSION_USED"); err != nil {
		return err
	}

	pay := map[string]any{"pnr": pnr, "method": "BKASH", "return_url": "http://localhost/return"}
	r, err = call("POST", *base+"/v1/payments", a.tok, idem(), pay)
	if err := expectCode("payment before OTP", r, err, 409, "OTP_REQUIRED"); err != nil {
		return err
	}
	code6, err := lastOTP(a.phone)
	if err != nil {
		return err
	}
	// A code that is neither the texted one nor the stack's OTP_DEV_CODE (both are accepted).
	wrong := "000000"
	for _, c := range []string{"000000", "111111", "222222"} {
		if c != code6 && c != os.Getenv("OTP_DEV_CODE") {
			wrong = c
			break
		}
	}
	r, err = call("POST", *base+"/v1/holds/"+pnr+"/otp/verify", a.tok, nil, map[string]any{"otp": wrong})
	if err := expectCode("wrong OTP", r, err, 422, "OTP_INVALID"); err != nil {
		return err
	}
	r, err = call("POST", *base+"/v1/holds/"+pnr+"/otp/verify", a.tok, nil, map[string]any{"otp": code6})
	if err := step("OTP verified", r, err, 200); err != nil {
		return err
	}

	r, err = call("POST", *base+"/v1/payments", a.tok, idem(), pay)
	if err := step("create payment (bKash)", r, err, 201); err != nil {
		return err
	}
	paymentID := r.body["payment_id"].(string)
	redirect := r.body["redirect_url"].(string)
	orderID := redirect[strings.LastIndex(redirect, "/")+1:]
	r, err = call("POST", *gateway+"/orders/"+orderID+"/pay?outcome=success", "", nil, nil)
	if err := step("gateway pay", r, err, 200); err != nil {
		return err
	}
	var status string
	for i := 0; i < 30; i++ {
		time.Sleep(time.Second)
		r, err = call("GET", *base+"/v1/bookings/"+pnr, a.tok, nil, nil)
		if err != nil {
			return err
		}
		if status, _ = r.body["status"].(string); status == "CONFIRMED" {
			break
		}
	}
	if err := step("booking confirmed", r, err, 200); err != nil {
		return err
	}
	if status != "CONFIRMED" {
		return fmt.Errorf("booking status %s: %s", status, r.raw)
	}
	fmt.Printf("     booking: %.400s\n", r.raw)
	r, err = call("GET", *base+"/v1/payments/"+paymentID, a.tok, nil, nil)
	if err := step("payment status", r, err, 200); err != nil {
		return err
	}
	r, err = call("GET", *base+"/v1/bookings/"+pnr+"/ticket", a.tok, nil, nil)
	if err := step("ticket pdf", r, err, 200); err != nil {
		return err
	}
	if !bytes.HasPrefix(r.raw, []byte("%PDF")) {
		return fmt.Errorf("ticket is not a PDF")
	}

	r, err = call("POST", *base+"/v1/bookings/"+pnr+"/cancel", a.tok, idem(), map[string]any{"passenger_seqs": []int{2}})
	if err := step("cancel passenger 2", settle(a, r), err, 200); err != nil {
		return err
	}
	for i := 0; i < 30; i++ {
		time.Sleep(time.Second)
		r, _ = call("GET", *base+"/v1/bookings/"+pnr, a.tok, nil, nil)
		if refunds, _ := r.body["refunds"].([]any); len(refunds) > 0 && r.body["status"] == "PARTIALLY_CANCELLED" {
			fmt.Printf("     refunds: %v\n", refunds)
			break
		}
	}
	if r.body["status"] != "PARTIALLY_CANCELLED" {
		return fmt.Errorf("expected PARTIALLY_CANCELLED: %s", r.raw)
	}
	time.Sleep(time.Second) // the cancelled seat reaches the API seat mirrors with the next delta
	r, err = call("PUT", selURL(s2), b.tok, b.admHdr(nil), sel)
	if err := step("B selects the cancelled seat "+s2, settle(b, r), err, 200); err != nil {
		return err
	}
	r, err = call("DELETE", selURL("*"), b.tok, b.admHdr(nil), nil)
	if err := step("B releases the whole cart", settle(b, r), err, 200); err != nil {
		return err
	}
	r, err = call("GET", *base+"/v1/me/bookings", a.tok, nil, nil)
	if err := step("my bookings", r, err, 200); err != nil {
		return err
	}
	time.Sleep(2 * time.Second)
	r, err = call("GET", fmt.Sprintf("%s/internal/v1/journeys/%d/invariants", *internal, *journey), "", map[string]string{"X-Internal-Token": *intToken}, nil)
	if err := step("invariants", r, err, 200); err != nil {
		return err
	}
	if ok, _ := r.body["ok"].(bool); !ok {
		return fmt.Errorf("invariants: %s", r.raw)
	}
	return nil
}
