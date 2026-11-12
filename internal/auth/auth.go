// Package auth makes and checks the tokens of the storage API: the core's per-call token (v1) and the shim's grant (g1).
// The strings are fixed byte for byte by the Nim module (testdata/vectors.json is made by its code).
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strconv"
	"strings"
)

const (
	TokenLifetime = 120         // seconds a v1 token is good for
	ClockSkew     = 30          // seconds a token may be from the future
	GrantLifetime = 2 * 60 * 60 // seconds a grant is good for
)

func mac(secret, msg string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(msg))
	return hex.EncodeToString(h.Sum(nil))
}

func eq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func tokenMAC(secret, shard, method, target string, expires int64) string {
	return mac(secret, "cinim/storage/v1|"+strconv.Itoa(len(shard))+"|"+shard+"|"+strings.ToUpper(method)+"|"+
		strconv.Itoa(len(target))+"|"+target+"|"+strconv.FormatInt(expires, 10))
}

// SignTarget returns the Authorization value (without "Bearer ") of a call for the shard.
func SignTarget(secret, shard, method, target string, now int64) string {
	exp := now + TokenLifetime
	return "v1." + strconv.FormatInt(exp, 10) + "." + tokenMAC(secret, shard, method, target, exp)
}

// parseExpiry reads the expiry field: digits only, at most 12 characters (a sign or a space could never match a token the core made).
func parseExpiry(s string) (int64, bool) {
	if len(s) == 0 || len(s) > 12 {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// VerifyTarget checks a v1 token for the shard, method and target (the decoded path plus the query as sent).
func VerifyTarget(secret, shard, method, target, token string, now int64) bool {
	if secret == "" || shard == "" {
		return false
	}
	p := strings.Split(token, ".")
	if len(p) != 3 || p[0] != "v1" {
		return false
	}
	exp, ok := parseExpiry(p[1])
	if !ok || exp < now || exp > now+TokenLifetime+ClockSkew {
		return false
	}
	return eq(p[2], tokenMAC(secret, shard, method, target, exp))
}

func grantMAC(secret, shard, store, key, upload, mode string, expires int64) string {
	msg := "cinim/storage/grant/v1"
	for _, f := range []string{shard, store, key, upload, mode} {
		msg += "|" + strconv.Itoa(len(f)) + "|" + f
	}
	return mac(secret, msg+"|"+strconv.FormatInt(expires, 10))
}

// SignGrant makes a grant for one upload (mode "w", upload its id) or one object read (mode "r", no upload).
func SignGrant(secret, shard, store, key, upload, mode string, now int64) string {
	exp := now + GrantLifetime
	return "g1." + shard + "." + strconv.FormatInt(exp, 10) + "." + grantMAC(secret, shard, store, key, upload, mode, exp)
}

// GrantShard is the shard a grant names ("" for something that is not a grant): the module needs it to find the secret that checks it.
func GrantShard(token string) string {
	p := strings.Split(token, ".")
	if len(p) == 4 && p[0] == "g1" {
		return p[1]
	}
	return ""
}

// VerifyGrant checks a grant against the shard, store, key, upload and mode of the call.
func VerifyGrant(secret, shard, store, key, upload, mode, token string, now int64) bool {
	if secret == "" || shard == "" || store == "" || key == "" || (mode != "w" && mode != "r") {
		return false
	}
	p := strings.Split(token, ".")
	if len(p) != 4 || p[0] != "g1" || p[1] != shard {
		return false
	}
	exp, ok := parseExpiry(p[2])
	if !ok || exp < now || exp > now+GrantLifetime+ClockSkew {
		return false
	}
	return eq(p[3], grantMAC(secret, shard, store, key, upload, mode, exp))
}
