package connect

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The run every extender activation reports (G3, net_extender_run.go).

// Fails unless `runId` is 32 lower case hex characters of 16 bytes.
func testExtenderRunId(t *testing.T, runId string) {
	t.Helper()
	decoded, err := hex.DecodeString(runId)
	if err != nil || len(decoded) != extenderRunIdByteCount || hex.EncodeToString(decoded) != runId {
		t.Fatalf("run id = %q, expected 32 lower case hex characters", runId)
	}
}

// The process has one run: every read returns the same id, start and previous
// exit, the id is 32 lower case hex characters, and the start is a past
// instant in utc.
func TestExtenderProcessRunIsOneRunPerProcess(t *testing.T) {
	first := ExtenderProcessRun()
	second := ExtenderProcessRun()
	if first.Id != second.Id ||
		!first.StartTime.Equal(second.StartTime) ||
		first.PreviousExit != second.PreviousExit {
		t.Fatalf("the process run changed between reads: %+v then %+v", first, second)
	}
	testExtenderRunId(t, first.Id)
	if first.StartTime.IsZero() || time.Now().Before(first.StartTime) {
		t.Fatalf("process start = %s, expected a past instant", first.StartTime)
	}
	if first.StartTime.Location() != time.UTC {
		t.Fatalf("process start is in %s, expected utc", first.StartTime.Location())
	}

	// each read is a copy, so no caller can change the run another reads
	first.Id = "changed"
	if ExtenderProcessRun().Id == "changed" {
		t.Fatal("a caller changed the process run")
	}
}

// Each start draws a new id, keeps the start in utc and reads the host's
// previous exit through the token filter: what a restarted process reports.
func TestNewExtenderRunDrawsANewIdEachStart(t *testing.T) {
	startTime := time.Date(2026, 11, 2, 3, 4, 5, 0, time.FixedZone("test", -5*60*60))
	first := newExtenderRun(startTime, " SIGKILL\n")
	second := newExtenderRun(startTime, "")

	testExtenderRunId(t, first.Id)
	testExtenderRunId(t, second.Id)
	if first.Id == second.Id {
		t.Fatalf("two starts drew the same run id %s", first.Id)
	}
	if !first.StartTime.Equal(startTime) || first.StartTime.Location() != time.UTC {
		t.Fatalf("run start = %s, expected %s in utc", first.StartTime, startTime)
	}
	if first.PreviousExit != "sigkill" {
		t.Fatalf("previous exit = %q, expected sigkill", first.PreviousExit)
	}
	if second.PreviousExit != "" {
		t.Fatalf("previous exit = %q, expected none", second.PreviousExit)
	}
}

// What a child process prints of its run when testExtenderRunChildEnv is set.
const testExtenderRunChildEnv = "URNETWORK_TEST_EXTENDER_RUN_CHILD"
const testExtenderRunChildPrefix = "extender-run-child "

