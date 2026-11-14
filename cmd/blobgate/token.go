package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zystem/blobgate/internal/auth"
)

// #nosec G101 -- the usage text of a command that makes tokens, not a credential
const tokenUsage = `usage: blobgate token <tenant> <secret> <METHOD> <target>

Prints the token of a call of the core (valid two minutes), to try the API with curl. <target> is the
decoded path, and the query as it will be sent, for example
  /v1/stores/_default/objects/acme/app.txt
  /v1/stores/main/objects/acme/app.txt?stat
Send it as "Authorization: Bearer <token>" with "X-Blobgate-Tenant: <tenant>".`

func tokenCommand(args []string) int {
	if len(args) != 4 {
		fmt.Fprintln(os.Stderr, tokenUsage)
		return 2
	}
	fmt.Println(auth.SignTarget(args[1], args[0], strings.ToUpper(args[2]), args[3], time.Now().Unix()))
	return 0
}

// #nosec G101 -- the usage text of a command that makes an S3 key pair, not a credential
const s3KeyUsage = `usage: blobgate s3-key <tenant> <secret>

Prints the access key and the secret key that a tool of S3 (aws cli, rclone, mc, an SDK) uses with the S3 door
(BLOBGATE_S3_PORT) as the tenant <tenant>, whose secret in BLOBGATE_TENANTS is <secret>. The access key is the name of
the tenant; the secret key is made from the secret and does not give it away.`

func s3KeyCommand(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, s3KeyUsage)
		return 2
	}
	fmt.Printf("access key: %s\nsecret key: %s\n", args[0], auth.S3Secret(args[1]))
	return 0
}
