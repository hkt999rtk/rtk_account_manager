package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"rtk_account_manager/internal/database"
)

func TestCloudDeletionWorkerChild(t *testing.T) {
	if os.Getenv("TEST_CLOUD_DELETION_WORKER_CHILD") != "1" {
		t.Skip("subprocess entry only")
	}
	main()
}

func TestCloudDeletionWorkerProcessStartsAndStops(t *testing.T) {
	dsn := os.Getenv("TEST_CLOUD_DELETION_WORKER_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires dedicated isolated worker process database")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme != "postgres" || u.Host != "127.0.0.1:63229" || u.Path != "/multicloud_am_deletion_worker_process_test" {
		t.Fatal("requires dedicated loopback test database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := database.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var jobs int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM cloud_deletion_jobs`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("process fixture must be empty %d %v", jobs, err)
	}
	var calls atomic.Int32
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer billing.Close()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCloudDeletionWorkerChild$")
	secret := strings.Repeat("w", 32)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TEST_CLOUD_DELETION_WORKER_CHILD=1", "DATABASE_URL=" + dsn, "BILLING_HANDOFF_BASE_URL=" + billing.URL, "BILLING_HANDOFF_TOKEN=" + secret, "CLOUD_DELETION_WORKER_POLL_INTERVAL=5ms", "ACCOUNT_MANAGER_ENV=test", "ACCOUNT_MANAGER_LOG_LEVEL=info"}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		writer.Close()
		t.Fatal(err)
	}
	writer.Close()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready, scanned := make(chan struct{}, 1), make(chan struct{})
	var output strings.Builder
	go func() {
		defer close(scanned)
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			line := scanner.Text()
			output.WriteString(line)
			output.WriteByte('\n')
			if strings.Contains(line, "starting cloud deletion recovery worker") {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
		}
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("worker did not become ready")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		<-scanned
		t.Fatalf("worker shutdown: %v %s", err, output.String())
	}
	<-scanned
	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), dsn) {
		t.Fatal("worker leaked credentials")
	}
	if calls.Load() != 0 {
		t.Fatal("empty recovery worker invented remote work")
	}
}
