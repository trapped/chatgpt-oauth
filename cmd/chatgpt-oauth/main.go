package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/trapped/chatgpt-oauth/internal/proxy"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	command := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	authFile := flags.String("auth-file", "/data/auth.json", "Dedicated OAuth credential file")
	listen := flags.String("listen", "127.0.0.1:10531", "HTTP listen address")
	models := flags.String("models", "", "Comma-separated allowed models (required)")
	source := flags.String("from", "", "Source credential file for import-auth")
	upstream := flags.String("upstream-url", "https://chatgpt.com/backend-api/codex", "Trusted upstream base URL; override only for local tests")
	maxBody := flags.String("max-body-bytes", os.Getenv("CHATGPT_OAUTH_MAX_BODY_BYTES"), "Maximum request bytes; positive decimal (default 104857600; env CHATGPT_OAUTH_MAX_BODY_BYTES)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if command == "healthcheck" {
		client := http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://127.0.0.1:10531/health/live")
		if err != nil {
			return errors.New("healthcheck failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return errors.New("healthcheck failed")
		}
		return nil
	}
	if !contains([]string{"serve", "login", "import-auth"}, command) {
		return errors.New("usage: chatgpt-oauth [serve|login|import-auth|healthcheck]")
	}
	bodyLimit := proxy.DefaultMaxBodyBytes
	if command == "serve" {
		var err error
		bodyLimit, err = parseBodyLimit(*maxBody)
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(*authFile), 0700); err != nil {
		return errors.New("cannot create state directory")
	}
	lock, err := os.OpenFile(*authFile+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return errors.New("cannot open credential lock")
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return errors.New("another process owns this credential file; stop it before login/import")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	auth := &proxy.Auth{Path: *authFile}
	switch command {
	case "login":
		ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		return auth.Login(ctx, os.Stderr)
	case "import-auth":
		if *source == "" {
			return errors.New("import-auth requires --from")
		}
		c, err := proxy.ReadCredentials(*source)
		if err != nil {
			return err
		}
		return proxy.SaveCredentials(*authFile, c)
	}
	token := ""
	if path := os.Getenv("BACKEND_TOKEN_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return errors.New("cannot read backend token file")
		}
		token = strings.TrimSpace(string(b))
		if token == "" {
			return errors.New("empty backend token")
		}
	}
	if token == "" && !strings.HasPrefix(*listen, "127.0.0.1:") && !strings.HasPrefix(*listen, "localhost:") {
		return errors.New("non-loopback listener requires BACKEND_TOKEN_FILE")
	}
	allowed := []string{}
	for _, m := range strings.Split(*models, ",") {
		m = strings.TrimSpace(m)
		if m != "" {
			allowed = append(allowed, m)
		}
	}
	if len(allowed) == 0 {
		return errors.New("at least one model is required")
	}
	handler := &proxy.Server{Auth: auth, Models: allowed, Token: token, BaseURL: *upstream, MaxBody: bodyLimit}
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	go func() {
		<-ctx.Done()
		deadline, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = server.Shutdown(deadline)
	}()
	fmt.Fprintln(os.Stderr, "chatgpt-oauth listening on", *listen)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func parseBodyLimit(value string) (int64, error) {
	if value == "" {
		return proxy.DefaultMaxBodyBytes, nil
	}
	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil || limit <= 0 {
		return 0, errors.New("--max-body-bytes / CHATGPT_OAUTH_MAX_BODY_BYTES must be a positive decimal byte count")
	}
	return limit, nil
}

func contains(values []string, v string) bool {
	for _, s := range values {
		if s == v {
			return true
		}
	}
	return false
}
