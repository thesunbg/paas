// Package s3 creates AWS Signature V4 presigned URLs for S3-compatible storage
// (AWS S3, Cloudflare R2, Backblaze B2, MinIO, Garage…). Agents upload and
// download with these URLs, so storage credentials never leave the control plane.
package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Config struct {
	Endpoint  string // e.g. https://s3.eu-central-1.amazonaws.com or https://<account>.r2.cloudflarestorage.com
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	Prefix    string
	// VirtualHost uses bucket.endpoint addressing instead of endpoint/bucket.
	VirtualHost bool
}

func (c *Config) Enabled() bool {
	return c != nil && c.Endpoint != "" && c.Bucket != "" && c.AccessKey != "" && c.SecretKey != ""
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Presign returns a URL that allows method (GET, PUT, DELETE) on key until expiry.
func (c *Config) Presign(method, key string, expires time.Duration, now time.Time) (string, error) {
	u, err := url.Parse(strings.TrimRight(c.Endpoint, "/"))
	if err != nil {
		return "", err
	}
	host := u.Host
	path := "/" + c.Bucket + "/" + strings.TrimLeft(key, "/")
	if c.VirtualHost {
		host = c.Bucket + "." + u.Host
		path = "/" + strings.TrimLeft(key, "/")
	}
	region := c.Region
	if region == "" {
		region = "us-east-1"
	}
	amzDate := now.UTC().Format("20060102T150405Z")
	day := amzDate[:8]
	scope := day + "/" + region + "/s3/aws4_request"
	q := map[string]string{
		"X-Amz-Algorithm":     "AWS4-HMAC-SHA256",
		"X-Amz-Credential":    c.AccessKey + "/" + scope,
		"X-Amz-Date":          amzDate,
		"X-Amz-Expires":       fmt.Sprint(int(expires.Seconds())),
		"X-Amz-SignedHeaders": "host",
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var cq []string
	for _, k := range keys {
		cq = append(cq, uriEncode(k, true)+"="+uriEncode(q[k], true))
	}
	canonicalQuery := strings.Join(cq, "&")
	canonical := strings.Join([]string{method, uriEncode(path, false), canonicalQuery, "host:" + host, "", "host", "UNSIGNED-PAYLOAD"}, "\n")
	sum := sha256.Sum256([]byte(canonical))
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(sum[:])}, "\n")
	k := hmacSHA256([]byte("AWS4"+c.SecretKey), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	return fmt.Sprintf("%s://%s%s?%s&X-Amz-Signature=%s", u.Scheme, host, uriEncode(path, false), canonicalQuery, sig), nil
}

// Delete removes an object (called from the control plane).
func (c *Config) Delete(key string) error {
	u, err := c.Presign(http.MethodDelete, key, 5*time.Minute, time.Now())
	if err != nil {
		return err
	}
	req, _ := http.NewRequest(http.MethodDelete, u, nil)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("delete %s: HTTP %d", key, resp.StatusCode)
	}
	return nil
}

// Key builds an object key under the configured prefix.
func (c *Config) Key(parts ...string) string {
	return strings.TrimLeft(strings.TrimRight(c.Prefix, "/")+"/"+strings.Join(parts, "/"), "/")
}
