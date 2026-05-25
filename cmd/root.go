package cmd

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/migrator"
	"github.com/rustfs/migration/internal/s3client"
	"github.com/rustfs/migration/internal/scanner"
	"github.com/rustfs/migration/internal/store"
)

var cfgPath string

func Execute() error {
	root := &cobra.Command{
		Use:   "rustfs-migration",
		Short: "Safe one-way migration tool between two S3-compatible (RustFS) clusters",
	}
	root.PersistentFlags().StringVarP(&cfgPath, "config", "c", "config.yaml", "path to YAML config")

	root.AddCommand(scanCmd())
	root.AddCommand(migrateCmd())
	root.AddCommand(statusCmd())
	return root.Execute()
}

// signalCtx returns a context cancelled on SIGINT/SIGTERM and a stop func.
func signalCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-ch
		log.Printf("received signal %s, shutting down gracefully...", s)
		cancel()
		// second signal -> hard exit
		s = <-ch
		log.Printf("received second signal %s, forcing exit", s)
		os.Exit(130)
	}()
	return ctx, cancel
}

func scanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "scan",
		Short: "Scan the Old bucket and persist object metadata locally",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			ctx, cancel := signalCtx()
			defer cancel()

			oldCli, err := s3client.New(ctx, cfg.Old)
			if err != nil {
				return err
			}
			st, err := store.Open(cfg.DBPath, cfg.Bucket)
			if err != nil {
				return err
			}
			defer st.Close()

			log.Printf("scan: bucket=%s endpoint=%s db=%s", cfg.Bucket, cfg.Old.Endpoint, cfg.DBPath)
			return scanner.Run(ctx, oldCli, cfg.Bucket, st)
		},
	}
}

func migrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Migrate scanned objects from Old to New (resumable, never overwrites)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			ctx, cancel := signalCtx()
			defer cancel()

			oldCli, err := s3client.New(ctx, cfg.Old)
			if err != nil {
				return err
			}
			newCli, err := s3client.New(ctx, cfg.New)
			if err != nil {
				return err
			}
			st, err := store.Open(cfg.DBPath, cfg.Bucket)
			if err != nil {
				return err
			}
			defer st.Close()

			m, err := migrator.New(cfg, oldCli, newCli, st)
			if err != nil {
				return err
			}
			defer m.Close()

			log.Printf("migrate: workers=%d threshold=%dMiB partSize=%dMiB partConcurrency=%d",
				cfg.Workers,
				cfg.MultipartThreshold>>20,
				cfg.MultipartPartSize>>20,
				cfg.MultipartConcurrency,
			)
			return m.Run(ctx)
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Print migration progress counters from the local DB",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			st, err := store.Open(cfg.DBPath, cfg.Bucket)
			if err != nil {
				return err
			}
			defer st.Close()
			c, err := st.Counts()
			if err != nil {
				return err
			}
			log.Printf("total=%d pending=%d done=%d conflict=%d skipped=%d failed=%d",
				c.Total, c.Pending, c.Done, c.Conflict, c.Skipped, c.Failed)
			return nil
		},
	}
}
