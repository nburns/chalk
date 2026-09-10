package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kardianos/service"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"
)

const (
	serviceName   = "com.chalk.blackboard"
	shutdownGrace = 5 * time.Second
)

var version = "dev"

type config struct {
	dbPath         string
	host           string
	port           string
	apiKey         string
	reaperInterval time.Duration
}

func (c config) addr() string { return c.host + ":" + c.port }

func (c config) localOnly() bool { return c.host == "127.0.0.1" || c.host == "localhost" }

func loadConfig() (config, error) {
	dbPath := os.Getenv("BLACKBOARD_DB")
	if dbPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return config{}, fmt.Errorf("home dir: %w", err)
		}
		dbPath = filepath.Join(home, ".blackboard", "board.db")
	}

	cfg := config{
		dbPath:         dbPath,
		host:           def(os.Getenv("BLACKBOARD_HOST"), "127.0.0.1"),
		port:           def(os.Getenv("BLACKBOARD_PORT"), "8080"),
		apiKey:         os.Getenv("BLACKBOARD_API_KEY"),
		reaperInterval: 60 * time.Second,
	}

	if s := os.Getenv("BLACKBOARD_REAPER_INTERVAL"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return config{}, fmt.Errorf("BLACKBOARD_REAPER_INTERVAL: %w", err)
		}
		cfg.reaperInterval = d
	}

	if cfg.apiKey == "" && !cfg.localOnly() {
		return config{}, fmt.Errorf("BLACKBOARD_API_KEY must be set when binding to %s", cfg.host)
	}

	return cfg, nil
}

// env returns the resolved config as the variables a service definition needs,
// so an installed service keeps running the settings it was installed with.
func (c config) env() map[string]string {
	vars := map[string]string{
		"BLACKBOARD_DB":              c.dbPath,
		"BLACKBOARD_HOST":            c.host,
		"BLACKBOARD_PORT":            c.port,
		"BLACKBOARD_REAPER_INTERVAL": c.reaperInterval.String(),
	}
	if c.apiKey != "" {
		vars["BLACKBOARD_API_KEY"] = c.apiKey
	}
	return vars
}

func requireBearer(key string, next http.Handler) http.Handler {
	expected := []byte("Bearer " + key)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, expected) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func runReaper(ctx context.Context, d *DB, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			n, err := d.reap(ctx)
			if err != nil {
				log.Printf("reaper: %v", err)
			} else if n > 0 {
				log.Printf("reaper: archived %d expired working_on entries", n)
			}
		case <-ctx.Done():
			return
		}
	}
}

func newMCPServer(db *DB) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "blackboard", Version: version}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "post",
		Description: fmt.Sprintf(
			"Post an entry to the blackboard. "+
				"type: %s (default: note). "+
				"source: human|agent (default: agent). "+
				"expires_at (ISO8601) is required when type=working_on. "+
				"tags is a comma-separated string.",
			typeList(),
		),
	}, db.handlePost)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "read",
		Description: "Read entries from the blackboard, newest first. " +
			"status defaults to 'active'; pass 'all' to include done and archived. " +
			"since accepts ISO8601 or 'YYYY-MM-DD HH:MM:SS' to return only entries created after that time. " +
			"limit defaults to 20.",
	}, db.handleRead)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "search",
		Description: "Full-text search the blackboard over content, tags, and agent_id. " +
			"Returns matching entries with a snippet showing context around the match. " +
			"Excludes archived entries. limit defaults to 10.",
	}, db.handleSearch)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "update",
		Description: "Update an existing entry. Any agent may update any entry. " +
			"Provide at least one of: content, tags, status (active|done|archived).",
	}, db.handleUpdate)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "mark_done",
		Description: "Mark an entry as done. Convenience wrapper for update with status=done.",
	}, db.handleMarkDone)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "delete",
		Description: "Delete an entry. " +
			"hard=false archives it (soft delete, reversible via update). " +
			"hard=true permanently deletes the row and its relations; requires instructed_by_human=true.",
	}, db.handleDelete)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "relate",
		Description: "Create a typed relationship between two entries. kind: responds_to|refutes|supports|follows_from|blocks.",
	}, db.handleRelate)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "relations",
		Description: "Fetch all relation rows for an entry (both directions). Retrieve the actual entries separately with read.",
	}, db.handleRelations)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "stats",
		Description: "Board summary: entry counts by type (active entries only), counts by status across all entries, and list of distinct active agent IDs.",
	}, db.handleStats)

	srv.AddPrompt(&mcp.Prompt{
		Name:        "board-usage",
		Description: "Coordination conventions for using the blackboard — read this before posting.",
	}, handleBoardUsage)

	return srv
}

