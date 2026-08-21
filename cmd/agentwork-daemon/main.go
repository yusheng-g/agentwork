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
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/eushing/agentwork/internal/daemon"
	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/notify"
	"github.com/eushing/agentwork/internal/proto"
	"github.com/eushing/agentwork/internal/proto/acpbackend"
	"github.com/eushing/agentwork/internal/proto/jsonlbackend"
	"github.com/eushing/agentwork/internal/proto/jsonrpcbackend"
	"github.com/eushing/agentwork/internal/server"
	"github.com/eushing/agentwork/internal/service"
	"github.com/eushing/agentwork/internal/store"
	"gopkg.in/natefinch/lumberjack.v2"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "version" {
		fmt.Fprintf(os.Stderr, "agentwork-daemon v%s\n", daemon.DaemonVersion)
		return
	}
	addr := flag.String("addr", ":7373", "HTTP listen address")
	dbPath := flag.String("db", "", "SQLite path (default ~/.agentwork/agentwork.db)")
	flag.Parse()

	st, err := store.Open(*dbPath)
	if err != nil {
		logging.Fatalf("store: %v", err)
	}
	defer st.Close()

	// slog + lumberjack: the daemon's structured logger writes its compact
	// line format (RFC3339 [LEVEL] msg — the tailer and /logs parse it) to
	// BOTH stdout and ~/.agentwork/daemon.log (10MB size rotation, 3
	// compressed backups). The runtime level is one atomic knob — the Web
	// panel's PUT /logs/level — and persists in app_settings.
	lj := &lumberjack.Logger{Filename: logging.DefaultPath(), MaxSize: 10, MaxBackups: 3, Compress: true}
	slog.SetDefault(slog.New(logging.NewHandler(io.MultiWriter(os.Stdout, lj))))
	if raw, err := service.NewSettingsService(st).Get(context.Background(), "logging.level"); err == nil && raw != "" {
		logging.SetLevel(logging.ParseLevel(raw))
		logging.Infof("logging: level restored to %s", logging.GetLevel())
	}

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
			logging.Errorf("notify: feishu connector ended: %v", err)
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

	skillSvc := service.NewSkillService(st)
	teamImportSvc := service.NewTeamImportService(st, bus)
	teamImportSvc.SetDependencies(runSvc, agentSvc, skillSvc, squadSvc)

	// M3 IM: the approval-card callbacks resolve through the goal layer; the
	// owner's inbound messages become intake parse runs on the configured
	// global parser agent (app_settings platform.intake_agent). The ask-card
	// reply form (决策 7-3) posts the human's reply through the comment layer
	// (parent_id → owner wake).
	imConn.SetGoalService(goalSvc)
	imConn.SetCommentService(commentSvc)
	intakeSvc := notify.NewIntakeService(qs, settingsSvc, runSvc)
	imConn.SetIntakeService(intakeSvc)

	// Protocol backends registered by provider name (runtime.provider selects).
	protoReg := proto.NewRegistry()
	protoReg.Register("acp", acpbackend.New())
	protoReg.Register("jsonl", jsonlbackend.New())
	protoReg.Register("jsonrpc", jsonrpcbackend.New())

	d := daemon.New(st, bus, *addr, protoReg, goalSvc, runSvc, commentSvc, agentSvc, squadSvc, schedSvc, imConn, qs, intakeSvc)
	d.SetTeamImportService(teamImportSvc)
	go func() {
		if err := d.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logging.Errorf("daemon: %v", err)
		}
	}()

	srv := server.New(st, bus, d, goalSvc, runSvc, commentSvc, squadSvc, schedSvc, domainSvc, imConn, teamImportSvc, skillSvc)
	if err := srv.ListenAndServe(ctx, *addr); err != nil && !errors.Is(err, context.Canceled) {
		logging.Fatalf("server: %v", err)
	}
}
