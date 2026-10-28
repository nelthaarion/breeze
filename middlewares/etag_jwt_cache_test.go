package middleware

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/nelthaarion/breeze/v2"
)

func TestETagCacheIsBoundedAndWritesNothingWhenUnchanged(t *testing.T) {
	c := NewETagCache()
	for i := 0; i < 100000; i++ {
		c.put(fmt.Sprintf("/u/%d", i), "e")
	}
	if n := c.entries(); n > etagShards*etagShardMax {
		t.Fatalf("%d entries, bound is %d", n, etagShards*etagShardMax)
	}
	c.put("/a", "x")
	if v, ok := c.get("/a"); !ok || v != "x" {
		t.Fatalf("get after put: %q %v", v, ok)
	}
	if n := testing.AllocsPerRun(500, func() { c.put("/a", "x") }); n != 0 {
		t.Fatalf("re-putting the same ETag allocates %v times", n)
	}
	c.put("/a", "y")
	if v, _ := c.get("/a"); v != "y" {
		t.Fatalf("update lost: %q", v)
	}
}

func TestETagCacheConcurrent(t *testing.T) {
	c := NewETagCache()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 5000; i++ {
				k := fmt.Sprintf("/k/%d", (i*7+g)%900)
				c.put(k, fmt.Sprint(i%3))
				c.get(k)
			}
		}(g)
	}
	wg.Wait()
}

func TestJWTCacheVerifiedTokensExpiryIsolationAndBound(t *testing.T) {
	const secret = "unit-test-secret-unit-test-secret-1234"
	tok, err := GenerateJWT(secret, jwt.MapClaims{"user_id": "7", "role": "admin", "perms": []any{"a", "b"}}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	claims, ok := validateJWTType(tok, secret, jwt.SigningMethodHS256, "access")
	if !ok {
		t.Fatal("token should verify")
	}
	c := newJWTCache(64)
	if _, hit := c.lookup(tok); hit {
		t.Fatal("hit before store")
	}
	c.store(tok, claims)
	got, hit := c.lookup(tok)
	if !hit || !reflect.DeepEqual(got, claims) {
		t.Fatalf("lookup: hit=%v got=%v want=%v", hit, got, claims)
	}
	got["role"] = "root"
	got["perms"].([]any)[0] = "zzz"
	again, _ := c.lookup(tok)
	if again["role"] != "admin" || again["perms"].([]any)[0] != "a" {
		t.Fatalf("a handler's edits leaked into the cache: %v", again)
	}

	jwtNow = func() time.Time { return time.Now().Add(2 * time.Hour) }
	defer func() { jwtNow = time.Now }()
	if _, hit := c.lookup(tok); hit {
		t.Fatal("expired token served from the cache")
	}
	jwtNow = time.Now

	for i := 0; i < 2000; i++ {
		c.store(fmt.Sprintf("tok-%d", i), claims)
	}
	if n := c.entries(); n > 64+jwtCacheShards {
		t.Fatalf("%d entries for a size-64 cache", n)
	}
}

func jwtRun(t *testing.T, mw breeze.HandlerFunc, token string) (status int, ran bool, claims jwt.MapClaims) {
	ctx := breeze.NewContext(breeze.GET, "/x")
	ctx.Req.Header["authorization"] = "Bearer " + token
	ctx.SetMiddlewareChain([]breeze.HandlerFunc{mw}, func(c *breeze.Context) error {
		ran = true
		v, _ := c.Get("user")
		claims, _ = v.(jwt.MapClaims)
		return c.WriteString("ok")
	})
	if err := ctx.Next(); err != nil {
		t.Fatal(err)
	}
	return ctx.Res.Status, ran, claims
}

func TestJWTMiddlewareWithCacheKeepsRejecting(t *testing.T) {
	const secret = "unit-test-secret-unit-test-secret-1234"
	good, _ := GenerateJWT(secret, jwt.MapClaims{"user_id": "7", "role": "admin"}, time.Hour, nil)
	expired, _ := GenerateJWT(secret, jwt.MapClaims{"user_id": "7", "role": "admin"}, -time.Hour, nil)
	wrong, _ := GenerateJWT("another-secret-another-secret-another", jwt.MapClaims{"user_id": "7", "role": "admin"}, time.Hour, nil)
	for _, size := range []int{0, 32} {
		mw := JWTAuthMiddleware(JWTOptions{AccessSecret: secret, RequiredRoles: []string{"admin"}, CacheSize: size})
		for round := 0; round < 3; round++ { // rounds 2 and 3 are cache hits when size > 0
			if st, ran, cl := jwtRun(t, mw, good); st == 401 || !ran || cl["user_id"] != "7" {
				t.Fatalf("size %d round %d: good token rejected (status %d ran %v claims %v)", size, round, st, ran, cl)
			}
			for name, bad := range map[string]string{"expired": expired, "wrong key": wrong, "garbage": "a.b.c", "tampered": good[:len(good)-2] + "xx"} {
				if st, ran, _ := jwtRun(t, mw, bad); st != 401 || ran {
					t.Fatalf("size %d round %d: %s token accepted (status %d ran %v)", size, round, name, st, ran)
				}
			}
		}
	}
}

func TestJWTCacheConcurrent(t *testing.T) {
	c := newJWTCache(64)
	claims := jwt.MapClaims{"user_id": "1", "exp": float64(time.Now().Add(time.Hour).Unix()), "perms": []any{"a"}}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 3000; i++ {
				tok := fmt.Sprintf("t-%d", (i*7+g)%200)
				c.store(tok, claims)
				if got, ok := c.lookup(tok); ok {
					got["x"] = i // edits to a returned copy must stay private
					got["perms"].([]any)[0] = "z"
				}
			}
		}(g)
	}
	wg.Wait()
	if claims["perms"].([]any)[0] != "a" || claims["x"] != nil {
		t.Fatal("the stored claims were modified through a returned copy")
	}
}
