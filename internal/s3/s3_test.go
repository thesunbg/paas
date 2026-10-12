package s3

import (
	"net/http"
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
