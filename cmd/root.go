package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"

	"github.com/rustfs/migration/internal/cluster"
	"github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/differ"
	"github.com/rustfs/migration/internal/migrator"
	"github.com/rustfs/migration/internal/s3client"
	"github.com/rustfs/migration/internal/scanner"
	"github.com/rustfs/migration/internal/store"
)

var (
	cfgPath           string
	bucketFlag        string
	diffExtra         bool
	workersFlag       int
	multipartConcFlag int
)

func Execute() error {
	return NewRoot().Execute()
}

func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "migration",
		Version:       Version,
		Short:         rootShort,
		Long:          rootLong + exampleFooter,
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.PersistentFlags().StringVarP(&cfgPath, "config", "c", "config.yaml", "config YAML path / 配置文件路径")
	root.PersistentFlags().StringVar(&bucketFlag, "bucket", "", "bucket name (empty = all) / 桶名（空=全部）")
	root.PersistentFlags().IntVar(&workersFlag, "workers", 0, "concurrent objects for run/migrate/diff, overrides YAML (must be >= 1) / 对象并发数，覆盖配置，须 >= 1")
	root.PersistentFlags().IntVar(&multipartConcFlag, "multipart-concurrency", 0, "parallel parts per large object, overrides YAML (must be >= 1) / 单个大对象的分片并发，覆盖配置，须 >= 1")
	root.CompletionOptions.DisableDefaultCmd = true

	root.AddCommand(listCmd())
	root.AddCommand(runCmd())
	root.AddCommand(scanCmd())
	root.AddCommand(migrateCmd())
	root.AddCommand(statusCmd())
	root.AddCommand(diffCmd())
	return root
}

func signalCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-ch
		log.Printf("received signal %s, shutting down gracefully... / 收到信号，正在优雅退出（再按一次强制退出）", s)
		cancel()
		s = <-ch
		log.Printf("received second signal %s, forcing exit / 第二次信号，强制退出", s)
		os.Exit(130)
	}()
	return ctx, cancel
}

func loadConfig(cmd *cobra.Command) (*config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	cfg.ApplyBucketFlag(bucketFlag)
	if err := cfg.ValidateNames(); err != nil {
		return nil, err
	}
	if err := cfg.ApplyConcurrencyOverrides(
		workersFlag,
		multipartConcFlag,
		cmd.Flags().Changed("workers"),
		cmd.Flags().Changed("multipart-concurrency"),
	); err != nil {
		return nil, err
	}
	return cfg, nil
}

func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: listShort,
		Long:  listLong,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			if err := cfg.ValidateSource(); err != nil {
				return err
			}
			ctx, cancel := signalCtx()
			defer cancel()
			oldCli, err := s3client.NewWithPool(ctx, cfg.Old, cfg.MaxInFlight())
			if err != nil {
				return err
			}
			buckets, err := cluster.ListBuckets(ctx, oldCli)
			if err != nil {
				return err
			}
			if len(buckets) == 0 {
				fmt.Println("(source has no buckets / 源集群没有桶)")
				return nil
			}
			fmt.Printf("%-44s CREATED (UTC)\n", "BUCKET")
			for _, b := range buckets {
				created := "-"
				if !b.Created.IsZero() {
					created = b.Created.UTC().Format(time.RFC3339)
				}
				fmt.Printf("%-44s %s\n", b.Name, created)
			}
			fmt.Printf("\n%d bucket(s) on %s\n", len(buckets), cfg.Old.Endpoint)
			return nil
		},
	}
}

func runCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: runShort,
		Long:  runLong,
		RunE: func(cmd *cobra.Command, args []string) error {
			return forEachBucket(cmd, true, func(ctx context.Context, cfg *config.Config, oldCli, newCli *s3.Client, bucket string) error {
				if err := scanOne(ctx, cfg, oldCli, bucket); err != nil {
					return err
				}
				return migrateOne(ctx, cfg, oldCli, newCli, bucket)
			})
		},
	}
}

func scanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "scan",
		Short: scanShort,
		Long:  scanLong,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			if err := cfg.ValidateSource(); err != nil {
				return err
			}
			ctx, cancel := signalCtx()
			defer cancel()
			oldCli, err := s3client.NewWithPool(ctx, cfg.Old, cfg.MaxInFlight())
			if err != nil {
				return err
			}
			names, err := resolveBuckets(ctx, cfg, oldCli)
			if err != nil {
				return err
			}
			for _, name := range names {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := scanOne(ctx, cfg, oldCli, name); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func migrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: migrateShort,
		Long:  migrateLong,
		RunE: func(cmd *cobra.Command, args []string) error {
			return forEachBucket(cmd, true, func(ctx context.Context, cfg *config.Config, oldCli, newCli *s3.Client, bucket string) error {
				return migrateOne(ctx, cfg, oldCli, newCli, bucket)
			})
		},
	}
}

func diffCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "diff",
		Short: diffShort,
		Long:  diffLong,
		RunE: func(cmd *cobra.Command, args []string) error {
			return forEachBucket(cmd, true, func(ctx context.Context, cfg *config.Config, oldCli, newCli *s3.Client, bucket string) error {
				res, err := differ.Run(ctx, oldCli, newCli, bucket, differ.Options{
					Workers:     cfg.Workers,
					ReportExtra: diffExtra,
					Writer:      os.Stdout,
				})
				if err != nil {
					return err
				}
				if res.HasGaps() {
					return fmt.Errorf("diff found gaps / 对比发现缺失或不一致: missing=%d size=%d etag=%d",
						res.Missing, res.SizeMismatch, res.ETagMismatch)
				}
				return nil
			})
		},
	}
	c.Flags().BoolVar(&diffExtra, "extra", false, "also report keys that exist only on the target / 同时报告仅目标上存在的 key")
	return c
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: statusShort,
		Long:  statusLong,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			targets, err := statusTargets(cfg)
			if err != nil {
				return err
			}
			if len(targets) == 0 {
				fmt.Println("no state DBs found — run `migration run` or `migration scan` first")
				fmt.Println("没有找到状态库 — 请先执行 migration run 或 migration scan")
				return nil
			}
			fmt.Printf("%-30s %9s %9s %9s %9s %9s %9s\n",
				"BUCKET", "TOTAL", "PENDING", "DONE", "CONFLICT", "SKIPPED", "FAILED")
			for _, t := range targets {
				st, err := store.Open(t.path, t.bucket)
				if err != nil {
					fmt.Printf("%s: %v\n", t.bucket, err)
					continue
				}
				c, err := st.Counts()
				_ = st.Close()
				if err != nil {
					fmt.Printf("%s: %v\n", t.bucket, err)
					continue
				}
				fmt.Printf("%-30s %9d %9d %9d %9d %9d %9d\n",
					truncate(t.bucket, 30), c.Total, c.Pending, c.Done, c.Conflict, c.Skipped, c.Failed)
			}
			return nil
		},
	}
}

func forEachBucket(cmd *cobra.Command, needTarget bool, fn func(ctx context.Context, cfg *config.Config, oldCli, newCli *s3.Client, bucket string) error) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	if needTarget {
		if err := cfg.ValidatePair(); err != nil {
			return err
		}
	} else if err := cfg.ValidateSource(); err != nil {
		return err
	}
	ctx, cancel := signalCtx()
	defer cancel()
	pool := cfg.MaxInFlight()
	oldCli, err := s3client.NewWithPool(ctx, cfg.Old, pool)
	if err != nil {
		return err
	}
	var newCli *s3.Client
	if needTarget {
		newCli, err = s3client.NewWithPool(ctx, cfg.New, pool)
		if err != nil {
			return err
		}
	}
	names, err := resolveBuckets(ctx, cfg, oldCli)
	if err != nil {
		return err
	}
	log.Printf("buckets=%d  %s → %s  workers=%d", len(names), cfg.Old.Endpoint, cfg.New.Endpoint, cfg.Workers)
	var failed []string
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(ctx, cfg, oldCli, newCli, name); err != nil {
			log.Printf("bucket %s: %v", name, err)
			failed = append(failed, name)
			continue
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed buckets / 失败的桶: %s", strings.Join(failed, ", "))
	}
	return nil
}

