// cookiepin 是 Codex LB Cookie Pin 插件进程入口。
// 实际逻辑在 internal/transport 与 internal/cookiestore，入口只做组装。
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/evidence"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/transport"
	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	journal, err := evidence.Open(os.Getenv("SUB2API_PLUGIN_STATE_DIR"))
	if err != nil {
		slog.Error("plugin_evidence_unavailable")
		os.Exit(1)
	}
	if len(os.Args) > 1 && os.Args[1] == "reconcile" {
		flags := flag.NewFlagSet("reconcile", flag.ExitOnError)
		account := flags.Int64("account", 0, "Account ID")
		operation := flags.String("operation", "", "Exact unknown operation ID")
		ack := flags.Bool("acknowledge-unknown", false, "Acknowledge manual reconciliation; never replays the request")
		_ = flags.Parse(os.Args[2:])
		if !*ack || *account <= 0 || *operation == "" || flags.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "reconciliation requires account, exact operation and explicit acknowledgement")
			os.Exit(2)
		}
		if err := journal.Reconcile(*account, *operation, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "reconciliation rejected; preserve state and recheck the current operation")
			os.Exit(1)
		}
		fmt.Println("unknown acknowledged; counters and cooldown preserved; no request replayed")
		return
	}
	pluginv1.Serve(transport.NewWithEvidence(cookiestore.New(), journal))
}