type program struct {
	cfg     config
	db      *DB
	httpSrv *http.Server
	cancel  context.CancelFunc
	served  chan struct{}
}

// Start must not block: it binds the port synchronously so that a failure is
// reported to the service manager, then serves in the background.
func (p *program) Start(service.Service) error {
	db, err := openDB(p.cfg.dbPath)
	if err != nil {
		return fmt.Errorf("db: %w", err)
	}

	ln, err := net.Listen("tcp", p.cfg.addr())
	if err != nil {
		db.Close()
		return fmt.Errorf("listen %s: %w", p.cfg.addr(), err)
	}

	srv := newMCPServer(db)
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return srv
	}, nil))

	var root http.Handler = mux
	if p.cfg.apiKey != "" {
		root = requireBearer(p.cfg.apiKey, mux)
		log.Printf("API key authentication enabled")
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.db = db
	p.cancel = cancel
	p.served = make(chan struct{})
	p.httpSrv = &http.Server{
		Handler:      root,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go runReaper(ctx, db, p.cfg.reaperInterval)

	go func() {
		defer close(p.served)
		err := p.httpSrv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			// The listener is gone but the process is not; exit so the service
			// manager restarts us instead of leaving a server that serves nothing.
			log.Printf("serve: %v", err)
			os.Exit(1)
		}
	}()

	log.Printf("blackboard %s listening on http://%s/mcp  db=%s", version, p.cfg.addr(), p.cfg.dbPath)
	return nil
}

func (p *program) Stop(service.Service) error {
	if p.cancel != nil {
		p.cancel()
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	var errs []error
	if p.httpSrv != nil {
		if err := p.httpSrv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown: %w", err))
		}
		<-p.served
	}
	if p.db != nil {
		if err := p.db.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close db: %w", err))
		}
	}
	return errors.Join(errs...)
}

// The stock unit template from the service library ends in
// WantedBy=multi-user.target, a target the systemd user manager never
// activates, so an installed user service would never start at login. This is
// that template with the user manager's boot target and a restart delay closer
// to what launchd does on macOS. Ignored on platforms that are not systemd.
const systemdUserUnit = `[Unit]
Description={{Description}}
ConditionFileIsExecutable={{Path | cmdEscape}}
{{range Dependencies}}{{.}}
{{end}}
[Service]
StartLimitInterval=5
StartLimitBurst=10
ExecStart={{Path | cmdEscape}}{{range Arguments}} {{. | cmd}}{{end}}
{{if ChRoot}}RootDirectory={{ChRoot | cmd}}
{{end}}{{if WorkingDirectory}}WorkingDirectory={{WorkingDirectory | cmdEscape}}
{{end}}{{if UserName}}User={{UserName}}
{{end}}{{if ReloadSignal}}ExecReload=/bin/kill -{{ReloadSignal}} "$MAINPID"
{{end}}{{if PIDFile}}PIDFile={{PIDFile | cmd}}
{{end}}{{if OutputFileSupport}}StandardOutput=file:{{LogDirectory}}/{{Name}}.out
StandardError=file:{{LogDirectory}}/{{Name}}.err
{{end}}{{if LimitNOFILE}}LimitNOFILE={{LimitNOFILE}}
{{end}}{{if Restart}}Restart={{Restart}}
{{end}}{{if SuccessExitStatus}}SuccessExitStatus={{SuccessExitStatus}}
{{end}}RestartSec=5
EnvironmentFile=-/etc/sysconfig/{{Name}}

{{range EnvVars}}{{.}}
{{end}}[Install]
WantedBy=default.target
`

