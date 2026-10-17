package s3

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Example from the AWS SigV4 query-string authentication documentation.
func TestPresignAWSExample(t *testing.T) {
	c := &Config{Endpoint: "https://s3.amazonaws.com", Region: "us-east-1", Bucket: "examplebucket", VirtualHost: true,
		AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	now, _ := time.Parse("20060102T150405Z", "20130524T000000Z")
	u, err := c.Presign(http.MethodGet, "test.txt", 86400*time.Second, now)
	if err != nil {
		t.Fatal(err)
	}
	want := "X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if !strings.HasPrefix(u, "https://examplebucket.s3.amazonaws.com/test.txt?") || !strings.HasSuffix(u, want) {
		t.Fatalf("unexpected url %s", u)
	}
}

func TestKey(t *testing.T) {
	c := &Config{Prefix: "paas/"}
	if k := c.Key("db1", "x.dump"); k != "paas/db1/x.dump" {
		t.Fatal(k)
	}
}

// fakeS3 answers the few calls the multipart and list helpers make.
func fakeS3(t *testing.T, h http.HandlerFunc) *Config {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s"}
}

func TestListPaginates(t *testing.T) {
	var tokens []string
	c := fakeS3(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/b/" || q.Get("list-type") != "2" || q.Get("prefix") != "pitr/db/wal/" || q.Get("X-Amz-Signature") == "" {
			t.Errorf("unexpected request %s", r.URL)
		}
		tokens = append(tokens, q.Get("continuation-token"))
		if q.Get("continuation-token") == "" {
			w.Write([]byte(`<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>t 1</NextContinuationToken>
<Contents><Key>pitr/db/wal/a</Key><Size>5</Size><LastModified>2026-10-17T01:02:03.000Z</LastModified></Contents></ListBucketResult>`))
			return
		}
		w.Write([]byte(`<ListBucketResult><IsTruncated>false</IsTruncated>
<Contents><Key>pitr/db/wal/b</Key><Size>7</Size><LastModified>2026-10-17T02:00:00.000Z</LastModified></Contents></ListBucketResult>`))
	})
	objs, err := c.List("pitr/db/wal/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 || objs[0].Key != "pitr/db/wal/a" || objs[1].Size != 7 || objs[0].LastModified.Hour() != 1 {
		t.Fatalf("objects: %+v", objs)
	}
	if len(tokens) != 2 || tokens[1] != "t 1" {
		t.Fatalf("tokens: %q", tokens)
	}
}

func TestMultipart(t *testing.T) {
	var completed string
	aborted := false
	c := fakeS3(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			w.Write([]byte(`<InitiateMultipartUploadResult><UploadId>u-1</UploadId></InitiateMultipartUploadResult>`))
		case r.Method == http.MethodPost && q.Get("uploadId") == "u-1":
			b, _ := io.ReadAll(r.Body)
			completed = string(b)
			w.Write([]byte(`<CompleteMultipartUploadResult><Key>x</Key></CompleteMultipartUploadResult>`))
		case r.Method == http.MethodDelete && q.Get("uploadId") == "u-1":
			aborted = true
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	})
	id, err := c.CreateMultipart("pitr/db/base/x")
	if err != nil || id != "u-1" {
		t.Fatalf("create: %q %v", id, err)
	}
	u, err := c.PresignPart("pitr/db/base/x", id, 2, time.Hour)
	if err != nil || !strings.Contains(u, "partNumber=2") || !strings.Contains(u, "uploadId=u-1") || !strings.Contains(u, "X-Amz-Signature=") {
		t.Fatalf("presign: %s %v", u, err)
	}
	if err := c.CompleteMultipart("pitr/db/base/x", id, []Part{{3, `"c"`}, {1, `"a"`}, {2, `"b"`}}); err != nil {
		t.Fatal(err)
	}
	i1, i2, i3 := strings.Index(completed, "a&#34;"), strings.Index(completed, "b&#34;"), strings.Index(completed, "c&#34;")
	if i1 < 0 || i1 > i2 || i2 > i3 || !strings.Contains(completed, "<PartNumber>1</PartNumber>") {
		t.Fatalf("parts not in order: %s", completed)
	}
	if err := c.AbortMultipart("pitr/db/base/x", id); err != nil || !aborted {
		t.Fatalf("abort: %v %v", aborted, err)
	}
}

func TestCompleteMultipartErrorBody(t *testing.T) {
	c := fakeS3(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<Error><Code>InternalError</Code></Error>`))
	})
	if err := c.CompleteMultipart("x", "u", []Part{{1, "a"}}); err == nil {
		t.Fatal("an error document must fail the upload")
	}
}
