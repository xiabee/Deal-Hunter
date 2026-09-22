package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// backup.stamp is written by deploy/backup.sh and read by two callers with different
// policies (doctor warns, the sweep refuses to delete). The *reading* must live here
// once, so the two can never disagree about what "recently backed up" means.
func TestBackupAgeReadsTheStampEverywhere(t *testing.T) {
	stamp := func(age time.Duration) string {
		return time.Now().UTC().Add(-age).Format("2006-01-02T15:04:05Z")
	}
	cases := []struct {
		name     string
		content  *string // nil = no file at all
		wantErr  bool
		sentinel bool
		archive  string
		fresh    bool
	}{
		{"没有文件", nil, true, true, "", false},
		{"空文件", ptr(""), true, false, "", false},
		{"首字段不是时刻", ptr("胡说八道 deal-hunter-x.tar.gz"), true, false, "", false},
		{"两小时前的成功备份", ptr(stamp(2*time.Hour) + " deal-hunter-20260922.tar.gz"), false, false, "deal-hunter-20260922.tar.gz", true},
		{"刚好在耐心线内", ptr(stamp(BackupPatience-time.Hour) + " a.tar.gz"), false, false, "a.tar.gz", true},
		{"超过耐心线", ptr(stamp(BackupPatience+time.Hour) + " b.tar.gz"), false, false, "b.tar.gz", false},
		{"只有时刻没有归档名", ptr(stamp(time.Minute)), false, false, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.content != nil {
				if err := os.WriteFile(filepath.Join(dir, "backup.stamp"), []byte(*tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			age, archive, err := BackupAge(dir)
			switch {
			case tc.wantErr && err == nil:
				t.Fatal("expected an error, got none")
			case !tc.wantErr && err != nil:
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.sentinel != (err != nil && errors.Is(err, ErrNoBackupRecord)) {
				t.Errorf("ErrNoBackupRecord = %v, want %v (err=%v)", errors.Is(err, ErrNoBackupRecord), tc.sentinel, err)
			}
			if archive != tc.archive {
				t.Errorf("archive = %q, want %q", archive, tc.archive)
			}
			if err == nil && (age > BackupPatience) == tc.fresh {
				t.Errorf("age %v classified wrong: fresh=%v", age, tc.fresh)
			}
		})
	}
}

func ptr(s string) *string { return &s }
