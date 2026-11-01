package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"

	"github.com/rustfs/migration/internal/cluster"
	"github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/differ"
	"github.com/rustfs/migration/internal/migrator"
	"github.com/rustfs/migration/internal/safeoutput"
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
	rateLimitFlag     string
	sourceFrozenFlag  bool
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
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&cfgPath, "config", "c", "config.yaml", "config YAML path / 配置文件路径")
	root.PersistentFlags().StringVar(&bucketFlag, "bucket", "", "bucket name (empty = all) / 桶名（空=全部）")
	root.PersistentFlags().IntVar(&workersFlag, "workers", 0, "concurrent objects for run/migrate/diff, overrides YAML (must be >= 1) / 对象并发数，覆盖配置，须 >= 1")
	root.PersistentFlags().IntVar(&multipartConcFlag, "multipart-concurrency", 0, "parallel parts per large object, overrides YAML (must be >= 1) / 单个大对象的分片并发，覆盖配置，须 >= 1")
	root.PersistentFlags().StringVar(&rateLimitFlag, "rate-limit", "", "aggregate source-read cap for run/migrate, e.g. 20M or 20MiB; 0 = unlimited / 所有 worker 从源读取的合计上限，例如 20M 或 20MiB；0 表示不限速")
	root.PersistentFlags().BoolVar(&sourceFrozenFlag, "source-frozen", false, "confirm source writes and deletes are externally frozen for run/migrate / 确认来源的写入和删除已在外部冻结")
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
	stop := make(chan struct{})
	var once sync.Once
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		var s os.Signal
		select {
		case s = <-ch:
		case <-stop:
			return
		}
		log.Printf("received signal %s, shutting down gracefully... / 收到信号，正在优雅退出（再按一次强制退出）", s)
		cancel()
		select {
		case s = <-ch:
		case <-stop:
			return
		}
		log.Printf("received second signal %s, forcing exit / 第二次信号，强制退出", s)
		os.Exit(130)
	}()
	return ctx, func() {
		once.Do(func() {
			signal.Stop(ch)
			close(stop)
			cancel()
		})
	}
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
	if err := cfg.ApplyRateLimitOverride(rateLimitFlag, cmd.Flags().Changed("rate-limit")); err != nil {
		return nil, err
	}
	if cmd.Flags().Changed("source-frozen") {
		cfg.SourceFrozen = sourceFrozenFlag
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
	var strict bool
	c := &cobra.Command{
		Use:   "run",
		Short: runShort,
		Long:  runLong,
		RunE: func(cmd *cobra.Command, args []string) error {
			return forEachBucket(cmd, true, func(ctx context.Context, cfg *config.Config, oldCli, newCli *s3.Client, bucket string) error {
				if err := scanOne(ctx, cfg, oldCli, bucket); err != nil {
					return err
				}
				return migrateOne(ctx, cfg, oldCli, newCli, bucket, strict)
			})
		},
	}
	c.Flags().BoolVar(&strict, "strict", false, "fail if any objects remain pending, failed, conflicted or skipped / 未完成、冲突或跳过时返回失败")
	return c
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
	var strict bool
	c := &cobra.Command{
		Use:   "migrate",
		Short: migrateShort,
		Long:  migrateLong,
		RunE: func(cmd *cobra.Command, args []string) error {
			return forEachBucket(cmd, true, func(ctx context.Context, cfg *config.Config, oldCli, newCli *s3.Client, bucket string) error {
				return migrateOne(ctx, cfg, oldCli, newCli, bucket, strict)
			})
		},
	}
	c.Flags().BoolVar(&strict, "strict", false, "fail if any objects remain pending, failed, conflicted or skipped / 未完成、冲突或跳过时返回失败")
	return c
}

