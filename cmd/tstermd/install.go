package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"tailscale.com/client/local"
)

// unitName is the systemd user unit tstermd install writes.
const unitName = "tstermd.service"

const darwinTODO = "Sorry, macOS + launchd support is a TODO still."

// usage describes tstermd's commands, and how it's installed here.
func usage(w io.Writer) {
	fmt.Fprint(w, `tstermd is tsterm's server. It runs terminal sessions on this machine, for
you to use from your other devices over Tailscale: in a browser, or with
the tsterm command.

Commands:
  tstermd install [flags]   run it as a systemd user service, started at boot
  tstermd uninstall         stop and remove the service; sessions keep running
  tstermd run [flags]       run it in the foreground
  tstermd run -h            list the flags

To upgrade, go install a new tstermd, then run tstermd install again.
Sessions survive it.
`)
	if runtime.GOOS == "darwin" {
		fmt.Fprintf(w, "\n%s tstermd run might work in the foreground,\nbut it isn't tested there.\n", darwinTODO)
		return
	}
	if exe, err := os.Executable(); err == nil && goRunBuild(exe) {
		fmt.Fprintf(w, "\nThis is a temporary build, from go run. To install tstermd, first:\n\n  go install github.com/bradfitz/tsterm/cmd/tstermd@latest\n")
		return
	}
	if runtime.GOOS == "linux" {
		fmt.Fprintf(w, "\nHere: %s\n", installState())
	}
}

// installState describes whether tstermd is installed and running.
func installState() string {
	path, err := unitPath()
	if err != nil {
		return "can't tell whether tstermd is installed: " + err.Error()
	}
	active := strings.TrimSpace(systemctlOutput("is-active", unitName))
	if _, err := os.Stat(path); err != nil {
		if active == "active" {
			return "tstermd is running, but not installed: run tstermd install."
		}
		return "tstermd isn't installed: run tstermd install."
	}
	return fmt.Sprintf("tstermd is installed (%s), and %s.", path, active)
}

// install installs tstermd as a systemd user service, running tstermd run
// with args, or updates it, and (re)starts it. It's for upgrades, too: it
// restarts tstermd with this binary, and sessions survive.
func install(args []string) {
	requireLinux()
	f := newRunFlags("tstermd install")
	f.fs.Parse(args) // to check them
	if f.fs.NArg() > 0 {
		fatalf("unexpected arguments %q", f.fs.Args())
	}
	if os.Getuid() == 0 {
		fatalf("run tstermd install as the user whose sessions they'll be, not root; installing for root isn't supported yet")
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fatalf("finding this program: %v", err)
	}
	if goRunBuild(exe) {
		fatalf("this is a temporary build, from go run. Install tstermd first:\n\n  go install github.com/bradfitz/tsterm/cmd/tstermd@latest\n\nand then run tstermd install.")
	}
	if out, err := systemctl("show-environment"); err != nil {
		fatalf("tstermd install needs systemd's user manager (systemctl --user), which isn't reachable: %v\n%s", err, out)
	}

	var warnings []string
	warnings = append(warnings, checkLinger()...)
	var url string
	if !*f.useTSNet {
		var w []string
		url, w = checkTailscale(*f.port)
		warnings = append(warnings, w...)
	}

	path, err := unitPath()
	if err != nil {
		fatalf("%v", err)
	}
	old, _ := os.ReadFile(path)
	unit := unitFile(exe, args)
	if string(old) != unit {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fatalf("%v", err)
		}
		if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
			fatalf("%v", err)
		}
	}
	// A transient unit of the same name, as from systemd-run, would hide
	// the installed one.
	if strings.TrimSpace(systemctlOutput("show", unitName, "-p", "Transient", "--value")) == "yes" {
		mustSystemctl("stop", unitName)
	}
	mustSystemctl("daemon-reload")
	mustSystemctl("enable", "--quiet", unitName)
	mustSystemctl("restart", unitName)
	if !waitActive(10 * time.Second) {
		out, _ := exec.Command("journalctl", "--user", "-u", unitName, "-n", "20", "--no-pager").CombinedOutput()
		fatalf("tstermd didn't start. Its log:\n%s", out)
	}

	verb := "Installed"
	if len(old) > 0 {
		verb = "Restarted"
	}
	fmt.Printf("%s tstermd %s, as the systemd user service %s.\n", verb, version(), unitName)
	switch {
	case *f.useTSNet:
		fmt.Printf("\nIt joins your tailnet itself. The first time, it logs a URL to visit to\nlog it in; see it with:\n\n  journalctl --user -u %s -f\n", unitName)
	case url != "":
		fmt.Printf("\nUse it from your other devices on your tailnet, at:\n\n  %s\n", url)
	}
	fmt.Printf(`
Or with the tsterm command, which you can install with:

  go install github.com/bradfitz/tsterm/cmd/tsterm@latest

To upgrade, go install a new tstermd, then run tstermd install again.
Its log: journalctl --user -u %s
`, unitName)
	if len(warnings) > 0 {
		fmt.Println()
		for _, w := range warnings {
			fmt.Printf("Note: %s\n", w)
		}
	}
}