// A process reads the previous exit from the environment it started with, and
// every start draws its own run: this is the process itself, run again with
// the variable a host sets, printing what its activations would report.
func TestExtenderProcessRunReadsTheEnvironmentOfItsStart(t *testing.T) {
	if os.Getenv(testExtenderRunChildEnv) != "" {
		run := ExtenderProcessRun()
		fmt.Printf("%s%s %s %q\n", testExtenderRunChildPrefix, run.Id, run.StartTime.Format(time.RFC3339Nano), run.PreviousExit)
		return
	}

	const unset = "\x00"
	seenRunIds := map[string]bool{ExtenderProcessRun().Id: true}
	for _, c := range []struct {
		value    string
		expected string
	}{
		{value: "sigkill", expected: "sigkill"},
		{value: "exit-137", expected: "exit-137"},
		// a host's spelling, which the filter normalizes
		{value: " SIGKILL\n", expected: "sigkill"},
		// not a token, so nothing rides the activation
		{value: "kill -9", expected: ""},
		{value: unset, expected: ""},
	} {
		beforeStart := time.Now()
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestExtenderProcessRunReadsTheEnvironmentOfItsStart$")
		command.Env = []string{testExtenderRunChildEnv + "=1"}
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, ExtenderPreviousExitEnv+"=") {
				command.Env = append(command.Env, entry)
			}
		}
		if c.value != unset {
			command.Env = append(command.Env, ExtenderPreviousExitEnv+"="+c.value)
		}
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("the child for %q failed: %v\n%s", c.value, err, output)
		}

		line := ""
		for _, outputLine := range strings.Split(string(output), "\n") {
			if rest, ok := strings.CutPrefix(outputLine, testExtenderRunChildPrefix); ok {
				line = rest
			}
		}
		fields := strings.SplitN(line, " ", 3)
		if len(fields) != 3 {
			t.Fatalf("the child for %q printed no run:\n%s", c.value, output)
		}
		runId, startText, quotedPreviousExit := fields[0], fields[1], fields[2]

		testExtenderRunId(t, runId)
		if seenRunIds[runId] {
			t.Fatalf("the child for %q drew a run id another process has: %s", c.value, runId)
		}
		seenRunIds[runId] = true

		startTime, err := time.Parse(time.RFC3339Nano, startText)
		if err != nil {
			t.Fatalf("the child for %q printed the start %q: %v", c.value, startText, err)
		}
		if startTime.Before(beforeStart) {
			t.Fatalf("the child for %q started at %s, before it was run at %s", c.value, startTime, beforeStart)
		}

		previousExit, err := strconv.Unquote(quotedPreviousExit)
		if err != nil {
			t.Fatalf("the child for %q printed the previous exit %s: %v", c.value, quotedPreviousExit, err)
		}
		if previousExit != c.expected {
			t.Errorf("the child for %q read the previous exit %q, expected %q", c.value, previousExit, c.expected)
		}
	}
}

// The host's value rides the activation only as a short token of lower case
// letters, digits and dashes; the operator judges the word itself. The
// variable's name is the contract with the hosts that set it: the sn miner's
// supervisor spells it out on its side (operatorChildPreviousExitEnv), so a
// rename here would silently end the reports.
func TestExtenderPreviousExitCarriesOnlyAShortToken(t *testing.T) {
	if ExtenderPreviousExitEnv != "URNETWORK_PREVIOUS_EXIT" {
		t.Fatalf("the variable is %q, which the hosts do not set", ExtenderPreviousExitEnv)
	}
	for _, c := range []struct {
		value    string
		expected string
	}{
		{value: "", expected: ""},
		{value: "sigkill", expected: "sigkill"},
		{value: " SIGKILL\n", expected: "sigkill"},
		{value: "oom", expected: "oom"},
		{value: "stop", expected: "stop"},
		{value: "exit-137", expected: "exit-137"},
		// the operator's column is 16 wide
		{value: "abcdefghijklmnop", expected: "abcdefghijklmnop"},
		{value: "abcdefghijklmnopq", expected: ""},
		{value: "exit 2", expected: ""},
		{value: "sig_kill", expected: ""},
		{value: "exit-2;x", expected: ""},
		{value: "sïgkill", expected: ""},
	} {
		if previousExit := extenderPreviousExit(c.value); previousExit != c.expected {
			t.Errorf("previous exit of %q = %q, expected %q", c.value, previousExit, c.expected)
		}
	}
}

