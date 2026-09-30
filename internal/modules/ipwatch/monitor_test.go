package ipwatch

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akastrmix/akastr-agent/internal/protocol"
)

type familySequenceObserver struct {
	v4      []string
	v6      []string
	v4Index int
	v6Index int
	v6Err   error
}

func (o *familySequenceObserver) Observe(_ context.Context, family Family) (Observation, error) {
	if family == IPv6 && o.v6Err != nil {
		return Observation{}, o.v6Err
	}
	values, index := o.v4, &o.v4Index
	if family == IPv6 {
		values, index = o.v6, &o.v6Index
	}
	value := values[*index]
	if *index < len(values)-1 {
		(*index)++
	}
	return Observation{
		Address:    netip.MustParseAddr(value),
		ObservedAt: time.Date(2026, 8, 22, 0, 0, *index, 0, time.UTC),
	}, nil
}

type sequenceObserver struct {
	values []string
	index  int
}

func (o *sequenceObserver) Observe(_ context.Context, _ Family) (Observation, error) {
	value := o.values[o.index]
	if o.index < len(o.values)-1 {
		o.index++
	}
	return Observation{
		Address:    netip.MustParseAddr(value),
		ObservedAt: time.Date(2026, 8, 13, 0, 0, o.index, 0, time.UTC),
	}, nil
}

type recorder struct{ reports []protocol.ReportBody }

func (r *recorder) publish(report protocol.ReportBody) error {
	r.reports = append(r.reports, report)
	return nil
}

func (r *recorder) last(t *testing.T) protocol.ReportBody {
	t.Helper()
	if len(r.reports) == 0 {
		t.Fatal("nothing was reported")
	}
	return r.reports[len(r.reports)-1]
}

const commandID = "123e4567-e89b-42d3-a456-426614174000"

func openMonitor(t *testing.T, filePath string, observer AddressObserver, ipv6 bool) *Monitor {
	t.Helper()
	monitor, err := OpenMonitor(filePath, observer, time.Minute, ipv6)
	if err != nil {
		t.Fatal(err)
	}
	return monitor
}

func step(t *testing.T, monitor *Monitor, sent *recorder) {
	t.Helper()
	if err := monitor.stepIPv4(context.Background(), sent.publish); err != nil {
		t.Fatal(err)
	}
}

func acknowledge(t *testing.T, monitor *Monitor, report protocol.ReportBody) {
	t.Helper()
	if handled, err := monitor.Acknowledge(report.ReportID); err != nil || !handled {
		t.Fatalf("acknowledge %s: handled=%v err=%v", report.Kind, handled, err)
	}
}

// settle reports the first address of a session and has Cloud store it.
func settle(t *testing.T, monitor *Monitor, sent *recorder) {
	t.Helper()
	step(t, monitor, sent)
	acknowledge(t, monitor, sent.last(t))
	if !monitor.AddressSettled() {
		t.Fatal("address is not settled after Cloud stored it")
	}
}

