package api

import "regexp"

// The words of a failure of the database or of a node name hosts, paths, node ids and the layout of the objects. A caller is told what kind of failure it was and a reference; the whole text goes to
// the log, where the reference finds it.

var (
	urlUser  = regexp.MustCompile(`(://)[^/\s"@]+@`)
	urlToken = regexp.MustCompile(`(?i)((?:x-amz-signature|x-amz-credential|x-amz-security-token|password|passwd|pass|token|secret|sig)=)[^&\s"]+`)
)

// redact takes out of a text what must not be in a log either: the user and password of a URL, and the secrets of its query.
func redact(s string) string {
	return urlToken.ReplaceAllString(urlUser.ReplaceAllString(s, "${1}***@"), "${1}***")
}

// hide logs err and gives back the words for the caller, which end with the reference of that line of the log.
func (s *Server) hide(words string, err error) string {
	ref := newID()
	s.log.Warn("a call failed", "ref", ref, "answer", words, "error", redact(err.Error()))
	return words + " (ref " + ref + ")"
}