// Every pass of one process posts the same run on every family -- at start,
// on the tick and after a network change -- and so does a second activator of
// the same process, so a restart is the only thing that changes the run id
// the operator stores.
func TestExtenderActivatorPostsOneRunAcrossPasses(t *testing.T) {
	run := ExtenderProcessRun()

	fixture := newTestActivatorFixture(t, nil)
	fixture.waitPass()
	posts := []*testActivatePost{fixture.waitPost(), fixture.waitPost()}

	// the tick re-posts both families
	fixture.clock.advance(24 * time.Hour)
	fixture.wake()
	posts = append(posts, fixture.waitPost(), fixture.waitPost())

	// and a network change both again
	fixture.clock.advance(2 * time.Minute)
	fixture.networkChanged()
	posts = append(posts, fixture.waitPost(), fixture.waitPost())

	// another activator of this process
	other := newTestActivatorFixture(t, nil)
	other.waitPass()
	posts = append(posts, other.waitPost(), other.waitPost())

	familyPostCounts := map[int]int{}
	for _, post := range posts {
		familyPostCounts[post.ipVersion] += 1
		if post.args.RunId != run.Id {
			t.Fatalf("v%d posted run id %q, expected the process run %q", post.ipVersion, post.args.RunId, run.Id)
		}
		if post.args.RunStartTime == nil || !post.args.RunStartTime.Equal(run.StartTime) {
			t.Fatalf("v%d posted run start %v, expected %s", post.ipVersion, post.args.RunStartTime, run.StartTime)
		}
		if post.args.PreviousExit != run.PreviousExit {
			t.Fatalf("v%d posted previous exit %q, expected %q", post.ipVersion, post.args.PreviousExit, run.PreviousExit)
		}
	}
	if familyPostCounts[4] != 4 || familyPostCounts[6] != 4 {
		t.Fatalf("posts by family = %v, expected four of each", familyPostCounts)
	}
}

// The run rides the post as the operator decodes it off the wire: the id, the
// start in utc whatever zone the host's clock reads, and the previous exit.
func TestExtenderActivatorPostsTheRunItIsGiven(t *testing.T) {
	startTime := time.Date(2026, 11, 2, 3, 4, 5, 6000, time.FixedZone("test", 2*60*60))
	run := &ExtenderRun{
		Id:           "00112233445566778899aabbccddeeff",
		StartTime:    startTime,
		PreviousExit: "sigkill",
	}
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.ApiUrlV6 = ""
		settings.Run = run
	})
	fixture.waitPass()
	post := fixture.waitPost()

	if post.args.RunId != run.Id {
		t.Fatalf("run id = %q, expected %q", post.args.RunId, run.Id)
	}
	if post.args.RunStartTime == nil || !post.args.RunStartTime.Equal(startTime) {
		t.Fatalf("run start = %v, expected %s", post.args.RunStartTime, startTime)
	}
	if _, offset := post.args.RunStartTime.Zone(); offset != 0 {
		t.Fatalf("run start was posted %d s off utc", offset)
	}
	if post.args.PreviousExit != "sigkill" {
		t.Fatalf("previous exit = %q, expected sigkill", post.args.PreviousExit)
	}
}

// The run fields are the operator's json contract
// (controller.ExtenderActivateArgs): the id and the previous exit as strings
// and the start in rfc 3339, each left out when unset, which is the document
// an extender that predates them sends.
func TestExtenderActivateArgsRunJsonContract(t *testing.T) {
	runStartTime := time.Date(2026, 11, 2, 3, 4, 5, 0, time.UTC)
	args := &ExtenderActivateArgs{
		PublicKeyHex: "00112233",
		TcpPort:      443,
		UdpPort:      443,
		DnsPort:      53,
		DnsTld:       "x.example.",
		Carriers:     []string{"tcp"},
		RunId:        "00112233445566778899aabbccddeeff",
		RunStartTime: &runStartTime,
		PreviousExit: "exit-2",
	}
	argsBytes, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"public_key_hex":"00112233","tcp_port":443,"udp_port":443,` +
		`"dns_port":53,"dns_tld":"x.example.","carriers":["tcp"],` +
		`"run_id":"00112233445566778899aabbccddeeff",` +
		`"run_start_time":"2026-11-02T03:04:05Z","previous_exit":"exit-2"}`
	if string(argsBytes) != expected {
		t.Fatalf("args json = %s, expected %s", argsBytes, expected)
	}

	decoded := &ExtenderActivateArgs{}
	if err := json.Unmarshal([]byte(expected), decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, args) {
		t.Fatalf("decoded args = %+v, expected %+v", decoded, args)
	}
}
