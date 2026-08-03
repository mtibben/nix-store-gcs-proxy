package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
	"github.com/urfave/cli/v3"
	"google.golang.org/api/option"
)

const (
	serverReadHeaderTimeout = 10 * time.Second
	serverIdleTimeout       = 60 * time.Second
	serverShutdownTimeout   = 10 * time.Second
)

var (
	version               = "dev"
	errBucketNameRequired = errors.New("please specify a bucket name")
)

func run(
	parentCtx context.Context,
	addr, bucketName, buildVersion string,
) (runErr error) {
	ctx, stop := signal.NotifyContext(parentCtx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := storage.NewClient(ctx, option.WithScopes(storage.ScopeReadWrite))
	if err != nil {
		return fmt.Errorf("create storage client: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close storage client: %w", err))
		}
	}()

	bucket := client.Bucket(bucketName)
	store := newGCSObjectStore(bucket)

	server := &http.Server{
		Addr: addr,
		Handler: newHTTPHandler(
			BucketProxy{store: store},
			cacheReadinessCheck(
				newCacheReadinessCheck(bucket),
				healthReadinessCacheTTL,
			),
			healthReadinessTimeout,
		),
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
	log.Print(startupLogMessage(buildVersion, addr, bucketName))

	return serveUntilShutdown(ctx, server)
}

func serveUntilShutdown(ctx context.Context, server *http.Server) error {
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		serverShutdownTimeout,
	)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		shutdownErr := fmt.Errorf("shut down HTTP server: %w", err)
		if err := server.Close(); err != nil {
			return errors.Join(shutdownErr, fmt.Errorf("close HTTP server: %w", err))
		}
		return shutdownErr
	}

	if err := <-serverErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP: %w", err)
	}

	return nil
}

func startupLogMessage(buildVersion, addr, bucketName string) string {
	return fmt.Sprintf(
		"Starting nix-store-gcs-proxy version %s on address %s for bucket %s",
		buildVersion,
		addr,
		bucketName,
	)
}

func action(ctx context.Context, c *cli.Command, buildVersion string) error {
	addr := c.String("addr")
	bucketName := c.String("bucket-name")
	if bucketName == "" {
		return errBucketNameRequired
	}
	return run(ctx, addr, bucketName, buildVersion)
}

func newCommand(buildVersion string) *cli.Command {
	return &cli.Command{
		Name:    "nix-store-gcs-proxy",
		Usage:   "A HTTP nix store that proxies requests to Google Storage",
		Version: buildVersion,
		Action: func(ctx context.Context, c *cli.Command) error {
			return action(ctx, c, buildVersion)
		},
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "bucket-name",
				Usage: "name of the bucket to proxy the data to",
			},
			&cli.StringFlag{
				Name:  "addr",
				Value: "localhost:3000",
				Usage: "listening address of the HTTP server",
			},
		},
	}
}

func main() {
	app := newCommand(version)
	err := app.Run(context.Background(), os.Args)
	if err != nil {
		log.Fatal(err)
	}
}
