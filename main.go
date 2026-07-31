package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
	"github.com/urfave/cli"
	"github.com/urfave/negroni"
	"google.golang.org/api/option"
)

const (
	serverReadHeaderTimeout = 10 * time.Second
	serverIdleTimeout       = 60 * time.Second
	serverShutdownTimeout   = 10 * time.Second
)

var errBucketNameRequired = errors.New("please specify a bucket name")

func fetchHeader(req *http.Request, key string) (string, bool) {
	if _, ok := req.Header[key]; ok {
		return req.Header.Get(key), true
	}
	return "", false
}

type BucketProxy struct {
	bucket *storage.BucketHandle
}

func (s BucketProxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	objectPath := req.URL.Path[1:]
	object := s.bucket.Object(objectPath)

	ctx := req.Context()
	switch req.Method {
	case http.MethodHead:
		_, err := object.Attrs(ctx)
		if err != nil {
			if errors.Is(err, storage.ErrObjectNotExist) {
				http.Error(w, "File not found", http.StatusNotFound)
			} else {
				http.Error(w, err.Error(), http.StatusBadGateway)
			}
			return
		}
	case http.MethodGet:
		rc, err := object.NewReader(ctx)
		if err != nil {
			if errors.Is(err, storage.ErrObjectNotExist) {
				http.Error(w, "File not found", http.StatusNotFound)
			} else {
				http.Error(w, err.Error(), http.StatusBadGateway)
			}
			return
		}
		defer func() {
			if err := rc.Close(); err != nil {
				log.Println(err)
			}
		}()

		if _, err := io.Copy(w, rc); err != nil {
			log.Println(err)
		}
	case http.MethodPut:
		// Write the object to GCS
		wc := object.NewWriter(ctx)

		// Copy the supported headers over from the original request
		if val, ok := fetchHeader(req, "Content-Type"); ok {
			wc.ContentType = val
		}
		if val, ok := fetchHeader(req, "Content-Language"); ok {
			wc.ContentLanguage = val
		}
		if val, ok := fetchHeader(req, "Content-Encoding"); ok {
			wc.ContentEncoding = val
		}
		if val, ok := fetchHeader(req, "Content-Disposition"); ok {
			wc.ContentDisposition = val
		}
		if val, ok := fetchHeader(req, "Cache-Control"); ok {
			wc.CacheControl = val
		}

		if _, err := io.Copy(wc, req.Body); err != nil {
			log.Println(err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if err := wc.Close(); err != nil {
			log.Println(err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		if _, err := fmt.Fprint(w, "OK"); err != nil {
			log.Println(err)
		}
	default:
		msg := fmt.Sprintf("Method '%s' is not supported", req.Method)
		http.Error(w, msg, http.StatusMethodNotAllowed)
	}
}

// Start the HTTP server
func run(addr, bucketName string) (runErr error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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

	n := negroni.Classic() // Includes some default middlewares
	n.UseHandler(BucketProxy{bucket})

	server := &http.Server{
		Addr:              addr,
		Handler:           newHTTPHandler(n, newCacheReadinessCheck(bucket), healthReadinessTimeout),
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.ListenAndServe()
	}()

	log.Printf("Starting proxy server on address %s for bucket %s", addr, bucketName)

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

// Urfave cli action
func action(c *cli.Context) error {
	addr := c.String("addr")
	bucketName := c.String("bucket-name")
	if bucketName == "" {
		return errBucketNameRequired
	}
	return run(addr, bucketName)
}

func main() {
	app := cli.NewApp()
	app.Name = "nix-store-gcs-proxy"
	app.Usage = "A HTTP nix store that proxies requests to Google Storage"
	app.Version = "0.0.1"
	app.Action = action
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "bucket-name",
			Usage: "name of the bucket to proxy the data to",
		},
		cli.StringFlag{
			Name:  "addr",
			Value: "localhost:3000",
			Usage: "listening address of the HTTP server",
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}
