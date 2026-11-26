package api

import "testing"

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		`Get "https://user:hunter2@nas.example/dav/k": EOF`:      `Get "https://***@nas.example/dav/k": EOF`,
		`GET http://s3/b/k?X-Amz-Signature=abc123&x=1 refused`:   `GET http://s3/b/k?X-Amz-Signature=***&x=1 refused`,
		`dial tcp 10.0.0.5:22: connect: connection refused`:      `dial tcp 10.0.0.5:22: connect: connection refused`,
		`ftp://admin@10.0.0.5/data?password=p:w&Token=t timeout`: `ftp://***@10.0.0.5/data?password=***&Token=*** timeout`,
	} {
		if got := redact(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
