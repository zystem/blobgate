package auth

import (
	"encoding/json"
	"os"
	"testing"
)

type vec struct {
	Token []struct {
		Secret, Shard, Method, Target, Token string
		Now                                  int64
	}
	Grant []struct {
		Secret, Shard, Store, Key, Upload, Mode, Token string
		Now                                            int64
	}
}

func load(t *testing.T) vec {
	t.Helper()
	b, err := os.ReadFile("../../testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vec
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The strings must equal those of the Nim module byte for byte (STO-006 contract, D-60).
func TestTokenMatchesNimVectors(t *testing.T) {
	for _, x := range load(t).Token {
		if got := SignTarget(x.Secret, x.Shard, x.Method, x.Target, x.Now); got != x.Token {
			t.Errorf("%s %s: got %s want %s", x.Method, x.Target, got, x.Token)
		}
		if !VerifyTarget(x.Secret, x.Shard, x.Method, x.Target, x.Token, x.Now+10) {
			t.Errorf("%s %s: a token the Nim module made is refused", x.Method, x.Target)
		}
	}
}

func TestTokenRefusals(t *testing.T) {
	x := load(t).Token[0]
	ok := func(secret, shard, method, target, tok string, now int64) bool {
		return VerifyTarget(secret, shard, method, target, tok, now)
	}
	if !ok(x.Secret, x.Shard, x.Method, x.Target, x.Token, x.Now) {
		t.Fatal("good token refused")
	}
	cases := map[string]bool{
		"other method": ok(x.Secret, x.Shard, "GET", x.Target, x.Token, x.Now),
		"other target": ok(x.Secret, x.Shard, x.Method, x.Target+"?stat", x.Token, x.Now),
		"other shard":  ok(x.Secret, "002", x.Method, x.Target, x.Token, x.Now),
		"other secret": ok("x", x.Shard, x.Method, x.Target, x.Token, x.Now),
		"expired":      ok(x.Secret, x.Shard, x.Method, x.Target, x.Token, x.Now+TokenLifetime+1),
		"future":       ok(x.Secret, x.Shard, x.Method, x.Target, x.Token, x.Now-ClockSkew-1),
		"empty":        ok(x.Secret, x.Shard, x.Method, x.Target, "", x.Now),
		"junk":         ok(x.Secret, x.Shard, x.Method, x.Target, "v1.1.2.3", x.Now),
		"empty secret": ok("", x.Shard, x.Method, x.Target, x.Token, x.Now),
	}
	for n, got := range cases {
		if got {
			t.Errorf("%s: accepted", n)
		}
	}
	if !ok(x.Secret, x.Shard, "put", x.Target, x.Token, x.Now) {
		t.Error("the method is not case sensitive in the Nim module")
	}
}

func TestGrantMatchesNimVectors(t *testing.T) {
	for _, x := range load(t).Grant {
		if got := SignGrant(x.Secret, x.Shard, x.Store, x.Key, x.Upload, x.Mode, x.Now); got != x.Token {
			t.Errorf("%s: got %s want %s", x.Mode, got, x.Token)
		}
		if GrantShard(x.Token) != x.Shard {
			t.Error("grant shard")
		}
		if !VerifyGrant(x.Secret, x.Shard, x.Store, x.Key, x.Upload, x.Mode, x.Token, x.Now+60) {
			t.Error("grant refused")
		}
		if VerifyGrant(x.Secret, x.Shard, x.Store, x.Key+"x", x.Upload, x.Mode, x.Token, x.Now) {
			t.Error("grant for another key accepted")
		}
		if VerifyGrant(x.Secret, x.Shard, "other", x.Key, x.Upload, x.Mode, x.Token, x.Now) {
			t.Error("grant for another store accepted")
		}
		if VerifyGrant(x.Secret, x.Shard, x.Store, x.Key, x.Upload, x.Mode, x.Token, x.Now+GrantLifetime+1) {
			t.Error("expired grant accepted")
		}
	}
	if GrantShard("v1.1.2") != "" {
		t.Error("not a grant")
	}
}

func FuzzParse(f *testing.F) {
	f.Add("v1.1700000120.abc", "g1.001.1.x")
	f.Fuzz(func(t *testing.T, tok, g string) {
		VerifyTarget("s", "001", "GET", "/x", tok, 1700000000)
		VerifyGrant("s", "001", "m", "k", "", "r", g, 1700000000)
		GrantShard(g)
	})
}
