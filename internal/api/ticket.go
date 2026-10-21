package api

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"rsc.io/qr"

	"railway/internal/api/dto"
	"railway/internal/catalog"
	"railway/internal/ticketsig"
)

// Ticket colours, from the web app's livery (web/src/styles.css).
type rgb struct{ r, g, b int }

var (
	cLivery  = rgb{11, 107, 79}   // --livery
	cBand    = rgb{224, 64, 47}   // --band
	cInk     = rgb{15, 29, 43}    // --ink
	cMuted   = rgb{101, 114, 128} // secondary text
	cLine    = rgb{225, 230, 234} // --line
	cPanel   = rgb{243, 245, 244} // --concrete
	cMint    = rgb{230, 244, 238} // --mint
	cGreen   = rgb{19, 138, 91}   // --green
	cAmber   = rgb{180, 116, 15}  // --amber
	cAmberBg = rgb{253, 243, 225}
	cRed     = rgb{192, 53, 43} // --red
	cWhite   = rgb{255, 255, 255}
)

var ticketClassNames = map[string]string{
	"S_CHAIR": "Shovan Chair", "SHOVAN": "Shovan", "SNIGDHA": "Snigdha (AC chair)", "F_SEAT": "First Seat",
	"F_CHAIR": "First Chair", "F_BERTH": "First Berth", "AC_S": "AC Seat", "AC_B": "AC Berth",
}

const (
	pageW   = 210.0
	marginX = 18.0
	contW   = pageW - 2*marginX
)

