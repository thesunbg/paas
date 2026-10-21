// Package s3 creates AWS Signature V4 presigned URLs for S3-compatible storage
// (AWS S3, Cloudflare R2, Backblaze B2, MinIO, Garage…). Agents upload and
// download with these URLs, so storage credentials never leave the control plane.
package s3

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
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
	return c.presign(method, key, nil, expires, now)
}

// presign signs extra query parameters (uploadId, partNumber, list-type…) too.
func (c *Config) presign(method, key string, extra map[string]string, expires time.Duration, now time.Time) (string, error) {
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
	for k, v := range extra {
		q[k] = v
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

var httpClient = &http.Client{Timeout: 30 * time.Second}

// transferClient moves whole objects (Put, Get): no overall deadline, the
// body can take long; a server that does not answer still times out.
var transferClient = &http.Client{Transport: &http.Transport{
	Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: time.Minute, TLSHandshakeTimeout: 15 * time.Second,
}}

// call runs a signed request against the storage and returns the body of a 2xx reply.
func (c *Config) call(method, key string, q map[string]string, body []byte) ([]byte, error) {
	u, err := c.presign(method, key, q, 5*time.Minute, time.Now())
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: HTTP %d %s", method, key, resp.StatusCode, bytes.TrimSpace(out[:min(len(out), 256)]))
	}
	return out, nil
}

type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// List returns every object whose key starts with prefix (a full key, see Key).
func (c *Config) List(prefix string) ([]Object, error) {
	var objs []Object
	token := ""
	for {
		q := map[string]string{"list-type": "2", "prefix": prefix}
		if token != "" {
			q["continuation-token"] = token
		}
		body, err := c.call(http.MethodGet, "", q, nil)
		if err != nil {
			return nil, err
		}
		var res struct {
			Truncated bool   `xml:"IsTruncated"`
			Next      string `xml:"NextContinuationToken"`
			Contents  []struct {
				Key          string    `xml:"Key"`
				Size         int64     `xml:"Size"`
				LastModified time.Time `xml:"LastModified"`
			} `xml:"Contents"`
		}
		if err := xml.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, o := range res.Contents {
			objs = append(objs, Object{Key: o.Key, Size: o.Size, LastModified: o.LastModified})
		}
		if !res.Truncated || res.Next == "" {
			return objs, nil
		}
		token = res.Next
	}
}

// CreateMultipart starts a multipart upload and returns its upload ID.
func (c *Config) CreateMultipart(key string) (string, error) {
	body, err := c.call(http.MethodPost, key, map[string]string{"uploads": ""}, nil)
	if err != nil {
		return "", err
	}
	var res struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(body, &res); err != nil || res.UploadID == "" {
		return "", fmt.Errorf("create multipart %s: unexpected reply", key)
	}
	return res.UploadID, nil
}

// PresignPart returns a URL an agent PUTs part n (1-based) to.
func (c *Config) PresignPart(key, uploadID string, n int, expires time.Duration) (string, error) {
	return c.presign(http.MethodPut, key, map[string]string{"partNumber": fmt.Sprint(n), "uploadId": uploadID}, expires, time.Now())
}

type Part struct {
	N    int
	ETag string
}

// CompleteMultipart assembles the parts, listed in part order.
func (c *Config) CompleteMultipart(key, uploadID string, parts []Part) error {
	type part struct {
		N    int    `xml:"PartNumber"`
		ETag string `xml:"ETag"`
	}
	req := struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []part   `xml:"Part"`
	}{}
	for _, p := range parts {
		req.Parts = append(req.Parts, part{p.N, p.ETag})
	}
	sort.Slice(req.Parts, func(i, j int) bool { return req.Parts[i].N < req.Parts[j].N })
	body, err := xml.Marshal(req)
	if err != nil {
		return err
	}
	out, err := c.call(http.MethodPost, key, map[string]string{"uploadId": uploadID}, body)
	if err != nil {
		return err
	}
	// S3 may answer 200 with an error document when assembling fails.
	if bytes.Contains(out, []byte("<Error>")) {
		return fmt.Errorf("complete multipart %s: %s", key, bytes.TrimSpace(out[:min(len(out), 256)]))
	}
	return nil
}

// AbortMultipart discards the uploaded parts.
func (c *Config) AbortMultipart(key, uploadID string) error {
	_, err := c.call(http.MethodDelete, key, map[string]string{"uploadId": uploadID}, nil)
	return err
}

// Put uploads an object in a single request (S3 allows up to 5 GB).
func (c *Config) Put(key string, body io.Reader, size int64) error {
	u, err := c.Presign(http.MethodPut, key, 30*time.Minute, time.Now())
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, u, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	resp, err := transferClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("PUT %s: HTTP %d %s", key, resp.StatusCode, bytes.TrimSpace(out))
	}
	return nil
}

// Get opens an object for reading; the caller closes it.
func (c *Config) Get(key string) (io.ReadCloser, error) {
	u, err := c.Presign(http.MethodGet, key, 30*time.Minute, time.Now())
	if err != nil {
		return nil, err
	}
	resp, err := transferClient.Get(u)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: HTTP %d %s", key, resp.StatusCode, bytes.TrimSpace(out))
	}
	return resp.Body, nil
}
