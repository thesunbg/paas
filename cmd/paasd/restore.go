package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/s3"
	"github.com/thesunbg/paas/internal/secrets"
	"github.com/thesunbg/paas/internal/statebackup"
)

const restoreUsage = `usage: paasd restore-state [flags]

Puts a backup of the platform state (the control-plane database) back.
Stop paasd first (systemctl stop paasd) and start it again afterwards.

Source, one of:
  --file PATH        a sealed backup file (downloaded from Settings → Backups)
  --latest           the newest backup in the off-site storage
  --name NAME        a given backup in the off-site storage (state-YYYYMMDD-HHMMSS.pgstate)
  --list             list the backups in the off-site storage and exit

Off-site storage (default: PAAS_S3_* in the environment):
  --s3-endpoint URL --s3-bucket NAME --s3-access-key KEY --s3-secret-key SECRET
  --s3-region REGION (auto) --s3-prefix PREFIX (paas-backups) --s3-virtual-host

Target:
  --database-url URL   default PAAS_DATABASE_URL, else /etc/paas/db.env
  --master-key PATH    default PAAS_MASTER_KEY, else $PAAS_DATA_DIR/master.key (/var/lib/paas)
`

func runRestoreState(args []string) error {
	fs := flag.NewFlagSet("restore-state", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, restoreUsage) }
	file := fs.String("file", "", "")
	latest := fs.Bool("latest", false, "")
	name := fs.String("name", "", "")
	list := fs.Bool("list", false, "")
	dbURL := fs.String("database-url", os.Getenv("PAAS_DATABASE_URL"), "")
	keyPath := fs.String("master-key", "", "")
	c := s3.Config{
		Endpoint: os.Getenv("PAAS_S3_ENDPOINT"), Region: envOr("PAAS_S3_REGION", "auto"), Bucket: os.Getenv("PAAS_S3_BUCKET"),
		AccessKey: os.Getenv("PAAS_S3_ACCESS_KEY"), SecretKey: os.Getenv("PAAS_S3_SECRET_KEY"), Prefix: envOr("PAAS_S3_PREFIX", "paas-backups"),
		VirtualHost: os.Getenv("PAAS_S3_VIRTUAL_HOST") == "true",
	}
	fs.StringVar(&c.Endpoint, "s3-endpoint", c.Endpoint, "")
	fs.StringVar(&c.Bucket, "s3-bucket", c.Bucket, "")
	fs.StringVar(&c.AccessKey, "s3-access-key", c.AccessKey, "")
	fs.StringVar(&c.SecretKey, "s3-secret-key", c.SecretKey, "")
	fs.StringVar(&c.Region, "s3-region", c.Region, "")
	fs.StringVar(&c.Prefix, "s3-prefix", c.Prefix, "")
	fs.BoolVar(&c.VirtualHost, "s3-virtual-host", c.VirtualHost, "")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *list || *latest || *name != "" {
		if !c.Enabled() {
			return errors.New("the off-site storage is not configured: pass --s3-endpoint, --s3-bucket, --s3-access-key and --s3-secret-key")
		}
	}
	var names []string
	if *list || *latest {
		objs, err := c.List(c.Key("state") + "/")
		if err != nil {
			return err
		}
		for _, o := range objs {
			if strings.HasSuffix(o.Key, ".pgstate") {
				names = append(names, filepath.Base(o.Key))
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(names)))
	}
	if *list {
		for _, n := range names {
			fmt.Println(n)
		}
		return nil
	}

	var src io.ReadCloser
	label := ""
	switch {
	case *file != "":
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		src, label = f, *file
	case *latest || *name != "":
		n := *name
		if *latest {
			if len(names) == 0 {
				return errors.New("there is no backup in the off-site storage")
			}
			n = names[0]
		}
		rc, err := c.Get(c.Key("state", n))
		if err != nil {
			return err
		}
		src, label = rc, n
	default:
		fs.Usage()
		return errors.New("choose --file, --latest or --name")
	}
	defer src.Close()

	if *dbURL == "" {
		*dbURL = readEnvFile("/etc/paas/db.env", "PAAS_DATABASE_URL")
	}
	if *dbURL == "" {
		return errors.New("no database: pass --database-url or set PAAS_DATABASE_URL")
	}
	if *keyPath == "" {
		*keyPath = filepath.Join(envOr("PAAS_DATA_DIR", "/var/lib/paas"), "master.key")
	}
	key, err := secrets.LoadKey(*keyPath, os.Getenv("PAAS_MASTER_KEY"))
	if err != nil {
		return fmt.Errorf("master key: %w (restore it from where you kept it before restoring the state)", err)
	}
	if resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://127.0.0.1:8080/healthz"); err == nil {
		resp.Body.Close()
		return errors.New("paasd is running on this machine: stop it first (systemctl stop paasd)")
	}

	tmp, err := os.CreateTemp("", "paas-state-*.dump")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	fmt.Println("decrypting", label)
	if err := statebackup.Open(tmp, src, key); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	bin := "pg_restore"
	if m, _ := filepath.Glob("/usr/lib/postgresql/*/bin/pg_restore"); len(m) > 0 {
		sort.Strings(m)
		bin = m[len(m)-1]
	}
	fmt.Println("restoring into the control-plane database")
	cmd := exec.Command(bin, "--clean", "--if-exists", "--no-owner", "--no-privileges", "--single-transaction", "--exit-on-error", "--dbname="+*dbURL, tmp.Name())
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_restore: %w", err)
	}
	fmt.Println("done: start paasd again (systemctl start paasd)")
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// readEnvFile reads one KEY=value from a systemd environment file.
func readEnvFile(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok && k == key {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}