// logDirectory keeps service logs somewhere conventional for the platform.
// Only launchd consumes it; systemd services log to the journal.
func logDirectory() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Logs")
}

func newService(cfg config) (service.Service, error) {
	return service.New(&program{cfg: cfg}, &service.Config{
		Name:        serviceName,
		DisplayName: "chalk blackboard",
		Description: "Shared blackboard MCP server for agent coordination.",
		EnvVars:     cfg.env(),
		Option: service.KeyValue{
			"UserService":   true,
			"RunAtLoad":     true,
			"KeepAlive":     true,
			"LogDirectory":  logDirectory(),
			"SystemdScript": systemdUserUnit,
		},
	})
}

const usage = `chalk - a shared blackboard MCP server for agent coordination

usage:
  chalk                      run in the foreground (ctrl-c to stop)
  chalk service install      install and enable a per-user background service
  chalk service uninstall    remove the service
  chalk service start        start the installed service
  chalk service stop         stop the installed service
  chalk service restart      restart the installed service
  chalk service status       report whether the service is running
  chalk version              print the version

environment:
  BLACKBOARD_DB                database path (default: ~/.blackboard/board.db)
  BLACKBOARD_HOST              bind address (default: 127.0.0.1)
  BLACKBOARD_PORT              port (default: 8080)
  BLACKBOARD_API_KEY           require 'Authorization: Bearer <key>'; required
                               unless the bind address is local
  BLACKBOARD_REAPER_INTERVAL   how often expired working_on entries are
                               archived (default: 1m)

'chalk service install' records the current values of those variables in the
service definition, so set them in the same command:

  BLACKBOARD_PORT=9000 chalk service install
`

func statusText(s service.Status) string {
	switch s {
	case service.StatusRunning:
		return "running"
	case service.StatusStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

func runServiceCommand(svc service.Service, cfg config, action string) error {
	if action == "status" {
		st, err := svc.Status()
		if err != nil {
			return err
		}
		fmt.Printf("%s: %s\n", serviceName, statusText(st))
		return nil
	}

	valid := false
	for _, a := range service.ControlAction {
		if a == action {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("unknown service command %q (want: %s, status)", action, strings.Join(service.ControlAction[:], ", "))
	}

	if err := service.Control(svc, action); err != nil {
		return err
	}

	switch action {
	case "install":
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve executable: %w", err)
		}
		fmt.Printf("installed %s\n", serviceName)
		fmt.Printf("  binary    %s\n", exe)
		fmt.Printf("  endpoint  http://%s/mcp\n", cfg.addr())
		fmt.Printf("  database  %s\n", cfg.dbPath)
		if dir := logDirectory(); dir != "" {
			fmt.Printf("  logs      %s/%s.out.log\n", dir, serviceName)
		}
		if cfg.apiKey != "" {
			fmt.Println("  warning: BLACKBOARD_API_KEY is stored in the service definition in plaintext")
		}
		fmt.Println("\nstart it with: chalk service start")
	default:
		fmt.Printf("%s: %s\n", action, serviceName)
	}
	return nil
}

func run() error {
	args := os.Args[1:]

	if len(args) > 0 {
		switch args[0] {
		case "help", "-h", "--help":
			fmt.Print(usage)
			return nil
		case "version", "-v", "--version":
			fmt.Printf("chalk %s\n", version)
			return nil
		case "service":
			if len(args) != 2 {
				return errors.New("usage: chalk service install|uninstall|start|stop|restart|status")
			}
		default:
			return fmt.Errorf("unknown command %q (try 'chalk help')", args[0])
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	svc, err := newService(cfg)
	if err != nil {
		return err
	}

	if len(args) > 0 {
		return runServiceCommand(svc, cfg, args[1])
	}
	return svc.Run()
}

func main() {
	log.SetFlags(log.LstdFlags)
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
