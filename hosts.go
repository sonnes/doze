// doze keeps a block of lines in /etc/hosts that sends each
// <name>.<domain> host to the loopback address. Browsers send *.localhost
// to 127.0.0.1 without DNS, but a custom domain needs these lines.

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

const (
	hostsBegin = "# doze begin: doze hosts writes these lines"
	hostsEnd   = "# doze end"
)

// hostsFile returns the path of the hosts file. Tests set DOZE_HOSTS_FILE.
func hostsFile() string {
	if p := os.Getenv("DOZE_HOSTS_FILE"); p != "" {
		return p
	}
	return "/etc/hosts"
}

// hostsBlock returns content with the doze block set to hosts. It replaces
// the block in place, so that the other lines stay where they are. With no
// hosts, it removes the block and the blank line before it.
func hostsBlock(content string, hosts []string) string {
	hosts = slices.Clone(hosts)
	slices.Sort(hosts)
	hosts = slices.Compact(hosts)
	var block strings.Builder
	if len(hosts) > 0 {
		block.WriteString(hostsBegin + "\n")
		for _, h := range hosts {
			fmt.Fprintf(&block, "127.0.0.1 %s\n::1 %s\n", h, h)
		}
		block.WriteString(hostsEnd + "\n")
	}

	lines := strings.SplitAfter(content, "\n")
	begin := slices.IndexFunc(lines, func(l string) bool { return strings.TrimSpace(l) == hostsBegin })
	if begin < 0 {
		if len(hosts) == 0 {
			return content
		}
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		return content + "\n" + block.String()
	}
	end := len(lines) - 1
	for i := begin; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == hostsEnd {
			end = i
			break
		}
	}
	before := lines[:begin]
	if len(hosts) == 0 && len(before) > 0 && strings.TrimSpace(before[len(before)-1]) == "" {
		before = before[:len(before)-1]
	}
	return strings.Join(before, "") + block.String() + strings.Join(lines[end+1:], "")
}

// syncHosts sets the doze block of the hosts file to <name>.<domain> for
// each route of the daemon that has a domain.
func syncHosts(c *Client) error {
	apps, err := c.List()
	if err != nil {
		return err
	}
	var hosts []string
	for _, a := range apps {
		if a.Domain != "" {
			hosts = append(hosts, a.Name+"."+a.Domain)
		}
	}
	return updateHosts(hosts)
}

// updateHosts writes the doze block of the hosts file when it changes.
func updateHosts(hosts []string) error {
	path := hostsFile()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	next := hostsBlock(string(data), hosts)
	if next == string(data) {
		return nil
	}
	return writeHosts(path, []byte(next))
}

// writeHosts writes the hosts file. /etc/hosts belongs to root, so when a
// direct write is not allowed, writeHosts copies a temporary file with sudo.
// sudo asks for the password on the terminal.
func writeHosts(path string, data []byte) error {
	err := os.WriteFile(path, data, 0o644)
	if !errors.Is(err, os.ErrPermission) {
		return err
	}
	tmp, err := os.CreateTemp("", "doze-hosts")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "doze: sudo updates %s\n", path)
	if err := sudo("cp", tmp.Name(), path); err != nil {
		return err
	}
	// macOS caches lookups. The new lines work only after mDNSResponder
	// reloads.
	if runtime.GOOS == "darwin" && filepath.Clean(path) == "/etc/hosts" {
		sudo("killall", "-HUP", "mDNSResponder")
	}
	return nil
}

func sudo(args ...string) error {
	cmd := exec.Command("sudo", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sudo %s: %w", strings.Join(args, " "), err)
	}
	return nil
}
