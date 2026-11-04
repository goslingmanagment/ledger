package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMigrateTwiceAndBackup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.Migrate(ctx)
	if err != nil || len(first) == 0 {
		t.Fatalf("first migrate: %v %v", first, err)
	}
	again, err := s.Migrate(ctx)
	if err != nil || len(again) != 0 {
		t.Fatalf("second migrate applied %v: %v", again, err)
	}
	v, err := SchemaVersion(ctx, s.RO)
	if err != nil || v != first[len(first)-1] {
		t.Fatalf("schema version %q %v", v, err)
	}
	dest := filepath.Join(dir, "copy.db")
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx, dest); err == nil {
		t.Fatal("backup over an existing file must fail")
	}
	c, err := OpenReadOnly(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if v2, _ := SchemaVersion(ctx, c.RO); v2 != v {
		t.Fatalf("copy schema %q != %q", v2, v)
	}
	if _, err := c.RO.Exec(`INSERT INTO meta(key, value) VALUES('x', 'y')`); err == nil {
		t.Fatal("read-only pool accepted a write")
	}
}
