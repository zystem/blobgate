// Package auth makes and checks the tokens of the API: the per-call token of a caller (v2) and the grant for one upload or one object (g2). The tokens of the first version of the program (v1 and g1, made
// under the domain "cinim/storage" and the name "shard") are still accepted, byte for byte as the Nim module of CiNim made them (testdata/vectors.json is made by its code); they are no longer made.
// A tenant is what the first version called a shard.
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

func tokenMAC(domain, secret, shard, method, target string, expires int64) string {
	return mac(secret, domain+"|"+strconv.Itoa(len(shard))+"|"+shard+"|"+strings.ToUpper(method)+"|"+
		strconv.Itoa(len(target))+"|"+target+"|"+strconv.FormatInt(expires, 10))
}

const (
	domainCallV1  = "cinim/storage/v1"
	domainCallV2  = "blobgate/call/v2"
	domainGrantV1 = "cinim/storage/grant/v1"
	domainGrantV2 = "blobgate/grant/v2"
)

// SignTarget returns the Authorization value (without "Bearer ") of a call for the tenant.
func SignTarget(secret, tenant, method, target string, now int64) string {
	return signTarget("v2", domainCallV2, secret, tenant, method, target, now)
}

// SignTargetV1 makes a token of the first version, which the module of CiNim made (kept for the vectors and for callers that were not changed).
func SignTargetV1(secret, shard, method, target string, now int64) string {
	return signTarget("v1", domainCallV1, secret, shard, method, target, now)
}

func signTarget(version, domain, secret, tenant, method, target string, now int64) string {
	exp := now + TokenLifetime
	return version + "." + strconv.FormatInt(exp, 10) + "." + tokenMAC(domain, secret, tenant, method, target, exp)
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

// VerifyTarget checks a token (v2, or v1 of the first version) for the tenant, method and target (the decoded path plus the query as sent).
func VerifyTarget(secret, shard, method, target, token string, now int64) bool {
	if secret == "" || shard == "" {
		return false
	}
	p := strings.Split(token, ".")
	if len(p) != 3 {
		return false
	}
	domain := ""
	switch p[0] {
	case "v2":
		domain = domainCallV2
	case "v1":
		domain = domainCallV1
	default:
		return false
	}
	exp, ok := parseExpiry(p[1])
	if !ok || exp < now || exp > now+TokenLifetime+ClockSkew {
		return false
	}
	return eq(p[2], tokenMAC(domain, secret, shard, method, target, exp))
}

func grantMAC(domain, secret, shard, store, key, upload, mode string, expires int64) string {
	msg := domain
	for _, f := range []string{shard, store, key, upload, mode} {
		msg += "|" + strconv.Itoa(len(f)) + "|" + f
	}
	return mac(secret, msg+"|"+strconv.FormatInt(expires, 10))
}

// SignGrant makes a grant for one upload (mode "w", upload its id) or one object read (mode "r", no upload).
func SignGrant(secret, tenant, store, key, upload, mode string, now int64) string {
	return signGrant("g2", domainGrantV2, secret, tenant, store, key, upload, mode, now)
}

// SignGrantV1 makes a grant of the first version (kept for the vectors and for callers that were not changed).
func SignGrantV1(secret, shard, store, key, upload, mode string, now int64) string {
	return signGrant("g1", domainGrantV1, secret, shard, store, key, upload, mode, now)
}

func signGrant(version, domain, secret, tenant, store, key, upload, mode string, now int64) string {
	exp := now + GrantLifetime
	return version + "." + tenant + "." + strconv.FormatInt(exp, 10) + "." + grantMAC(domain, secret, tenant, store, key, upload, mode, exp)
}

// GrantTenant is the tenant a grant names ("" for something that is not a grant): the module needs it to find the secret that checks it.
func GrantTenant(token string) string {
	p := strings.Split(token, ".")
	if len(p) == 4 && (p[0] == "g2" || p[0] == "g1") {
		return p[1]
	}
	return ""
}

// ValidTenant says whether this may be the name of a tenant: 1..64 of lowercase letters, digits, `-` and `_`, not starting with `-`. The shards of the first version (digits only) are tenants.
func ValidTenant(s string) bool {
	if len(s) < 1 || len(s) > 64 || s[0] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// VerifyGrant checks a grant against the shard, store, key, upload and mode of the call.
func VerifyGrant(secret, shard, store, key, upload, mode, token string, now int64) bool {
	if secret == "" || shard == "" || store == "" || key == "" || (mode != "w" && mode != "r") {
		return false
	}
	p := strings.Split(token, ".")
	if len(p) != 4 || p[1] != shard {
		return false
	}
	domain := ""
	switch p[0] {
	case "g2":
		domain = domainGrantV2
	case "g1":
		domain = domainGrantV1
	default:
		return false
	}
	exp, ok := parseExpiry(p[2])
	if !ok || exp < now || exp > now+GrantLifetime+ClockSkew {
		return false
	}
	return eq(p[3], grantMAC(domain, secret, shard, store, key, upload, mode, exp))
}