func TestOpenMonitorRejectsUnknownReconciliationSchema(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "changeip-reconciliation.json")
	if err := os.WriteFile(filePath, []byte(`{"schema_version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := OpenMonitor(filePath, &sequenceObserver{values: []string{"8.8.8.8"}}, time.Minute, false)
	if err == nil || !strings.Contains(err.Error(), "schema is unsupported") {
		t.Fatalf("schema error = %v", err)
	}
}

func TestEverySessionIsToldTheCurrentAddressAndChangesAreResentUntilStored(t *testing.T) {
	sent := &recorder{}
	monitor := openMonitor(t, filepath.Join(t.TempDir(), "state.json"),
		&sequenceObserver{values: []string{"8.8.8.8", "8.8.8.8", "8.8.8.8", "1.1.1.1"}}, false)

	step(t, monitor, sent)
	first := sent.last(t)
	data := first.Data.(AddressData)
	if first.Kind != KindAddress || data.Address != "8.8.8.8" || data.Family != "ipv4" || data.CommandID != nil {
		t.Fatalf("first report = %#v", first)
	}
	if monitor.AddressSettled() {
		t.Fatal("address settled before Cloud stored it")
	}
	// Until it is stored the same report goes out again.
	step(t, monitor, sent)
	if sent.last(t).ReportID != first.ReportID {
		t.Fatal("an unstored report was replaced")
	}
	acknowledge(t, monitor, first)
	step(t, monitor, sent)
	if len(sent.reports) != 2 {
		t.Fatalf("an unchanged address was reported again: %#v", sent.reports)
	}

	// A new session gets the address again and waits for it to be stored.
	monitor.NotifyControlReady()
	if monitor.AddressSettled() {
		t.Fatal("address stayed settled across a new session")
	}
	step(t, monitor, sent)
	again := sent.last(t)
	if again.ReportID == first.ReportID || again.Data.(AddressData).Address != "8.8.8.8" {
		t.Fatalf("session report = %#v", again)
	}
	acknowledge(t, monitor, again)

	step(t, monitor, sent)
	changed := sent.last(t)
	if data := changed.Data.(AddressData); data.Address != "1.1.1.1" || data.CommandID != nil {
		t.Fatalf("natural change = %#v", changed)
	}
	// A repeated acknowledgement of an older report leaves the new one waiting.
	if handled, _ := monitor.Acknowledge(again.ReportID); handled {
		t.Fatal("a stale acknowledgement was taken")
	}
	if monitor.AddressSettled() {
		t.Fatal("stale acknowledgement settled the new address")
	}
}

func TestChangedAddressDuringChangeIPIsReportedAsItsOutcomeAcrossRestart(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "state.json")
	sent := &recorder{}
	observer := &sequenceObserver{values: []string{"8.8.8.8", "8.8.4.4"}}
	monitor := openMonitor(t, filePath, observer, false)
	settle(t, monitor, sent)
	now := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	monitor.now = func() time.Time { return now }
	if err := monitor.ArmChange(commandID, "1.1.1.1", now); err == nil {
		t.Fatal("ChangeIP armed from an address Cloud does not hold")
	}
	if err := monitor.ArmChange(commandID, "8.8.8.8", now); err != nil {
		t.Fatal(err)
	}
	if (Reporter{Monitor: monitor}).UpdateSafe() == nil {
		t.Fatal("update allowed during a ChangeIP reconciliation")
	}
	step(t, monitor, sent)
	outcome := sent.last(t)
	data := outcome.Data.(AddressData)
	if outcome.Kind != KindAddress || data.Address != "8.8.4.4" || data.CommandID == nil || *data.CommandID != commandID {
		t.Fatalf("ChangeIP outcome = %#v", outcome)
	}

	// A restart before Cloud stores it resends the same outcome, not a natural change.
	reopened := openMonitor(t, filePath, &sequenceObserver{values: []string{"8.8.4.4"}}, false)
	if address, found := reopened.ChangeAddress(commandID); !found || address != "8.8.8.8" {
		t.Fatalf("reconciliation address after restart = %q %v", address, found)
	}
	resent := &recorder{}
	step(t, reopened, resent)
	if resent.last(t).ReportID != outcome.ReportID {
		t.Fatalf("outcome after restart = %#v", resent.reports)
	}
	acknowledge(t, reopened, outcome)
	if (Reporter{Monitor: reopened}).UpdateSafe() != nil {
		t.Fatal("stored outcome still blocks updates")
	}
	step(t, reopened, resent)
	if len(resent.reports) != 2 || resent.last(t).Data.(AddressData).CommandID != nil {
		t.Fatalf("first report of the restarted process = %#v", resent.reports)
	}
}

func TestUnchangedAddressSettlesChangeIPAfterGraceAndTwoObservations(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "state.json")
	sent := &recorder{}
	monitor := openMonitor(t, filePath, &sequenceObserver{values: []string{"8.8.8.8"}}, false)
	settle(t, monitor, sent)
	now := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	monitor.now = func() time.Time { return now }
	if err := monitor.ArmChange(commandID, "8.8.8.8", now); err != nil {
		t.Fatal(err)
	}
	// The old address before the grace ends does not settle the attempt.
	now = now.Add(time.Minute)
	step(t, monitor, sent)
	now = now.Add(time.Minute)
	for index := 0; index < changeUnchangedConfirmations; index++ {
		if len(sent.reports) != 1 {
			t.Fatalf("attempt settled after %d confirmations: %#v", index, sent.reports)
		}
		step(t, monitor, sent)
		now = now.Add(changeObserveInterval)
	}
	outcome := sent.last(t)
	data, ok := outcome.Data.(UnchangedData)
	if outcome.Kind != KindUnchanged || !ok || data.CommandID != commandID || data.Address != "8.8.8.8" {
		t.Fatalf("unchanged outcome = %#v", outcome)
	}
	reopened := openMonitor(t, filePath, &sequenceObserver{values: []string{"8.8.8.8"}}, false)
	acknowledge(t, reopened, outcome)
	if _, found := reopened.ChangeAddress(commandID); found {
		t.Fatal("stored outcome left the reconciliation open")
	}
}

func TestIPv6IsReportedWithoutSettlingIPv4(t *testing.T) {
	sent := &recorder{}
	monitor := openMonitor(t, filepath.Join(t.TempDir(), "state.json"),
		&familySequenceObserver{v4: []string{"8.8.8.8"}, v6: []string{"2606:4700:4700::1111", "2001:4860:4860::8888"}}, true)
	if err := monitor.stepIPv6(context.Background(), sent.publish); err != nil {
		t.Fatal(err)
	}
	first := sent.last(t)
	if data := first.Data.(AddressData); data.Family != "ipv6" || data.Address != "2606:4700:4700::1111" {
		t.Fatalf("IPv6 report = %#v", first)
	}
	acknowledge(t, monitor, first)
	if monitor.AddressSettled() {
		t.Fatal("an IPv6 report settled the IPv4 address")
	}
	if err := monitor.stepIPv6(context.Background(), sent.publish); err != nil {
		t.Fatal(err)
	}
	if data := sent.last(t).Data.(AddressData); data.Address != "2001:4860:4860::8888" {
		t.Fatalf("IPv6 change = %#v", sent.last(t))
	}
}

func TestIPv6ProbeFailureIsTransient(t *testing.T) {
	monitor := openMonitor(t, filepath.Join(t.TempDir(), "state.json"),
		&familySequenceObserver{v4: []string{"8.8.8.8"}, v6Err: errors.New("no IPv6 route")}, true)
	err := monitor.stepIPv6(context.Background(), (&recorder{}).publish)
	if !errors.Is(err, errTransientMonitor) {
		t.Fatalf("IPv6 probe error = %v", err)
	}
}

func TestOpenMonitorRejectsIntervalsLongerThanFiveMinutes(t *testing.T) {
	if _, err := OpenMonitor(filepath.Join(t.TempDir(), "state.json"),
		&sequenceObserver{values: []string{"8.8.8.8"}}, 5*time.Minute+time.Second, false); err == nil {
		t.Fatal("an interval over five minutes was accepted")
	}
}
