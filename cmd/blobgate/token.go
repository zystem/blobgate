package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zystem/blobgate/internal/auth"
)

const tokenUsage = `usage: blobgate token <shard> <secret> <METHOD> <target>

Prints the token of a call of the core (valid two minutes), to try the API with curl. <target> is the
decoded path, and the query as it will be sent, for example
  /v1/stores/_default/objects/acme/app.txt
  /v1/stores/main/objects/acme/app.txt?stat
Send it as "Authorization: Bearer <token>" with "X-Cinim-Shard: <shard>".`

func tokenCommand(args []string) int {
	if len(args) != 4 {
		fmt.Fprintln(os.Stderr, tokenUsage)
		return 2
	}
	fmt.Println(auth.SignTarget(args[1], args[0], strings.ToUpper(args[2]), args[3], time.Now().Unix()))
	return 0
}