// uninstall stops tstermd and removes its service. Sessions keep running,
// each in its own unit, and their history stays on disk.
func uninstall(args []string) {
	requireLinux()
	if len(args) > 0 {
		fatalf("tstermd uninstall takes no arguments")
	}
	path, err := unitPath()
	if err != nil {
		fatalf("%v", err)
	}
	_, statErr := os.Stat(path)
	active := strings.TrimSpace(systemctlOutput("is-active", unitName)) == "active"
	if statErr != nil && !active {
		fmt.Println("tstermd isn't installed.")
		return
	}
	systemctl("disable", "--now", "--quiet", unitName)
	systemctl("stop", unitName) // if it's transient
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fatalf("%v", err)
	}
	mustSystemctl("daemon-reload")
	fmt.Println("Stopped tstermd and removed its service.")
	if shims := runningShims(); shims > 0 {
		fmt.Printf("\n%d sessions are still running, each in its own systemd unit, tsterm-pty-*.\nA tstermd started later finds them again. To end them:\n\n  systemctl --user stop 'tsterm-pty-*'\n", shims)
	}
	fmt.Printf("\nSessions' history and saved screens are in %s;\ndelete it to remove them.\n", defaultSessionsDir())
}

// requireLinux exits, explaining, unless this is Linux.
func requireLinux() {
	switch runtime.GOOS {
	case "linux":
		return
	case "darwin":
		fatalf("%s", darwinTODO)
	}
	fatalf("installing as a service on %s isn't supported; try tstermd run", runtime.GOOS)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "tstermd: "+format+"\n", args...)
	os.Exit(1)
}

// goRunBuild reports whether exe is a temporary build, from go run.
func goRunBuild(exe string) bool {
	return strings.Contains(exe, string(filepath.Separator)+"go-build")
}

// version describes this build of tstermd.
func version() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown version)"
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev, dirty string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	if rev == "" {
		return "(devel)"
	}
	return rev[:min(len(rev), 12)] + dirty
}

// unitPath returns where tstermd install writes its unit.
func unitPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user", unitName), nil
}

// unitFile returns the systemd unit that runs tstermd run, from exe, with
// args.
func unitFile(exe string, args []string) string {
	cmd := []string{systemdQuote(exe), "run"}
	for _, a := range args {
		cmd = append(cmd, systemdQuote(a))
	}
	return `# tstermd's systemd user unit, written by "tstermd install". Run that
# again to change its flags, or after upgrading tstermd; "tstermd
# uninstall" removes it.
[Unit]
Description=tsterm daemon
Documentation=https://github.com/bradfitz/tsterm
After=network-online.target

[Service]
ExecStart=` + strings.Join(cmd, " ") + `
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=default.target
`
}

