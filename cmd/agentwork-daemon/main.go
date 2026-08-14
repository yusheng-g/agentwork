// agentwork-daemon — task-driven multi-runtime agent management platform.
//
// Single binary: HTTP server + embedded daemon. MVP runs everything in one
// process. The agent-side CLI (agentwork-cli) is a separate binary; the daemon
// injects its directory into the agent subprocess PATH so the agent can call
// back.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/eushing/agentwork/internal/daemon"
	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/notify"
	"github.com/eushing/agentwork/internal/proto"
	"github.com/eushing/agentwork/internal/proto/acpbackend"
	"github.com/eushing/agentwork/internal/proto/jsonlbackend"
	"github.com/eushing/agentwork/internal/proto/jsonrpcbackend"
	"github.com/eushing/agentwork/internal/server"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
)

var version = "dev"

func main() {
	versionFlag := flag.Bool("version", false, "print version and exit")
	addr := flag.String("addr", ":7373", "HTTP listen address")
	dbPath := flag.String("db", "", "SQLite path (default ~/.agentwork/agentwork.db)")
	flag.Parse()

	if *versionFlag {
		fmt.Println(version)
		os.Exit(0)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	bus := events.NewBus()

	// IM connection (DESIGN.md decision 2-14): the Web UI drives the Feishu
	// connect flow — one-click app registration via QR scan, then the first
	// bot message captures the receive target. Credentials persist to SQLite;
	// the daemon auto-reconnects on startup. No environment configuration.
	settingsSvc := service.NewSettingsService(st)
	qs := notify.NewSQLQueryStore(st) // M3: card evidence / digest / intake queries
	imConn := notify.NewConnector(settingsSvc, bus)
	imConn.SetQueryStore(qs)
	go func() {
		if err := imConn.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("notify: feishu connector ended: %v", err)
		}
	}()

	// Services. wired together explicitly to avoid a constructor-order cycle:
	// GoalService <-> RunService hold cross-references (reconcile enqueues
	// retries/wakes; run finish delegates to reconcile).
	goalSvc := service.NewGoalService(st, bus)
	runSvc := service.NewRunService(st, bus)
	commentSvc := service.NewCommentService(st, bus)
	agentSvc := service.NewAgentService(st, bus)
	goalSvc.SetRunService(runSvc)
	runSvc.SetGoalService(goalSvc)
	commentSvc.SetRunService(runSvc)
	commentSvc.SetGoalService(goalSvc)

	squadSvc := service.NewSquadService(st, bus)
	schedSvc := service.NewScheduleService(st, bus)
	domainSvc := service.NewDomainService(st, bus)
	domainSvc.SetRunService(runSvc)

	// M3 IM: the approval-card callbacks resolve through the goal layer; the
	// owner's inbound messages become intake parse runs on the configured
	// global parser agent (app_settings platform.intake_agent).
	imConn.SetGoalService(goalSvc)
	intakeSvc := notify.NewIntakeService(qs, settingsSvc, runSvc)
	imConn.SetIntakeService(intakeSvc)

	// Protocol backends registered by provider name (runtime.provider selects).
	protoReg := proto.NewRegistry()
	protoReg.Register("acp", acpbackend.New())
	protoReg.Register("jsonl", jsonlbackend.New())
	protoReg.Register("jsonrpc", jsonrpcbackend.New())

	d := daemon.New(st, bus, *addr, protoReg, goalSvc, runSvc, commentSvc, agentSvc, squadSvc, schedSvc, imConn, qs, intakeSvc)
	go func() {
		if err := d.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("daemon: %v", err)
		}
	}()

	srv := server.New(st, bus, d, goalSvc, runSvc, commentSvc, squadSvc, schedSvc, domainSvc, imConn)
	if err := srv.ListenAndServe(ctx, *addr); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("server: %v", err)
	}
}