// renderTicket draws the one-page A4 e-ticket: brand header, a journey card
// with a tear-off stub holding the QR code, trip details, passengers, fare and
// travel notes. Everything is vector, so the file stays small and prints sharp.
// Times are Asia/Dhaka. qr is the signed payload from qrPayload. Production
// caches the file in object storage keyed by pnr + updated_at.
func renderTicket(b *dto.BookingResponse, qr string, now time.Time) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(marginX, 0, marginX)
	pdf.SetAutoPageBreak(false, 0)
	pdf.SetTitle("E-ticket "+b.PNR, false)
	pdf.SetAuthor("RailRush", false)
	pdf.SetCreator("RailRush", false)
	pdf.AddPage()
	t := &ticketPDF{Fpdf: pdf, tr: pdf.UnicodeTranslatorFromDescriptor(""), bn: newBanglaText()}

	t.header(b)
	t.journeyCard(b, qr)
	y := t.details(b, 116)
	y = t.passengers(b, y+10)
	y = t.total(b, y+6)
	t.notes(b, y+12)
	t.footer(b, now)

	var out bytes.Buffer
	if err := pdf.Output(&out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type ticketPDF struct {
	*fpdf.Fpdf
	tr func(string) string
	bn *banglaText // nil if the embedded fonts failed to load
}

func (t *ticketPDF) fill(c rgb)  { t.SetFillColor(c.r, c.g, c.b) }
func (t *ticketPDF) draw(c rgb)  { t.SetDrawColor(c.r, c.g, c.b) }
func (t *ticketPDF) color(c rgb) { t.SetTextColor(c.r, c.g, c.b) }

// width measures s as text() would draw it.
func (t *ticketPDF) width(s, style string, size float64) float64 {
	if t.bn != nil && hasBangla(s) {
		return t.bn.width(s, style, size)
	}
	t.SetFont("Helvetica", style, size)
	return t.GetStringWidth(t.tr(s))
}

// text writes s with its baseline at y; align is "L", "C" or "R" around x.
// Text with Bangla in it is drawn in Hind Siliguri (ticket_bangla.go).
func (t *ticketPDF) text(x, y float64, s, style string, size float64, c rgb, align string) float64 {
	w := t.width(s, style, size)
	switch align {
	case "C":
		x -= w / 2
	case "R":
		x -= w
	}
	if t.bn != nil && hasBangla(s) {
		t.fill(c)
		t.bn.draw(t, x, y, s, style, size)
		return w
	}
	t.SetFont("Helvetica", style, size)
	t.color(c)
	t.Text(x, y, t.tr(s))
	return w
}

// label is the small grey caption above a value.
func (t *ticketPDF) label(x, y float64, s, align string) {
	t.text(x, y, strings.ToUpper(s), "B", 6.5, cMuted, align)
}

func (t *ticketPDF) hairline(y float64) {
	t.draw(cLine)
	t.SetLineWidth(0.25)
	t.Line(marginX, y, pageW-marginX, y)
}

func (t *ticketPDF) header(b *dto.BookingResponse) {
	// Livery stripe across the top edge.
	t.fill(cLivery)
	t.Rect(0, 0, pageW, 3, "F")
	t.fill(cBand)
	t.Rect(0, 3, pageW, 0.8, "F")

	// Brand mark: the app's green tile with the red disc.
	t.fill(cLivery)
	t.RoundedRect(marginX, 14, 10, 10, 2.4, "1234", "F")
	t.fill(cBand)
	t.Circle(marginX+4.6, 19, 2.3, "F")
	t.text(marginX+13.5, 18.6, "RailRush", "B", 13, cInk, "L")
	t.text(marginX+13.5, 23.4, "Bangladesh Railway  ·  Electronic ticket", "", 8, cMuted, "L")

	right := pageW - marginX
	t.label(right, 16.5, "E-ticket", "R")
	fg, bg, word := cGreen, cMint, "CONFIRMED"
	if b.Status == "PARTIALLY_CANCELLED" {
		fg, bg, word = cAmber, cAmberBg, "PARTLY CANCELLED"
	}
	t.SetFont("Helvetica", "B", 7.5)
	w := t.GetStringWidth(word) + 7
	t.fill(bg)
	t.RoundedRect(right-w, 18.6, w, 5.6, 2.8, "1234", "F")
	t.fill(fg)
	t.Circle(right-w+2.8, 21.4, 0.75, "F")
	t.text(right-w+4.6, 22.5, word, "B", 7.5, fg, "L")

	t.hairline(31)
}

func (t *ticketPDF) journeyCard(b *dto.BookingResponse, qr string) {
	const (
		top, h = 40.0, 66.0
		stubX  = 148.0 // perforation between the ticket and its stub
		left   = marginX + 9
		right  = stubX - 9
	)
	t.fill(cPanel)
	t.RoundedRect(marginX, top, contW, h, 4, "1234", "F")

	from, to := b.From.Time.In(catalog.Dhaka), b.To.Time.In(catalog.Dhaka)

	// From / to.
	t.label(left, top+11, "From", "L")
	t.label(right, top+11, "To", "R")
	t.text(left, top+25, b.From.StationCode, "B", 30, cInk, "L")
	t.text(right, top+25, b.To.StationCode, "B", 30, cInk, "R")
	t.text(left, top+32, b.From.StationName, "", 9, cMuted, "L")
	t.text(right, top+32, b.To.StationName, "", 9, cMuted, "R")
	t.text(left, top+46, from.Format("15:04"), "B", 16, cInk, "L")
	t.text(right, top+46, to.Format("15:04"), "B", 16, cInk, "R")
	t.text(left, top+52, from.Format("Mon, 02 Jan 2006"), "", 8, cMuted, "L")
	t.text(right, top+52, to.Format("Mon, 02 Jan 2006"), "", 8, cMuted, "R")

	// Route line with the train in the middle.
	lineY, x1, x2 := top+19, left+27, right-27
	mid := (x1 + x2) / 2
	t.draw(cLivery)
	t.SetLineWidth(0.5)
	t.SetDashPattern([]float64{1.2, 1.2}, 0)
	t.Line(x1, lineY, x2, lineY)
	t.SetDashPattern([]float64{}, 0)
	t.fill(cLivery)
	t.Circle(x1, lineY, 1.1, "F")
	t.fill(cWhite)
	t.Circle(x2, lineY, 1.1, "FD")
	t.trainBadge(mid, lineY, 5.5)
	t.text(mid, lineY-8.5, duration(to.Sub(from)), "B", 8, cInk, "C")
	t.text(mid, lineY+11, "Train "+b.TrainNo, "", 7.5, cMuted, "C")

	// Perforation: notches at both edges and a dashed tear line.
	t.fill(cWhite)
	t.Circle(stubX, top, 3.2, "F")
	t.Circle(stubX, top+h, 3.2, "F")
	t.draw(rgb{200, 207, 213})
	t.SetLineWidth(0.3)
	t.SetDashPattern([]float64{1.4, 1.4}, 0)
	t.Line(stubX, top+5, stubX, top+h-5)
	t.SetDashPattern([]float64{}, 0)

	// Stub: QR code and PNR.
	stubMid := (stubX + pageW - marginX) / 2
	t.label(stubMid, top+10, "Scan at the gate", "C")
	t.qrCode(qr, stubMid-14, top+13, 28)
	t.label(stubMid, top+50, "PNR", "C")
	t.SetFont("Courier", "B", 13)
	t.color(cInk)
	pnr := t.tr(b.PNR)
	t.Text(stubMid-t.GetStringWidth(pnr)/2, top+57, pnr)
}

// trainBadge is a green disc with a front view of a train.
func (t *ticketPDF) trainBadge(cx, cy, r float64) {
	t.fill(cLivery)
	t.Circle(cx, cy, r, "F")
	t.fill(cWhite)
	t.RoundedRect(cx-2.4, cy-3.3, 4.8, 5.4, 1.2, "1234", "F")
	t.fill(cLivery)
	t.Rect(cx-1.65, cy-2.5, 3.3, 1.7, "F")
	t.Circle(cx-1.25, cy+0.9, 0.38, "F")
	t.Circle(cx+1.25, cy+0.9, 0.38, "F")
	t.draw(cWhite)
	t.SetLineWidth(0.55)
	t.SetLineCapStyle("round")
	t.Line(cx-1.3, cy+2.2, cx-2.3, cy+3.5)
	t.Line(cx+1.3, cy+2.2, cx+2.3, cy+3.5)
	t.SetLineCapStyle("butt")
}

func (t *ticketPDF) qrCode(payload string, x, y, size float64) {
	code, err := qr.Encode(payload, qr.M)
	if err != nil {
		return
	}
	const quiet = 2 // modules of white border
	n := float64(code.Size + 2*quiet)
	m := size / n
	t.fill(cWhite)
	t.RoundedRect(x, y, size, size, 1.5, "1234", "F")
	t.fill(cInk)
	for row := 0; row < code.Size; row++ {
		for col := 0; col < code.Size; col++ {
			if code.Black(col, row) {
				// A hair of overlap avoids white seams between modules in some viewers.
				t.Rect(x+float64(col+quiet)*m, y+float64(row+quiet)*m, m+0.02, m+0.02, "F")
			}
		}
	}
}

// details is the row of trip facts under the card. It returns the next y.
func (t *ticketPDF) details(b *dto.BookingResponse, y float64) float64 {
	class := ticketClassNames[b.ClassCode]
	if class == "" {
		class = b.ClassCode
	}
	cells := [][2]string{
		{"Train", b.TrainNo + "  " + b.TrainName},
		{"Travel date", runDate(b.RunDate)},
		{"Class", class},
		{"Coach / seats", seatSummary(b)},
	}
	colW := contW / float64(len(cells))
	for i, c := range cells {
		x := marginX + float64(i)*colW
		if i > 0 {
			t.draw(cLine)
			t.SetLineWidth(0.25)
			t.Line(x-3, y-3, x-3, y+8)
		}
		t.label(x, y, c[0], "L")
		t.text(x, y+6, fit(t, c[1], "B", 10, colW-6), "B", 10, cInk, "L")
	}
	t.hairline(y + 14)
	return y + 14
}

// passengers draws the passenger table. It returns the y below it.
func (t *ticketPDF) passengers(b *dto.BookingResponse, y float64) float64 {
	t.text(marginX, y, "Passengers", "B", 10.5, cInk, "L")
	y += 4

	type col struct {
		title string
		w     float64
		align string
	}
	cols := []col{{"#", 9, "L"}, {"Name", 61, "L"}, {"Ticket", 21, "L"}, {"Gender", 21, "L"}, {"Seat", 22, "L"}, {"Status", 20, "L"}, {"Fare", 20, "R"}}
	const rowH = 9.0

	t.fill(cPanel)
	t.RoundedRect(marginX, y, contW, 7, 1.5, "1234", "F")
	x := marginX
	for _, c := range cols {
		cx := x + 3
		if c.align == "R" {
			cx = x + c.w - 3
		}
		t.label(cx, y+4.7, c.title, c.align)
		x += c.w
	}
	y += 7

	for _, p := range b.Passengers {
		base := y + rowH/2 + 1.3
		status, sc := passengerStatus(p.Status)
		vals := []string{
			fmt.Sprintf("%d", p.Seq),
			p.Name,
			titleCase(p.Type),
			titleCase(p.Gender),
			p.Seat.SeatID,
			status,
			money(p.Fare),
		}
		x := marginX
		for i, c := range cols {
			style, clr := "", cInk
			switch i {
			case 0:
				clr = cMuted
			case 1, 4:
				style = "B"
			case 5:
				style, clr = "B", sc
			}
			v := fit(t, vals[i], style, 9, c.w-6)
			if c.align == "R" {
				t.text(x+c.w-3, base, v, style, 9, clr, "R")
			} else {
				t.text(x+3, base, v, style, 9, clr, "L")
			}
			x += c.w
		}
		y += rowH
		t.draw(cLine)
		t.SetLineWidth(0.25)
		t.Line(marginX, y, pageW-marginX, y)
	}
	return y
}

// total shows the fare breakdown and amount paid on the right and, if seats
// were changed, a note on the left. It returns the y below it.
func (t *ticketPDF) total(b *dto.BookingResponse, y float64) float64 {
	right := pageW - marginX
	top := y
	// Fares are per seat before the service charge; the booking amount includes it.
	var fares int64
	for _, p := range b.Passengers {
		fares += p.Fare.AmountMinor
	}
	if charge := b.Amount.AmountMinor - fares; charge > 0 && fares > 0 {
		for _, l := range [][2]string{
			{"Fare", money(dto.Money{AmountMinor: fares, Currency: b.Amount.Currency})},
			{"Service charge", money(dto.Money{AmountMinor: charge, Currency: b.Amount.Currency})},
		} {
			y += 5
			t.text(right-32, y, l[0], "", 8.5, cMuted, "R")
			t.text(right, y, l[1], "", 8.5, cInk, "R")
		}
		y += 3
		t.draw(cLine)
		t.SetLineWidth(0.25)
		t.Line(right-60, y, right, y)
		y += 1
	}
	t.label(right, y+4, "Total paid", "R")
	t.text(right, y+12, money(b.Amount), "B", 15, cInk, "R")
	if b.Payment != nil {
		t.text(right, y+17.5, "Paid with "+paymentName(b.Payment.Method), "", 8, cMuted, "R")
	}
	if b.SeatsChanged {
		t.fill(cAmberBg)
		t.RoundedRect(marginX, top+1, 100, 15, 2, "1234", "F")
		t.fill(cAmber)
		t.RoundedRect(marginX, top+1, 1.4, 15, 0.7, "14", "F")
		t.text(marginX+5, top+7, "Your seats were changed", "B", 8.5, cAmber, "L")
		t.text(marginX+5, top+12, "Payment arrived after the hold ended, so new seats were given.", "", 7.5, cInk, "L")
	}
	return y + 20
}

func (t *ticketPDF) notes(b *dto.BookingResponse, y float64) {
	const h = 34.0
	if y+h > 270 {
		y = 270 - h
	}
	t.draw(cLine)
	t.SetLineWidth(0.3)
	t.RoundedRect(marginX, y, contW, h, 3, "1234", "D")
	t.text(marginX+7, y+8.5, "Before you travel", "B", 9.5, cInk, "L")
	tips := []string{
		"Carry the NID or passport used for booking. Tickets are checked against it on board.",
		"Be on the platform at least 30 minutes before departure. The train leaves on time.",
		"This ticket is valid only for the passengers, seats and train shown above.",
	}
	for i, s := range tips {
		ty := y + 16 + float64(i)*6
		t.fill(cLivery)
		t.Circle(marginX+8.2, ty-1.1, 0.9, "F")
		t.text(marginX+12, ty, s, "", 8.3, cInk, "L")
	}
}

func (t *ticketPDF) footer(b *dto.BookingResponse, now time.Time) {
	t.hairline(279)
	t.text(marginX, 285, "Issued "+now.In(catalog.Dhaka).Format("02 Jan 2006, 15:04")+" BST  ·  PNR "+b.PNR, "", 7.5, cMuted, "L")
	t.text(pageW-marginX, 285, "Show this ticket on your phone or printed.", "", 7.5, cMuted, "R")
	t.fill(cLivery)
	t.Rect(0, 294, pageW, 3, "F")
}

// qrPayload is what the gate scanner reads: the issued seats of the booking,
// signed so that it cannot be forged or altered (see internal/ticketsig).
func qrPayload(sig *ticketsig.Signer, b *dto.BookingResponse) (string, error) {
	seats := make([]string, 0, len(b.Passengers))
	for _, p := range b.Passengers {
		if p.Status == "ISSUED" {
			seats = append(seats, p.Seat.SeatID)
		}
	}
	return sig.Sign(ticketsig.Payload{PNR: b.PNR, TrainNo: b.TrainNo, RunDate: b.RunDate,
		From: b.From.StationCode, To: b.To.StationCode, Seats: seats})
}

// seatSummary groups seats by coach: "CHA 10, 11 · KHA 4".
func seatSummary(b *dto.BookingResponse) string {
	byCoach := map[string][]int{}
	var coaches []string
	for _, p := range b.Passengers {
		if p.Status == "CANCELLED" || p.Status == "EXPIRED" || p.Status == "FAILED" {
			continue
		}
		if _, ok := byCoach[p.Seat.Coach]; !ok {
			coaches = append(coaches, p.Seat.Coach)
		}
		byCoach[p.Seat.Coach] = append(byCoach[p.Seat.Coach], p.Seat.SeatNo)
	}
	parts := make([]string, 0, len(coaches))
	for _, c := range coaches {
		nos := byCoach[c]
		sort.Ints(nos)
		s := make([]string, len(nos))
		for i, n := range nos {
			s[i] = fmt.Sprint(n)
		}
		parts = append(parts, c+" "+strings.Join(s, ", "))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "  ·  ")
}

func passengerStatus(s string) (string, rgb) {
	switch s {
	case "ISSUED":
		return "Issued", cGreen
	case "CANCELLED":
		return "Cancelled", cRed
	case "HELD":
		return "Held", cAmber
	}
	return titleCase(s), cMuted
}

func paymentName(m string) string {
	switch m {
	case "BKASH":
		return "bKash"
	case "NEXUS":
		return "DBBL Nexus"
	case "VISA", "MASTERCARD", "NAGAD", "ROCKET", "UPAY":
		return titleCase(m)
	}
	return m
}

func money(m dto.Money) string {
	cur := m.Currency
	if cur == "BDT" {
		cur = "Tk"
	}
	whole := fmt.Sprint(m.AmountMinor / 100)
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	if m.AmountMinor%100 != 0 {
		return fmt.Sprintf("%s %s.%02d", cur, whole, m.AmountMinor%100)
	}
	return cur + " " + whole
}

func duration(d time.Duration) string {
	m := int(d.Round(time.Minute).Minutes())
	if m <= 0 {
		return ""
	}
	return fmt.Sprintf("%dh %02dm", m/60, m%60)
}

func runDate(s string) string {
	if d, err := time.Parse("2006-01-02", s); err == nil {
		return d.Format("Mon, 02 Jan 2006")
	}
	return s
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

// fit shortens s with an ellipsis until it fits in w at the given font.
func fit(t *ticketPDF, s, style string, size, w float64) string {
	if t.width(s, style, size) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 1 && t.width(string(r)+"…", style, size) > w {
		r = r[:len(r)-1]
	}
	return strings.TrimSpace(string(r)) + "…"
}
