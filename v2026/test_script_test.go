// Pins the local suite's output-filter status and binary-text contracts.
package connect

import (
	"os"
	"strings"
	"testing"
)

// Every live test pipeline snapshots both zsh statuses before selecting the
// downstream failure ahead of a secondary upstream SIGPIPE.
func TestTestScriptPreservesPipelineFailures(t *testing.T) {
	contentBytes, err := os.ReadFile("test.sh")
	if err != nil {
		t.Fatal(err)
	}
	content := string(contentBytes)
	checks := []struct {
		signature     string
		expectedCount int
	}{
		{signature: "| grep --binary-files=text --line-buffered", expectedCount: 3},
		{signature: `pipeline_status=("${pipestatus[@]}")`, expectedCount: 3},
		{signature: `test_pipeline_status "${pipeline_status[1]}" "${pipeline_status[2]}" || exit $?`, expectedCount: 3},
	}
	for _, check := range checks {
		if count := strings.Count(content, check.signature); count != check.expectedCount {
			t.Errorf("%q count = %d; want %d", check.signature, count, check.expectedCount)
		}
	}
}
