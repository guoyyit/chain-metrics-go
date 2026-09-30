// Command metricsd runs the local on-chain token metrics service.
//
// It is intentionally minimal: state lives under --data-dir. Blocks
// are ingested and snapshots queried through the service package in
// embedded/test use; this binary demonstrates configuration and
// lifecycle.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/guoyyit/chain-metrics-go/domain"
	"github.com/guoyyit/chain-metrics-go/service"
)

func main() {
	dataDir := flag.String("data-dir", "./metrics-data", "state directory")
	window := flag.Uint64("window-blocks", 64, "rolling window size in blocks")
	topK := flag.Int("top-holders", 10, "top holder ranking size")
	history := flag.Uint64("history-blocks", 10_000, "retained snapshot history in blocks")
	flag.Parse()

	cfg := domain.DefaultConfig()
	cfg.DataDir = *dataDir
	cfg.ActiveWindowBlocks = *window
	cfg.TopHolderCount = *topK
	cfg.HistoryRetainBlocks = *history

	svc, err := service.Open(cfg)
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer svc.Close()

	head := svc.Head()
	fmt.Fprintf(os.Stdout, "metricsd ready, data=%s head=%d %s\n",
		cfg.DataDir, head.Height, head.Hash)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	fmt.Fprintln(os.Stdout, "shutting down")
}
