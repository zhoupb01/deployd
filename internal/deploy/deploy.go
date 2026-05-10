package deploy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zhoupb01/deployd/internal/config"
)

const (
	StateIdle    = "idle"
	StateRunning = "running"
	StateSuccess = "success"
	StateFailed  = "failed"

	tailBytes   = 32 * 1024
	historyFile = "history.jsonl"
)

var pullRetryDelays = []time.Duration{
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
}

var (
	ErrUnknownService = errors.New("unknown service")
	ErrBusy           = errors.New("service is currently being deployed")
)

type Record struct {
	DeployID   string     `json:"deploy_id"`
	Service    string     `json:"service"`
	State      string     `json:"state"`
	Tag        string     `json:"tag,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMS int64      `json:"duration_ms,omitempty"`
	ExitErr    string     `json:"exit_err,omitempty"`
	Tail       string     `json:"tail,omitempty"`
}

type Manager struct {
	stateDir  string
	services  map[string]*svc
	historyMu sync.Mutex
}

type svc struct {
	cfg  *config.ServiceConfig
	busy atomic.Bool
	last atomic.Pointer[Record]
}

func New(cfg *config.Config) (*Manager, error) {
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	m := &Manager{
		stateDir: cfg.StateDir,
		services: make(map[string]*svc, len(cfg.Services)),
	}
	for name, s := range cfg.Services {
		m.services[name] = &svc{cfg: s}
	}
	return m, nil
}

func (m *Manager) Trigger(service, tag string) (string, error) {
	s, ok := m.services[service]
	if !ok {
		return "", ErrUnknownService
	}
	if !s.busy.CompareAndSwap(false, true) {
		return "", ErrBusy
	}
	deployID := newDeployID()
	started := time.Now().UTC()
	s.last.Store(&Record{
		DeployID:  deployID,
		Service:   service,
		State:     StateRunning,
		Tag:       tag,
		StartedAt: started,
	})
	go m.run(s, deployID, tag, started)
	return deployID, nil
}

func (m *Manager) Status(service string) (*Record, error) {
	s, ok := m.services[service]
	if !ok {
		return nil, ErrUnknownService
	}
	if rec := s.last.Load(); rec != nil {
		return rec, nil
	}
	return &Record{Service: service, State: StateIdle}, nil
}

func (m *Manager) run(s *svc, deployID, tag string, started time.Time) {
	defer s.busy.Store(false)

	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
	defer cancel()
	cmdErr := runCompose(ctx, s.cfg, &output)

	finished := time.Now().UTC()
	rec := &Record{
		DeployID:   deployID,
		Service:    s.cfg.Name,
		Tag:        tag,
		StartedAt:  started,
		FinishedAt: &finished,
		DurationMS: finished.Sub(started).Milliseconds(),
		Tail:       tailString(output.Bytes(), tailBytes),
	}
	if cmdErr != nil {
		rec.State = StateFailed
		rec.ExitErr = cmdErr.Error()
	} else {
		rec.State = StateSuccess
	}
	s.last.Store(rec)

	if err := m.appendHistory(rec); err != nil {
		log.Printf("deployd: append history failed: %v", err)
	}
	log.Printf("deployd: service=%s deploy_id=%s state=%s duration=%dms",
		rec.Service, rec.DeployID, rec.State, rec.DurationMS)
}

func runCompose(ctx context.Context, cfg *config.ServiceConfig, out *bytes.Buffer) error {
	if _, err := os.Stat(cfg.Workdir); err != nil {
		return fmt.Errorf("workdir missing: %w", err)
	}
	pullArgs := []string{"docker", "compose", "pull"}
	upArgs := []string{"docker", "compose", "up", "-d", "--wait"}
	if cfg.ComposeService != "" {
		pullArgs = append(pullArgs, cfg.ComposeService)
		upArgs = append(upArgs, cfg.ComposeService)
	}

	if err := runComposeCommandWithRetry(ctx, cfg, out, pullArgs, pullRetryDelays); err != nil {
		return err
	}
	return runComposeCommand(ctx, cfg, out, upArgs)
}

func runComposeCommandWithRetry(ctx context.Context, cfg *config.ServiceConfig, out *bytes.Buffer, args []string, delays []time.Duration) error {
	var lastErr error
	for attempt := 0; attempt <= len(delays); attempt++ {
		if attempt > 0 {
			delay := delays[attempt-1]
			fmt.Fprintf(out, "\nretry %d/%d after %s: %s\n", attempt+1, len(delays)+1, delay, strings.Join(args, " "))
			if err := sleepContext(ctx, delay); err != nil {
				return fmt.Errorf("%s retry canceled: %w (last error: %v)", strings.Join(args, " "), err, lastErr)
			}
		}

		err := runComposeCommand(ctx, cfg, out, args)
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return err
		}
	}
	return lastErr
}

func runComposeCommand(ctx context.Context, cfg *config.ServiceConfig, out *bytes.Buffer, args []string) error {
	fmt.Fprintf(out, "\n$ %s\n", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = cfg.Workdir
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", strings.Join(args, " "), err)
	}
	return nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) appendHistory(rec *Record) error {
	m.historyMu.Lock()
	defer m.historyMu.Unlock()
	path := filepath.Join(m.stateDir, historyFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(rec)
}

func newDeployID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%d-%s", time.Now().Unix(), hex.EncodeToString(b[:]))
}

func tailString(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return "...[truncated]...\n" + string(b[len(b)-max:])
}
