package diagnostics

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

// An answer larger than one protocol message would end the session instead of
// reaching the operator.
func TestAnswerFitsOneProtocolMessage(t *testing.T) {
	escaped := strings.Repeat("<\x01é", maxStream/4)
	result := fit(Result{RequestID: protocol.NewUUID(), Code: "completed", Stdout: escaped, Stderr: escaped})
	encoded, err := protocol.Encode(ResultType, result)
	if err != nil || len(encoded) > protocol.MaxMessage {
		t.Fatalf("encoded answer is %d bytes: %v", len(encoded), err)
	}
	if !result.StdoutTruncated || !result.StderrTruncated {
		t.Fatalf("shortened output is not marked truncated: %+v", result)
	}
	if !utf8.ValidString(result.Stdout) || !utf8.ValidString(result.Stderr) {
		t.Fatal("shortened output is not valid UTF-8")
	}
}

// The sandbox is the whole guarantee that a diagnostics script changes nothing
// on a node, so it is checked under the real systemd.
func TestSandboxReadsButChangesNothingAndLeavesNothing(t *testing.T) {
	runner := sandboxRunner(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	marker := "akastr-diagnostics-test-" + protocol.NewUUID()
	script := `
head -c1 /etc/shadow >/dev/null && echo read-ok
touch /etc/` + marker + ` 2>/dev/null || echo write-denied
touch /var/lib/` + marker + ` 2>/dev/null || echo state-denied
kill -0 1 2>/dev/null || echo signal-denied
timeout 3 bash -c "echo > /dev/tcp/127.0.0.1/` + strconv.Itoa(port) + `" 2>/dev/null || echo local-port-denied
touch /tmp/` + marker + ` /var/tmp/` + marker + ` 2>/dev/null || echo tmp-closed
scratch=$(mktemp) && [ "$(stat -f -c %T "$scratch")" = tmpfs ] && echo temp-in-memory
touch /dev/shm/` + marker + ` && echo shm-writable
logger -t ` + marker + ` leaked 2>/dev/null || echo journal-write-denied
journalctl -n1 -q --no-pager >/dev/null && echo journal-read-ok
(exec -a ` + marker + ` sleep 300) &
head -c ` + strconv.Itoa(maxStream+1000) + ` /dev/zero | tr '\0' x
echo to-stderr >&2
exit 3`
	result := answer(t, runner, context.Background(), script)
	if result.Code != "completed" || result.ExitCode == nil || *result.ExitCode != 3 {
		t.Fatalf("result=%+v", summary(result))
	}
	for _, want := range []string{"read-ok", "write-denied", "state-denied", "signal-denied", "local-port-denied",
		"tmp-closed", "temp-in-memory", "shm-writable", "journal-write-denied", "journal-read-ok"} {
		if !strings.Contains(result.Stdout, want) {
			t.Errorf("stdout lacks %q", want)
		}
	}
	if !result.StdoutTruncated || len(result.Stdout) != maxStream || result.Stderr != "to-stderr\n" {
		t.Errorf("output limits not applied: %+v", summary(result))
	}
	for _, path := range []string{"/etc/" + marker, "/var/lib/" + marker, "/tmp/" + marker, "/dev/shm/" + marker} {
		if _, err := os.Stat(path); err == nil {
			os.Remove(path)
			t.Errorf("%s was left behind", path)
		}
	}
	if out, _ := exec.Command("journalctl", "-q", "--no-pager", "-t", marker).Output(); len(out) != 0 {
		t.Errorf("the script wrote to the journal: %s", out)
	}
	if out, _ := exec.Command("pgrep", "-f", marker).Output(); len(out) != 0 {
		t.Errorf("a process outlived the script: %s", out)
	}
	assertNoUnits(t)
}

func TestTimeLimitAndSessionEndStopTheUnit(t *testing.T) {
	runner := sandboxRunner(t)
	runner.timeLimit = 2 * time.Second
	started := time.Now()
	// A script ignoring SIGTERM must not hold the answer past Cloud's wait.
	result := answer(t, runner, context.Background(), `trap "" TERM; echo started; sleep 60`)
	if result.Code != "timed_out" || result.ExitCode != nil || result.Stdout != "started\n" ||
		time.Since(started) > 15*time.Second {
		t.Fatalf("result=%+v", summary(result))
	}
	assertNoUnits(t)

	runner.timeLimit = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	answered := make(chan any, 1)
	go func() {
		answered <- runner.Answer(ctx, request(t, "sleep 60"))
	}()
	waitForUnit(t)
	if busy := answer(t, runner, context.Background(), "true"); busy.Code != "busy" {
		t.Errorf("a second script ran alongside the first: %+v", summary(busy))
	}
	cancel()
	if got := <-answered; got != nil {
		t.Errorf("a script stopped with its session was answered: %+v", got)
	}
	assertNoUnits(t)
}

// Where systemd silently ignores the local-port block, a script could reach
// the passwordless Xray API; it must not run at all.
func TestScriptDoesNotRunWhereLocalPortsAreReachable(t *testing.T) {
	runner := sandboxRunner(t)
	runner.localBlock = nil
	result := answer(t, runner, context.Background(), "echo ran")
	if result.Code != "sandbox_unavailable" || result.Stdout != "" || result.ExitCode != nil {
		t.Fatalf("result=%+v", summary(result))
	}
	assertNoUnits(t)
}

func TestSweeperStopsUnitsOfAnEarlierProcess(t *testing.T) {
	requireSandbox(t)
	unit := unitPrefix + protocol.NewUUID() + ".service"
	if out, err := exec.Command(systemdRun, "--quiet", "--collect", "--unit="+unit, "/bin/sleep", "300").CombinedOutput(); err != nil {
		t.Fatalf("start leftover unit: %v %s", err, out)
	}
	sweeper := NewSweeper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- sweeper.Run(ctx) }()
	select {
	case <-sweeper.done:
	case <-time.After(time.Minute):
		t.Fatal("sweeper did not finish")
	}
	assertNoUnits(t)
	cancel()
	<-stopped
}

