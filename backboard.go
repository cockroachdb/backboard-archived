package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/google/go-github/github"
	_ "github.com/lib/pq" // activate postgres database adapter
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
)

var repos = []repo{
	{githubOwner: "cockroachdb", githubRepo: "cockroach"},
}

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %s\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func run(args []string) error {
	var (
		flags      = flag.NewFlagSet("", flag.ContinueOnError)
		bindAddr   = flags.String("bind", "", "listen address")
		branch     = flags.String("branch", "", "default branch to display [required if --bind]")
		connString = flags.String("conn", "", "connection string [required]")
	)

	if err := flags.Parse(args[1:]); err != nil {
		flags.PrintDefaults()
		return err
	}

	if *connString == "" {
		flags.PrintDefaults()
		return errors.New("--conn is required")
	}

	githubToken := os.Getenv("BACKBOARD_GITHUB_TOKEN")
	if githubToken == "" {
		return errors.New("missing BACKBOARD_GITHUB_TOKEN env var")
	}

	db, err := sql.Open("postgres", *connString)
	if err != nil {
		return err
	}

	work := &errgroup.Group{}

	// Create a top-level context that responds to SIGINT.
	ctx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	ch := make(chan os.Signal)
	signal.Notify(ch, os.Interrupt)
	work.Go(func() error {
		select {
		case <-ch:
			log.Print("Shutting down")
			shutdown()
		case <-ctx.Done():
		}
		return nil
	})

	if err := db.PingContext(ctx); err != nil {
		return err
	}

	ghClient := github.NewClient(oauth2.NewClient(ctx, oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: githubToken},
	)))

	// Just sync files instead of starting a server.
	if *bindAddr == "" {
		if err := bootstrap(ctx, db); err != nil {
			return fmt.Errorf("while bootstrapping: %s", err)
		}

		return syncAll(ctx, ghClient, db)
	}

	if *branch == "" {
		flags.PrintDefaults()
		return errors.New("--branch flag is required")
	}

	// Configure the HTTP handler with just /healthz until bootstrapped.
	bootstrapped := false

	handler := &http.ServeMux{}
	handler.HandleFunc("/healthz", func(writer http.ResponseWriter, request *http.Request) {
		ctx, _ := context.WithTimeout(ctx, 5*time.Second)
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")

		// Look for /healthz?ready=1 to enable network traffic.
		if request.URL.Query().Get("ready") != "" && !bootstrapped {
			writer.WriteHeader(http.StatusServiceUnavailable)
			writer.Write([]byte("Bootstrapping"))
		} else if err := db.PingContext(ctx); err != nil {
			writer.WriteHeader(http.StatusInternalServerError)
			writer.Write([]byte(err.Error()))
		} else {
			writer.WriteHeader(http.StatusOK)
			writer.Write([]byte("OK"))
		}
	})

	// Start the server, ignoring graceful-shutdown error value.
	s := &http.Server{Addr: *bindAddr, Handler: handler}
	work.Go(func() error {
		err := s.ListenAndServe()
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	})
	// Terminate the server when the top-level context is cancelled.
	work.Go(func() error {
		<-ctx.Done()
		timeout, _ := context.WithTimeout(context.Background(), 5*time.Second)
		return s.Shutdown(timeout)
	})

	// Download all necessary data.
	if err := bootstrap(ctx, db); err != nil {
		return fmt.Errorf("while bootstrapping: %s", err)
	}

	// Add the main handler only after all of the data is set up.
	handler.Handle("/", &server{db: db, defaultBranch: *branch})
	// Allow readiness check to proceed.
	bootstrapped = true
	log.Print("Bootstrapping complete")
	// Launch the sync loop.
	go syncLoop(ctx, ghClient, db)
	return work.Wait()
}

func syncLoop(ctx context.Context, ghClient *github.Client, db *sql.DB) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
			// TODO(benesch): webhook support?
			if err := syncAll(ctx, ghClient, db); err != nil {
				log.Printf("sync error: %s", err)
			}
		}
	}
}
