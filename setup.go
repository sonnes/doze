package main

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
func setup() error {
	if runtime.GOOS != "darwin" {
		return errors.New("doze setup supports only macOS")
	}
	if _, err := LoadCA(home()); err != nil {
		return err
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
	for _, k := range []string{"DOZE_HOME", "DOZE_ADDR", "DOZE_HTTPS_ADDR"} {
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
	return nil
}

// uninstall removes the launchd agent and the trust of the CA. It keeps the
// CA files and apps.json.
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
	// These commands fail when the CA is not in the keychain. That is the goal.
	exec.Command("security", "remove-trusted-cert", CAFile(home())).Run()
	sum := fmt.Sprintf("%X", sha1.Sum(ca.Cert.Raw))
	exec.Command("security", "delete-certificate", "-Z", sum, loginKeychain()).Run()
	fmt.Println("doze: removed the CA from the login keychain")
	return nil
}
