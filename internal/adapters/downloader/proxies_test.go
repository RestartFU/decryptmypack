package downloader

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProxiesFormats(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxies.txt")
	contents := "\n# comment\nplain.example:1080\nuser:pass:old.example:1081\nevomi.example:12345:user2:pass2\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write proxies: %v", err)
	}

	got, err := loadProxies(path)
	if err != nil {
		t.Fatalf("load proxies: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}

	if got[0].IP != "plain.example" || got[0].Port != "1080" || got[0].Auth {
		t.Fatalf("plain proxy parsed wrong: %+v", got[0])
	}
	if got[1].Username != "user" || got[1].Password != "pass" || got[1].IP != "old.example" || got[1].Port != "1081" || !got[1].Auth {
		t.Fatalf("username:password:host:port parsed wrong: %+v", got[1])
	}
	if got[2].IP != "evomi.example" || got[2].Port != "12345" || got[2].Username != "user2" || got[2].Password != "pass2" || !got[2].Auth {
		t.Fatalf("host:port:username:password parsed wrong: %+v", got[2])
	}
}
