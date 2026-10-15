// tetherd is a durable task and action ledger in front of E2B sandboxes.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/iamanishx/tether/internal/api"
	"github.com/iamanishx/tether/internal/sandbox"
	"github.com/iamanishx/tether/internal/store"
	"github.com/iamanishx/tether/internal/task"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "HTTP listen address")
	dataDir := flag.String("data-dir", "./data", "directory for durable task state (one owning process)")
	template := flag.String("template", "code-interpreter-v1", "E2B template id or alias")
	ttl := flag.Int("sandbox-ttl", 300, "seconds of idleness before E2B auto-pauses a sandbox")
	execTimeout := flag.Duration("exec-timeout", 2*time.Minute, "time budget for a single action")
	maxAttempts := flag.Int("max-attempts", 3, "max executions per action (persisted across restarts)")
	maxActions := flag.Int("max-actions", 200, "max actions per task")
	noAuth := flag.Bool("no-auth", false, "disable bearer-token auth (local development only)")
	flag.Parse()

	e2bKey := os.Getenv("E2B_API_KEY")
	if e2bKey == "" {
		log.Fatal("E2B_API_KEY is required")
	}
	token := os.Getenv("TETHER_API_TOKEN")
	if token == "" && !*noAuth {
		log.Fatal("TETHER_API_TOKEN is required (or pass -no-auth for local development)")
	}

	st, err := store.Open(*dataDir)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	eng := task.NewEngine(st, sandbox.NewE2B(e2bKey, *template), task.Config{
		SandboxTTLSec: *ttl, ExecTimeout: *execTimeout, MaxAttempts: *maxAttempts, MaxActions: *maxActions,
	})

	// Reclassify anything that was in flight when the last process stopped
	// BEFORE accepting requests; idempotent actions are replayed in the background.
	replays, err := eng.Recover()
	if err != nil {
		log.Fatalf("recovery failed: %v", err)
	}
	log.Printf("recovery complete: %d idempotent action(s) to replay", len(replays))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go eng.RunReplays(ctx, replays)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           api.New(eng, token).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("tetherd listening on %s (data: %s)", *listen, *dataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down; waiting for in-flight actions to be recorded")
	sctx, cancel := context.WithTimeout(context.Background(), *execTimeout+10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
