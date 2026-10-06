package main

import (
	"bytes"
	"crypto/sha1"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const serviceLabel = "com.github.sonnes.doze"

func plistPath() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "Library", "LaunchAgents", serviceLabel+".plist")
}

func loginKeychain() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "Library", "Keychains", "login.keychain-db")
}

func launchdTarget() string { return "gui/" + strconv.Itoa(os.Getuid()) }

// setup trusts the CA of doze in the login keychain and starts the daemon at
// login with launchd. macOS asks for the password of the user to trust the CA.
// If --domain names a domain that the CA does not permit, setup replaces
// the CA and removes the trust of the old CA. Then it writes the hosts file.
func setup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	domain := fs.String("domain", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	*domain = strings.ToLower(*domain)
	if err := validDomain(*domain); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		return errors.New("doze setup supports only macOS")
	}
	old, err := LoadCA(home())
	if err != nil {
		return err
	}
	ca, err := EnsureCA(home(), *domain)
	if err != nil {
		return err
	}
	if !bytes.Equal(old.Cert.Raw, ca.Cert.Raw) {
		untrust(old.Cert)
		fmt.Printf("doze: made a new CA for %s\n", strings.Join(ca.Cert.PermittedDNSDomains, ", "))
	}
	trust := exec.Command(
		"security", "add-trusted-cert",
		"-r", "trustRoot",
		"-k", loginKeychain(),
		CAFile(home()),
	)
	if out, err := trust.CombinedOutput(); err != nil {
		return fmt.Errorf("trust the CA: %v: %s", err, out)
	}
	fmt.Printf("doze: trusted %s\n", CAFile(home()))

	target := launchdTarget()
	exec.Command("launchctl", "bootout", target+"/"+serviceLabel).Run()
	// bootout returns before launchd removes the service, and bootstrap fails
	// with error 5 while the old service is still there.
	for range 100 {
		if exec.Command("launchctl", "print", target+"/"+serviceLabel).Run() != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// launchd must own the daemon, so stop a daemon that a command started.
	if err := stopDaemon(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	env := ""
	for _, k := range []string{"DOZE_HOME", "DOZE_ADDR", "DOZE_HTTPS_ADDR", "DOZE_HOSTS_FILE"} {
		if v := os.Getenv(k); v != "" {
			env += fmt.Sprintf("\n\t\t<key>%s</key><string>%s</string>", k, html.EscapeString(v))
		}
	}
	log := html.EscapeString(filepath.Join(home(), "daemon.log"))
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array><string>%s</string><string>daemon</string></array>
	<key>EnvironmentVariables</key>
	<dict>%s
	</dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key>
	<dict><key>SuccessfulExit</key><false/></dict>
	<key>StandardOutPath</key><string>%s</string>
	<key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, serviceLabel, html.EscapeString(exe), env, log, log)

	if err := os.MkdirAll(filepath.Dir(plistPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plistPath(), []byte(plist), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("launchctl", "bootstrap", target, plistPath()).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	fmt.Printf("doze: installed %s. The daemon starts at login.\n", plistPath())

	// connect would start a second daemon, so wait for the one that launchd
	// starts.
	c := NewClient(sockPath())
	for range 100 {
		if c.Ping() {
			warnHosts(syncHosts(c))
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	warnHosts(errors.New("the daemon did not answer"))
	return nil
}

// uninstall removes the launchd agent, the trust of the CA, and the doze
// lines of the hosts file. It keeps the CA files and apps.json.
func uninstall() error {
	if runtime.GOOS != "darwin" {
		return errors.New("doze uninstall supports only macOS")
	}
	exec.Command("launchctl", "bootout", launchdTarget()+"/"+serviceLabel).Run()
	if err := os.Remove(plistPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Println("doze: removed the launchd agent")

	ca, err := LoadCA(home())
	if err != nil {
		return err
	}
	untrust(ca.Cert)
	fmt.Println("doze: removed the CA from the login keychain")

	if err := updateHosts(nil); err != nil {
		return err
	}
	fmt.Printf("doze: removed the doze lines from %s\n", hostsFile())
	return nil
}

// untrust removes cert and its trust from the login keychain. The commands
// fail when the cert is not in the keychain. That is the goal, so untrust
// ignores the errors.
func untrust(cert *x509.Certificate) {
	// remove-trusted-cert reads a file, and the CA file can hold a newer CA.
	if f, err := os.CreateTemp("", "doze-ca"); err == nil {
		pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
		f.Close()
		exec.Command("security", "remove-trusted-cert", f.Name()).Run()
		os.Remove(f.Name())
	}
	sum := fmt.Sprintf("%X", sha1.Sum(cert.Raw))
	exec.Command("security", "delete-certificate", "-Z", sum, loginKeychain()).Run()
}
