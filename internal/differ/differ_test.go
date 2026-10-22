package differ

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rustfs/migration/internal/cluster"
	appconfig "github.com/rustfs/migration/internal/config"
	"github.com/rustfs/migration/internal/fakes3"
	"github.com/rustfs/migration/internal/s3client"
)

func TestDiffReportsMissingAndExtra(t *testing.T) {
	ctx := context.Background()
	lm := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	oldS3 := fakes3.NewWithObjects(t, "bucket", map[string][]byte{
		"ok.txt":      []byte("same"),
		"missing.txt": []byte("only-old"),
		"size.txt":    []byte("abcdef"),
	}, lm)
	defer oldS3.Close()
	newS3 := fakes3.NewWithObjects(t, "bucket", map[string][]byte{
		"ok.txt":    []byte("same"),
		"size.txt":  []byte("ab"),
		"extra.txt": []byte("only-new"),
	}, lm)
	defer newS3.Close()

	oldCli := testClient(t, ctx, oldS3.URL)
	newCli := testClient(t, ctx, newS3.URL)

	var buf bytes.Buffer
	res, err := Run(ctx, oldCli, newCli, "bucket", Options{Workers: 2, ReportExtra: true, Writer: &buf})
	if err != nil {
		t.Fatal(err)
	}
	if !res.HasGaps() {
		t.Fatal("expected gaps")
	}
	if res.OK != 1 || res.Missing != 1 || res.SizeMismatch != 1 || res.Extra != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	out := buf.String()
	if !strings.Contains(out, "missing.txt") || !strings.Contains(out, "缺失") {
		t.Fatalf("output missing bilingual report: %s", out)
	}

	kinds := map[string]bool{}
	for _, issue := range res.Issues {
		kinds[issue.Kind] = true
	}
	for _, k := range []string{"missing", "size", "extra"} {
		if !kinds[k] {
			t.Fatalf("missing issue kind %s in %+v", k, res.Issues)
		}
	}
}

func TestDiffWorkersCapParallelHeads(t *testing.T) {
	ctx := context.Background()
	lm := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	objs := map[string][]byte{}
	for i := 0; i < 8; i++ {
		objs[string(rune('a'+i))+".txt"] = []byte("same")
	}
	oldS3 := fakes3.NewWithObjects(t, "bucket", objs, lm)
	defer oldS3.Close()
	newS3 := fakes3.NewWithObjects(t, "bucket", objs, lm)
	defer newS3.Close()
	newS3.SetHeadDelay(40 * time.Millisecond)

	res, err := Run(ctx, testClient(t, ctx, oldS3.URL), testClient(t, ctx, newS3.URL), "bucket", Options{Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.HasGaps() || res.OK != int64(len(objs)) {
		t.Fatalf("expected clean diff: %+v", res)
	}
	got := newS3.MaxHeadInFlight()
	if got < 2 || got > 3 {
		t.Fatalf("diff workers=3 max in-flight HEADs=%d", got)
	}
	if oldS3.DeleteObjectCalls() != 0 || newS3.DeleteObjectCalls() != 0 || oldS3.CopyObjectCalls() != 0 || newS3.CopyObjectCalls() != 0 {
		t.Fatalf("diff issued unsafe calls: oldDelete=%d newDelete=%d oldCopy=%d newCopy=%d",
			oldS3.DeleteObjectCalls(), newS3.DeleteObjectCalls(), oldS3.CopyObjectCalls(), newS3.CopyObjectCalls())
	}
}

func TestDiffCleanWhenInSync(t *testing.T) {
	ctx := context.Background()
	lm := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	body := []byte("hello")
	oldS3 := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"a.txt": body}, lm)
	defer oldS3.Close()
	newS3 := fakes3.NewWithObjects(t, "bucket", map[string][]byte{"a.txt": body}, lm)
	defer newS3.Close()

	res, err := Run(ctx, testClient(t, ctx, oldS3.URL), testClient(t, ctx, newS3.URL), "bucket", Options{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.HasGaps() || res.OK != 1 {
		t.Fatalf("expected clean diff: %+v", res)
	}
}

func TestListAndEnsureBucket(t *testing.T) {
	ctx := context.Background()
	lm := time.Now()
	src := fakes3.NewWithObjects(t, "photos", map[string][]byte{"x": []byte("1")}, lm)
	defer src.Close()
	dst := fakes3.New(t)
	defer dst.Close()

	oldCli := testClient(t, ctx, src.URL)
	newCli := testClient(t, ctx, dst.URL)

	list, err := cluster.ListBuckets(ctx, oldCli)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "photos" {
		t.Fatalf("list=%+v", list)
	}
	if err := cluster.EnsureBucket(ctx, newCli, "photos"); err != nil {
		t.Fatal(err)
	}
	if err := cluster.EnsureBucket(ctx, newCli, "photos"); err != nil {
		t.Fatal(err)
	}
	list, err = cluster.ListBuckets(ctx, newCli)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "photos" {
		t.Fatalf("target list=%+v", list)
	}
}

func testClient(t *testing.T, ctx context.Context, endpoint string) *s3.Client {
	t.Helper()
	cli, err := s3client.New(ctx, appconfig.Endpoint{
		Endpoint:  endpoint,
		AccessKey: "ak",
		SecretKey: "sk",
		Region:    "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return cli
}