func resolveBuckets(ctx context.Context, cfg *config.Config, oldCli *s3.Client) ([]string, error) {
	if !cfg.AllBuckets() {
		return []string{cfg.Bucket}, nil
	}
	list, err := cluster.ListBuckets(ctx, oldCli)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list))
	for _, b := range list {
		if b.Name != "" {
			names = append(names, b.Name)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("source instance has no buckets / 源集群没有桶")
	}
	return names, nil
}

func scanOne(ctx context.Context, cfg *config.Config, oldCli *s3.Client, bucket string) error {
	st, err := store.Open(cfg.DBPathFor(bucket), bucket)
	if err != nil {
		return err
	}
	defer st.Close()
	log.Printf("scan: bucket=%s db=%s (single-threaded list; --workers applies to copy and diff)", bucket, cfg.DBPathFor(bucket))
	_, err = scanner.Run(ctx, oldCli, bucket, st)
	return err
}

func migrateOne(ctx context.Context, cfg *config.Config, oldCli, newCli *s3.Client, bucket string) error {
	if err := cluster.HeadBucket(ctx, oldCli, bucket); err != nil {
		return err
	}
	if err := cluster.EnsureBucket(ctx, newCli, bucket); err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPathFor(bucket), bucket)
	if err != nil {
		return err
	}
	defer st.Close()
	m, err := migrator.NewForBucket(cfg, bucket, oldCli, newCli, st)
	if err != nil {
		return err
	}
	defer m.Close()
	log.Printf("migrate: bucket=%s workers=%d threshold=%dMiB partSize=%dMiB partConcurrency=%d",
		bucket, cfg.Workers, cfg.MultipartThreshold>>20, cfg.MultipartPartSize>>20, cfg.MultipartConcurrency)
	return m.Run(ctx)
}

type dbTarget struct {
	bucket string
	path   string
}

func statusTargets(cfg *config.Config) ([]dbTarget, error) {
	if !cfg.AllBuckets() {
		return []dbTarget{{bucket: cfg.Bucket, path: cfg.DBPath}}, nil
	}
	dir := filepath.Dir(cfg.DBPath)
	if dir == "" {
		dir = "."
	}
	matches, err := filepath.Glob(filepath.Join(dir, "migration-*.db"))
	if err != nil {
		return nil, err
	}
	var out []dbTarget
	for _, p := range matches {
		if st, err := store.OpenExisting(p); err == nil {
			name, berr := st.BoundBucket()
			_ = st.Close()
			if berr == nil && name != "" {
				out = append(out, dbTarget{bucket: name, path: p})
				continue
			}
		}
		base := filepath.Base(p)
		name := strings.TrimSuffix(strings.TrimPrefix(base, "migration-"), ".db")
		if name != "" {
			out = append(out, dbTarget{bucket: name, path: p})
		}
	}
	if _, err := os.Stat(cfg.DBPath); err == nil {
		if st, err := store.OpenExisting(cfg.DBPath); err == nil {
			name, berr := st.BoundBucket()
			_ = st.Close()
			if berr == nil && name != "" {
				dup := false
				for _, t := range out {
					if t.path == cfg.DBPath {
						dup = true
						break
					}
				}
				if !dup {
					out = append(out, dbTarget{bucket: name, path: cfg.DBPath})
				}
			}
		}
	}
	return out, nil
}

func truncate(s string, max int) string {
	if len([]rune(s)) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}
