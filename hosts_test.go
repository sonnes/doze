package main

import (
	"strings"
	"testing"
)

func TestHostsBlock(t *testing.T) {
	base := "127.0.0.1 localhost\n::1 localhost\n"

	got := hostsBlock(base, []string{"web.dev.test", "api.dev.test"})
	want := base + "\n" + hostsBegin + "\n" +
		"127.0.0.1 api.dev.test\n::1 api.dev.test\n" +
		"127.0.0.1 web.dev.test\n::1 web.dev.test\n" +
		hostsEnd + "\n"
	if got != want {
		t.Fatalf("add:\n%s\nwant:\n%s", got, want)
	}

	replaced := hostsBlock(got+"10.0.0.1 db\n", []string{"api.dev.test"})
	if strings.Contains(replaced, "web.dev.test") || !strings.Contains(replaced, "127.0.0.1 api.dev.test") {
		t.Fatalf("replace:\n%s", replaced)
	}
	if !strings.HasPrefix(replaced, base) || !strings.HasSuffix(replaced, "10.0.0.1 db\n") {
		t.Fatalf("replace changed the other lines:\n%s", replaced)
	}
	if strings.Count(replaced, hostsBegin) != 1 {
		t.Fatalf("replace left two blocks:\n%s", replaced)
	}

	if removed := hostsBlock(got, nil); removed != base {
		t.Fatalf("remove:\n%q\nwant:\n%q", removed, base)
	}
}
