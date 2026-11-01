// Command examd 是考场工作台的后端：静态资源、文件 API、快照与判题。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"exam.local/monaco-examd/internal/httpapi"
	"exam.local/monaco-examd/internal/judge"
	"exam.local/monaco-examd/internal/snapshot"
	"exam.local/monaco-examd/internal/store"
	"exam.local/monaco-examd/internal/workspace"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	cmd, rest := "serve", os.Args[1:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		cmd, rest = rest[0], rest[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = runServe(rest)
	case "addstudent":
		err = runAddStudent(rest)
	case "liststudents":
		err = runListStudents(rest)
	case "token":
		fmt.Println(store.NewToken())
	case "snapshot":
		err = runSnapshot(rest)
	case "provision":
		err = runProvision(rest)
	case "judge":
		err = runJudge(rest)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `examd - exam workbench backend

usage:
  examd serve       [-addr :8080] [-data ./data] [-web ./public] ...
  examd addstudent  -no 2026001 [-name 张三] [-token xxx] [-data ./data]
  examd liststudents [-data ./data]
  examd token
  examd snapshot    [-student 2026001] [-data ./data]
  examd provision   [-data ./data]
  examd judge       -student 2026001 -problem sum [-image exam-go:1.26] [-local] [-data ./data]
`)
}

type paths struct {
	data, workspaces, snapshots, db string
}