func diffCmd() *cobra.Command {
	var strict bool
	var jsonlPath string
	c := &cobra.Command{
		Use:   "diff",
		Short: diffShort,
		Long:  diffLong,
		RunE: func(cmd *cobra.Command, args []string) error {
			var jsonl *os.File
			if jsonlPath != "" {
				var err error
				jsonl, err = os.OpenFile(jsonlPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					return fmt.Errorf("create diff JSONL: %w", err)
				}
			}
			runErr := forEachBucket(cmd, true, func(ctx context.Context, cfg *config.Config, oldCli, newCli *s3.Client, bucket string) error {
				var issueWriter io.Writer
				if jsonl != nil {
					issueWriter = jsonl
				}
				res, err := differ.Run(ctx, oldCli, newCli, bucket, differ.Options{
					Workers:     cfg.Workers,
					ReportExtra: diffExtra || strict,
					Writer:      os.Stdout,
					IssuesJSONL: issueWriter,
				})
				if err != nil {
					return err
				}
				if res.HasGaps() || (strict && res.Extra > 0) {
					return fmt.Errorf("diff found gaps / 对比发现缺失或不一致: missing=%d size=%d content=%d metadata=%d extra=%d",
						res.Missing, res.SizeMismatch, res.ContentMismatch, res.MetadataMismatch, res.Extra)
				}
				return nil
			})
			if jsonl != nil {
				if err := jsonl.Close(); err != nil {
					return errors.Join(runErr, fmt.Errorf("close diff JSONL: %w", err))
				}
			}
			return runErr
		},
	}
	c.Flags().StringVar(&jsonlPath, "issues-jsonl", "", "write all differences to a new JSONL file / 将全部差异写入新的 JSONL 文件")
	c.Flags().BoolVar(&diffExtra, "extra", false, "also report keys that exist only on the target / 同时报告仅目标上存在的 key")
	c.Flags().BoolVar(&strict, "strict", false, "also fail on target-only keys / 目标多余 key 也返回失败")
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
				return fmt.Errorf("no state DBs found; run scan first / 没有找到状态库，请先执行 scan")
			}
			fmt.Printf("%-30s %9s %9s %9s %9s %9s %9s\n",
				"BUCKET", "TOTAL", "PENDING", "DONE", "CONFLICT", "SKIPPED", "FAILED")
			var failures []error
			for _, t := range targets {
				st, err := store.OpenExisting(t.path)
				if err != nil {
					fmt.Printf("%s: %s\n", t.bucket, safeoutput.Escape(err.Error()))
					failures = append(failures, fmt.Errorf("status %s: %w", t.bucket, err))
					continue
				}
				bound, err := st.BoundBucket()
				if err != nil || bound != t.bucket {
					_ = st.Close()
					failures = append(failures, fmt.Errorf("state %s bound to %q, expected %q: %v", t.path, bound, t.bucket, err))
					continue
				}
				c, err := st.Counts()
				_ = st.Close()
				if err != nil {
					fmt.Printf("%s: %s\n", t.bucket, safeoutput.Escape(err.Error()))
					failures = append(failures, fmt.Errorf("status %s: %w", t.bucket, err))
					continue
				}
				fmt.Printf("%-30s %9d %9d %9d %9d %9d %9d\n",
					truncate(t.bucket, 30), c.Total, c.Pending, c.Done, c.Conflict, c.Skipped, c.Failed)
			}
			return errors.Join(failures...)
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
	if cmd.Name() == "run" || cmd.Name() == "migrate" {
		if err := cfg.ValidateMigration(); err != nil {
			return err
		}
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
	log.Printf("buckets=%d  %s → %s  workers=%d  rate_limit=%s", len(names), cfg.Old.Endpoint, cfg.New.Endpoint, cfg.Workers, cfg.RateLimit)
	var failed []string
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(ctx, cfg, oldCli, newCli, name); err != nil {
			log.Printf("bucket %s: %s", name, safeoutput.Escape(err.Error()))
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
	if err := st.BindMigration(cfg.Old.Endpoint, cfg.New.Endpoint); err != nil {
		return err
	}
	log.Printf("scan: bucket=%s db=%s (single-threaded list; --workers applies to copy and diff)", bucket, cfg.DBPathFor(bucket))
	_, err = scanner.Run(ctx, oldCli, bucket, st)
	return err
}

func migrateOne(ctx context.Context, cfg *config.Config, oldCli, newCli *s3.Client, bucket string, strict ...bool) error {
	if err := cfg.ValidateMigration(); err != nil {
		return err
	}
	if err := cluster.HeadBucket(ctx, oldCli, bucket); err != nil {
		return err
	}
	if _, err := os.Stat(cfg.DBPathFor(bucket)); err != nil {
		return fmt.Errorf("migration requires an existing completed scan; run scan or run first: %w", err)
	}
	st, err := store.Open(cfg.DBPathFor(bucket), bucket)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.BindMigration(cfg.Old.Endpoint, cfg.New.Endpoint); err != nil {
		return err
	}
	if err := st.RequireCompleteScan(); err != nil {
		return err
	}
	if err := cluster.EnsureBucket(ctx, newCli, bucket); err != nil {
		return err
	}
	m, err := migrator.NewForBucket(cfg, bucket, oldCli, newCli, st)
	if err != nil {
		return err
	}
	defer m.Close()
	log.Printf("migrate: bucket=%s workers=%d rate_limit=%s threshold=%dMiB partSize=%dMiB partConcurrency=%d",
		bucket, cfg.Workers, cfg.RateLimit, cfg.MultipartThreshold>>20, cfg.MultipartPartSize>>20, cfg.MultipartConcurrency)
	if err := m.Run(ctx); err != nil {
		return err
	}
	if len(strict) > 0 && strict[0] {
		counts, err := st.Counts()
		if err != nil {
			return fmt.Errorf("read migration status: %w", err)
		}
		if counts.Pending != 0 || counts.Failed != 0 || counts.Conflict != 0 || counts.Skipped != 0 || counts.Done != counts.Total {
			return fmt.Errorf("strict migration incomplete: pending=%d failed=%d conflict=%d skipped=%d done=%d total=%d", counts.Pending, counts.Failed, counts.Conflict, counts.Skipped, counts.Done, counts.Total)
		}
	}
	return nil
}

type dbTarget struct {
	bucket string
	path   string
}

func statusTargets(cfg *config.Config) ([]dbTarget, error) {
	if !cfg.AllBuckets() {
		return []dbTarget{{bucket: cfg.Bucket, path: cfg.DBPath}}, nil
	}
	matches, err := filepath.Glob(cfg.DBPathFor("*"))
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(cfg.DBPath); err == nil {
		matches = append(matches, cfg.DBPath)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	var out []dbTarget
	seen := make(map[string]bool)
	for _, path := range matches {
		if seen[path] {
			continue
		}
		seen[path] = true
		st, err := store.OpenExisting(path)
		if err != nil {
			return nil, fmt.Errorf("read state %s: %w", path, err)
		}
		name, readErr := st.BoundBucket()
		closeErr := st.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return nil, err
		}
		if name == "" {
			return nil, fmt.Errorf("state %s has no bound bucket", path)
		}
		out = append(out, dbTarget{bucket: name, path: path})
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
