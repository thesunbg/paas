package util

import "testing"

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Nguyễn Văn An":      "nguyen-van-an",
		"Đỗ Thị Hương":       "do-thi-huong",
		"Lớp n8n K1":         "lop-n8n-k1",
		"  My App!! ":        "my-app",
		"":                   "app",
		"Crème brûlée café":  "creme-brulee-cafe",
		"already-a-slug-123": "already-a-slug-123",
		"日本":                 "app",
	} {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