func resolvePaths(data string) (paths, error) {
	p := paths{
		data:       data,
		workspaces: filepath.Join(data, "workspaces"),
		snapshots:  filepath.Join(data, "snapshots"),
		db:         filepath.Join(data, "examd.db"),
	}
	if err := os.MkdirAll(p.workspaces, 0o755); err != nil {
		return p, err
	}
	if err := os.MkdirAll(p.snapshots, 0o755); err != nil {
		return p, err
	}
	return p, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", envOr("EXAM_ADDR", ":8080"), "listen address")
	data := fs.String("data", envOr("EXAM_DATA", "./data"), "data directory")
	web := fs.String("web", envOr("EXAM_WEB", "./public"), "built workbench directory")
	adminToken := fs.String("admin-token", envOr("EXAM_ADMIN_TOKEN", ""), "admin token (generated and stored if empty)")
	sessionTTL := fs.Duration("session-ttl", 12*time.Hour, "login session lifetime")
	snapInterval := fs.Duration("snapshot-interval", 15*time.Second, "workspace snapshot interval")
	maxFile := fs.Int64("max-file", 2<<20, "max file size in bytes")
	image := fs.String("judge-image", envOr("EXAM_JUDGE_IMAGE", "exam-go:1.26"), "docker image for judging")
	problemsDir := fs.String("problems", envOr("EXAM_PROBLEMS", "./problems"), "problem definition directory")
	workers := fs.Int("judge-workers", 2, "judge worker count")
	adminCIDRs := fs.String("admin-cidr", envOr("EXAM_ADMIN_CIDR", ""), "comma separated CIDRs allowed to use admin endpoints")
	trustProxy := fs.Bool("trust-proxy", false, "trust X-Forwarded-For")
	_ = fs.Parse(args)

	p, err := resolvePaths(*data)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	st, err := store.Open(ctx, p.db)
	if err != nil {
		return err
	}
	defer st.Close()

	if *adminToken == "" {
		tokenFile := filepath.Join(p.data, "admin_token")
		if b, err := os.ReadFile(tokenFile); err == nil && strings.TrimSpace(string(b)) != "" {
			*adminToken = strings.TrimSpace(string(b))
		} else {
			*adminToken = store.NewToken()
			if err := os.WriteFile(tokenFile, []byte(*adminToken+"\n"), 0o600); err != nil {
				return err
			}
		}
	}

	snap := snapshot.New(p.workspaces, p.snapshots, st, *snapInterval, slog.Default())
	if err := snap.Init(ctx); err != nil {
		slog.Warn("snapshot init failed", "err", err)
	}
	go snap.Run(ctx)

	problems, err := judge.LoadProblems(*problemsDir)
	if err != nil {
		return fmt.Errorf("load problems: %w", err)
	}
	var exec judge.Executor
	if err := judge.DockerAvailable(ctx); err == nil {
		exec = judge.NewDockerExecutor(*image)
	} else {
		slog.Warn("docker unavailable, judging disabled", "err", err)
	}
	j := &judge.Judge{Workspaces: p.workspaces, Problems: problems, Exec: exec, TempDir: filepath.Join(p.data, "tmp")}
	queue := judge.NewQueue(j, st, slog.Default())
	queue.Start(ctx, *workers)
	defer queue.Wait()

	api := httpapi.New(httpapi.Config{
		WebDir:       *web,
		WorkspaceDir: p.workspaces,
		SessionTTL:   *sessionTTL,
		MaxFileSize:  *maxFile,
		AdminToken:   *adminToken,
		AdminCIDRs:   splitList(*adminCIDRs),
		JudgeLimit:   20,
		TrustProxy:   *trustProxy,
	}, st, snap, queue, slog.Default())

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	slog.Info("examd listening", "addr", ln.Addr().String(), "web", *web, "data", p.data,
		"problems", len(problems), "judge", exec != nil)
	if *adminToken != "" {
		slog.Info("admin token loaded", "file", filepath.Join(p.data, "admin_token"))
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case err := <-errCh:
		return err
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	_ = srv.Shutdown(shutdownCtx)
	// 退出前补一次快照，避免刚编辑的内容没有落盘。
	if _, err := snap.SnapshotAll(context.Background()); err != nil {
		slog.Warn("final snapshot failed", "err", err)
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func openForTool(data string) (paths, *store.Store, error) {
	p, err := resolvePaths(data)
	if err != nil {
		return p, nil, err
	}
	st, err := store.Open(context.Background(), p.db)
	return p, st, err
}

func runAddStudent(args []string) error {
	fs := flag.NewFlagSet("addstudent", flag.ExitOnError)
	data := fs.String("data", envOr("EXAM_DATA", "./data"), "data directory")
	no := fs.String("no", "", "student number")
	name := fs.String("name", "", "student name")
	token := fs.String("token", "", "login token (generated if empty)")
	_ = fs.Parse(args)
	if *no == "" {
		return errors.New("-no is required")
	}
	p, st, err := openForTool(*data)
	if err != nil {
		return err
	}
	defer st.Close()
	if *token == "" {
		*token = store.NewToken()
	}
	if _, err := st.CreateStudent(context.Background(), *no, *name, *token); err != nil {
		return err
	}
	dir, err := workspace.Provision(p.workspaces, *no)
	if err != nil {
		return err
	}
	fmt.Printf("student %s created\nworkspace %s\ntoken %s\n", *no, dir, *token)
	return nil
}

func runListStudents(args []string) error {
	fs := flag.NewFlagSet("liststudents", flag.ExitOnError)
	data := fs.String("data", envOr("EXAM_DATA", "./data"), "data directory")
	_ = fs.Parse(args)
	_, st, err := openForTool(*data)
	if err != nil {
		return err
	}
	defer st.Close()
	list, err := st.ListStudents(context.Background())
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(list)
}

func runProvision(args []string) error {
	fs := flag.NewFlagSet("provision", flag.ExitOnError)
	data := fs.String("data", envOr("EXAM_DATA", "./data"), "data directory")
	_ = fs.Parse(args)
	p, st, err := openForTool(*data)
	if err != nil {
		return err
	}
	defer st.Close()
	students, err := st.ListStudents(context.Background())
	if err != nil {
		return err
	}
	for _, s := range students {
		dir, err := workspace.Provision(p.workspaces, s.No)
		if err != nil {
			return err
		}
		fmt.Printf("provisioned %s -> %s\n", s.No, dir)
	}
	return nil
}

func runSnapshot(args []string) error {
	fs := flag.NewFlagSet("snapshot", flag.ExitOnError)
	data := fs.String("data", envOr("EXAM_DATA", "./data"), "data directory")
	student := fs.String("student", "", "only this student")
	_ = fs.Parse(args)
	p, st, err := openForTool(*data)
	if err != nil {
		return err
	}
	defer st.Close()
	m := snapshot.New(p.workspaces, p.snapshots, st, 15*time.Second, slog.Default())
	if err := m.Init(context.Background()); err != nil {
		return err
	}
	var results []snapshot.Result
	if *student != "" {
		res, err := m.SnapshotOne(context.Background(), *student)
		if err != nil {
			return err
		}
		results = append(results, res)
	} else {
		results, err = m.SnapshotAll(context.Background())
		if err != nil {
			return err
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(results)
}

func runJudge(args []string) error {
	fs := flag.NewFlagSet("judge", flag.ExitOnError)
	data := fs.String("data", envOr("EXAM_DATA", "./data"), "data directory")
	student := fs.String("student", "", "student number")
	problem := fs.String("problem", "", "problem id")
	image := fs.String("image", envOr("EXAM_JUDGE_IMAGE", "exam-go:1.26"), "docker image")
	problemsDir := fs.String("problems", envOr("EXAM_PROBLEMS", "./problems"), "problem definition directory")
	local := fs.Bool("local", false, "run on the host instead of docker (dev only)")
	_ = fs.Parse(args)
	if *student == "" || *problem == "" {
		return errors.New("-student and -problem are required")
	}
	p, st, err := openForTool(*data)
	if err != nil {
		return err
	}
	defer st.Close()
	problems, err := judge.LoadProblems(*problemsDir)
	if err != nil {
		return err
	}
	var exec judge.Executor
	if *local {
		exec = &judge.LocalExecutor{}
	} else {
		if err := judge.DockerAvailable(context.Background()); err != nil {
			return fmt.Errorf("docker unavailable: %w", err)
		}
		exec = judge.NewDockerExecutor(*image)
	}
	j := &judge.Judge{Workspaces: p.workspaces, Problems: problems, Exec: exec, TempDir: filepath.Join(p.data, "tmp")}
	run := store.JudgeRun{ID: store.NewID(), StudentNo: *student, Problem: *problem, Status: "running", CreatedAt: time.Now().UTC()}
	if err := st.InsertJudgeRun(context.Background(), run); err != nil {
		return err
	}
	res := j.Run(context.Background(), run)
	if err := st.UpdateJudgeRun(context.Background(), res); err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}
