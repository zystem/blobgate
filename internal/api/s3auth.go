package api

// Who is calling: the signature of a call of the S3 door, and the headers it must cover.

import (
	"errors"
	"net/http"

	"github.com/zystem/blobgate/internal/auth"
)

// s3SecretOf is the secret key of an access key.
func (s *Server) s3SecretOf(ak string) (string, bool) {
	secret, ok := s.cfg.ShardSecrets[ak]
	if !ok || !validShard(ak) {
		return "", false
	}
	return auth.S3Secret(secret), true
}

// s3Verify says who calls: the signature is right, and it covers what decides what the call does. The refusal is written here.
func (s *Server) s3Verify(w http.ResponseWriter, r *http.Request) (*auth.S3Call, bool) {
	call, err := auth.VerifySigV4(r, s.s3SecretOf, s.cfg.Now())
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrNoAuth):
			s3Fail(w, r, 403, "AccessDenied", "the call is not signed (Signature Version 4)")
		case errors.Is(err, auth.ErrUnknownKey), errors.Is(err, auth.ErrBadSig): // one answer: the answer does not say which tenants there are
			s.log.Info("s3: a call was refused", "why", err.Error())
			s3Fail(w, r, 403, "SignatureDoesNotMatch", "the access key or the signature is not right: check the access key (a tenant), the secret key (blobgate s3-key) and the region")
		case errors.Is(err, auth.ErrExpired):
			s3Fail(w, r, 403, "RequestTimeTooSkewed", "the time of the request is not within 15 minutes of the time of the service")
		default:
			s3Fail(w, r, 400, "AuthorizationHeaderMalformed", err.Error())
		}
		return nil, false
	}
	if err := call.CheckSigned(r); err != nil {
		s3Fail(w, r, 403, "AccessDenied", err.Error()+": sign it (S3 refuses such a call too)")
		return nil, false
	}
	return call, true
}