func sandboxRunner(t *testing.T) *Runner {
	t.Helper()
	requireSandbox(t)
	sweeper := NewSweeper()
	close(sweeper.done)
	return New(sweeper)
}

func requireSandbox(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("the sandbox needs root and systemd")
	}
	comm, _ := os.ReadFile("/proc/1/comm")
	if strings.TrimSpace(string(comm)) != "systemd" {
		t.Skip("the sandbox needs systemd")
	}
	for _, path := range []string{systemdRun, systemctl, "/sys/fs/cgroup/cgroup.controllers"} {
		if _, err := os.Stat(path); err != nil {
			t.Skip(err)
		}
	}
}

func request(t *testing.T, script string) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(Request{RequestID: protocol.NewUUID(), Script: script})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func answer(t *testing.T, runner *Runner, ctx context.Context, script string) Result {
	t.Helper()
	body := request(t, script)
	if err := runner.Validate(body); err != nil {
		t.Fatal(err)
	}
	result, ok := runner.Answer(ctx, body).(Result)
	if !ok {
		t.Fatal("no answer")
	}
	return result
}

func units(t *testing.T) string {
	t.Helper()
	out, err := exec.Command(systemctl, "list-units", "--all", "--no-legend", "--plain", unitPrefix+"*").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func assertNoUnits(t *testing.T) {
	t.Helper()
	if left := units(t); left != "" {
		t.Errorf("units left behind: %s", left)
	}
}

func waitForUnit(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for units(t) == "" {
		if time.Now().After(deadline) {
			t.Fatal("unit did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func summary(result Result) Result {
	result.Stdout, result.Stderr = short(result.Stdout), short(result.Stderr)
	return result
}

func short(text string) string {
	if len(text) > 200 {
		return text[:200] + "…"
	}
	return text
}
