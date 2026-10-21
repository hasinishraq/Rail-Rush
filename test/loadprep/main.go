// Command loadprep creates load-test users and their tokens for test/load.
//
// Logging in thousands of users through the API is slow by design (argon2id)
// and rate-limited per IP, and the local waiting room admits ADMISSION_RATE
// users per second. For a load test that is the wrong bottleneck, so this tool
// inserts users straight into Postgres (one shared password hash) and signs
// their access and admission tokens with the stack's own keys:
//
//	docker cp railway-application-auth-1:/keys ./test/load/keys
//	go run ./test/loadprep --users 1000 --out test/load/users.json
//
// Users get phones +880199XXXXXXX so they are easy to recognise. Local stacks
// only: it needs the private signing keys.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"

	"railway/internal/admission"
	"railway/internal/auth"
	"railway/internal/platform"
)

var (
	pgURL    = flag.String("pg", os.Getenv("PG_URL"), "Postgres URL (default $PG_URL)")
	users    = flag.Int("users", 1000, "number of load-test users")
	password = flag.String("password", "load-test-password", "password of every load-test user")
	jwtKey   = flag.String("jwt-key", "test/load/keys/dev-es256.pem", "ES256 key that signs access tokens (JWT_SIGNING_KEY_FILE)")
	jwtKID   = flag.String("kid", "dev1", "JWT key ID (JWT_KID)")
	admKey   = flag.String("adm-key", "test/load/keys/admission-es256.pem", "ES256 key that signs admission tokens (ADMISSION_KEY_FILE)")
	ttl      = flag.Duration("ttl", 3*time.Hour, "lifetime of the generated tokens")
	out      = flag.String("out", "test/load/users.json", "output file for k6")
)

type user struct {
	UserID    int64  `json:"user_id"`
	Phone     string `json:"phone"`
	Token     string `json:"token"`
	Admission string `json:"admission"`
}

func main() {
	flag.Parse()
	ctx := context.Background()
	if *pgURL == "" {
		log.Fatal("--pg or PG_URL is required")
	}
	if *users < 1 || *users > 9_999_999 {
		log.Fatal("--users must be between 1 and 9,999,999")
	}

	signKey, err := auth.LoadECKey(*jwtKey)
	if err != nil {
		log.Fatalf("jwt key: %v (copy the keys first: docker cp railway-application-auth-1:/keys ./test/load/keys)", err)
	}
	admissionKey, err := auth.LoadECKey(*admKey)
	if err != nil {
		log.Fatalf("admission key: %v", err)
	}

	db, err := pgx.Connect(ctx, *pgURL)
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer db.Close(ctx)

	// One argon2id hash for everyone: hashing 1,000 passwords would take a minute for nothing.
	hash, err := auth.NewHasher(65536, 1, 2).Hash(*password)
	if err != nil {
		log.Fatal(err)
	}
	phones := make([]string, *users)
	names := make([]string, *users)
	for i := range phones {
		phones[i] = fmt.Sprintf("+880199%07d", i)
		names[i] = fmt.Sprintf("Load User %d", i)
	}
	tag, err := db.Exec(ctx, `
		INSERT INTO app_user (phone, name, password_hash)
		SELECT p, n, $3 FROM unnest($1::text[], $2::text[]) AS t(p, n)
		ON CONFLICT (phone) DO NOTHING`, phones, names, hash)
	if err != nil {
		log.Fatalf("insert users: %v", err)
	}
	log.Printf("inserted %d new users (%d requested; existing ones are reused)", tag.RowsAffected(), *users)

	rows, err := db.Query(ctx, `SELECT user_id, phone FROM app_user WHERE phone = ANY($1) ORDER BY phone`, phones)
	if err != nil {
		log.Fatal(err)
	}
	signer := auth.NewSigner(*jwtKID, signKey, *ttl)
	now := time.Now()
	var list []user
	for rows.Next() {
		var u user
		if err := rows.Scan(&u.UserID, &u.Phone); err != nil {
			log.Fatal(err)
		}
		if u.Token, _, err = signer.Sign(u.UserID, "passenger", false, "load-test", now); err != nil {
			log.Fatal(err)
		}
		// The same claims the waiting room issues (internal/admission), one per user.
		c := admission.Claims{RegisteredClaims: jwt.RegisteredClaims{
			Issuer: admission.Issuer, Audience: jwt.ClaimStrings{admission.Audience},
			Subject: strconv.FormatInt(u.UserID, 10), IssuedAt: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(*ttl)), ID: platform.NewRequestID(),
		}, Scope: admission.Scope}
		if u.Admission, err = jwt.NewWithClaims(jwt.SigningMethodES256, c).SignedString(admissionKey); err != nil {
			log.Fatal(err)
		}
		list = append(list, u)
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}

	b, _ := json.Marshal(list)
	// 0644: the k6 container reads it as a non-root user. The tokens are local test credentials.
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %d users with tokens valid for %s to %s", len(list), *ttl, *out)
}