// systemdQuote quotes s as one word of a systemd command line.
func systemdQuote(s string) string {
	s = strings.NewReplacer("%", "%%", "$", "$$").Replace(s)
	if s != "" && !strings.ContainsAny(s, " \t\n\"'\\;") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

// systemctl runs systemctl --user with args.
func systemctl(args ...string) (string, error) {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	return string(out), err
}

func systemctlOutput(args ...string) string {
	out, _ := systemctl(args...)
	return out
}

func mustSystemctl(args ...string) {
	if out, err := systemctl(args...); err != nil {
		fatalf("systemctl --user %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// waitActive waits up to d for tstermd's unit to be active, and to stay
// so for a moment, as a crash would end it.
func waitActive(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.TrimSpace(systemctlOutput("is-active", unitName)) == "active" {
			time.Sleep(time.Second)
			return strings.TrimSpace(systemctlOutput("is-active", unitName)) == "active"
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// runningShims returns how many sessions' shim units are running.
func runningShims() int {
	out := strings.TrimSpace(systemctlOutput("list-units", "--plain", "--no-legend", "--state=active", "tsterm-pty-*"))
	if out == "" {
		return 0
	}
	return len(strings.Split(out, "\n"))
}

// checkLinger makes sure the user's systemd manager runs without a login
// session, so that tstermd starts at boot and sessions survive logging
// out, enabling lingering if it can. It returns warnings.
func checkLinger() []string {
	u, err := user.Current()
	if err != nil {
		return []string{fmt.Sprintf("can't check lingering: %v", err)}
	}
	linger := func() bool {
		out, _ := exec.Command("loginctl", "show-user", u.Username, "-p", "Linger", "--value").Output()
		return strings.TrimSpace(string(out)) == "yes"
	}
	if linger() {
		return nil
	}
	if err := exec.Command("loginctl", "enable-linger").Run(); err == nil && linger() {
		fmt.Println("Enabled lingering (loginctl enable-linger), so that tstermd starts at boot\nand sessions survive logging out.")
		return nil
	}
	return []string{fmt.Sprintf("until lingering is enabled, tstermd won't start at boot, and stops when\nyou log out, ending sessions. Enable it with:\n\n  sudo loginctl enable-linger %s\n", u.Username)}
}

// checkTailscale checks that tailscaled is up, for tstermd to serve on,
// and returns the web UI's URL and warnings.
func checkTailscale(port int) (url string, warnings []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var lc local.Client
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		fatalf("can't reach Tailscale (%v). Install it and run tailscale up, or\nhave tstermd join your tailnet itself with: tstermd install -tsnet", err)
	}
	if st.BackendState != "Running" {
		fatalf("Tailscale isn't connected (it's %s). Run tailscale up first.", st.BackendState)
	}
	fqdn := strings.TrimSuffix(st.Self.DNSName, ".")
	https := false
	for _, d := range st.CertDomains {
		https = https || d == fqdn
	}
	if fqdn == "" && len(st.Self.TailscaleIPs) > 0 {
		fqdn = st.Self.TailscaleIPs[0].String()
	}
	if https {
		url = fmt.Sprintf("https://%s:%d/", fqdn, port)
		if prefs, err := lc.GetPrefs(ctx); err == nil {
			if u, err := user.Current(); err == nil && prefs.OperatorUser != u.Username {
				warnings = append(warnings, fmt.Sprintf("Tailscale won't give tstermd HTTPS certificates until you're its operator:\n\n  sudo tailscale set --operator=%s\n", u.Username))
			}
		}
	} else {
		url = fmt.Sprintf("http://%s:%d/", fqdn, port)
		warnings = append(warnings, "HTTPS certificates are off for your tailnet, so the web UI is HTTP only,\nand browsers won't show its desktop notifications. Turn on MagicDNS and\nHTTPS Certificates at https://login.tailscale.com/admin/dns, then run\ntstermd install again.")
	}
	return url, warnings
}
