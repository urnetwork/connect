package connect

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// The structural property behind three separate defects in this program: a
// value computed correctly and then not consumed.
//
// The ceiling was derived and then overwritten by the initial size. The
// interval was computed from a resend timer rather than from the path. The
// delivery estimate was summed over the wrong horizon. Each was one place
// reading an ingredient of the window instead of the window, and each was found
// by measurement after it had already cost time. A test that makes a fourth
// impossible is worth more than three tests that catch the three we found.
//
// So this asserts ownership rather than behaviour: the window has exactly one
// place that computes it, and the ingredients that go into it are read nowhere
// else. A new consumer that reaches for the share, the peer's advertised
// capacity, the configured ceiling, the target or the delivery rate — rather
// than for the window the estimator returns — fails here, at the point it is
// written, instead of in a campaign three weeks later.
//
// This is a source-level assertion, which is unusual and deliberate. The
// property is about which code may read what, and no runtime test can express
// that: every runtime test can only observe the consequences of a wrong reader
// after someone has added one.
//
// Derived rather than counted, and named as such: the enclosing function of an
// occurrence is derived by scanning back to the nearest line beginning "func ",
// which is exact for this file's formatting (gofmt puts every declaration at
// column zero) and would need a real parser if that changed.
func TestTheWindowHasOneOwner(t *testing.T) {
	// Go's build overlay does not redirect runtime source reads. The private
	// source gate binds this path explicitly; ordinary integration reads live source.
	sourcePath := os.Getenv("CONNECT_TEST_WINDOW_OWNER_SOURCE")
	if sourcePath == "" {
		sourcePath = "transfer.go"
	}
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read %s: %v", sourcePath, err)
	}
	t.Logf("window source: %s", sourcePath)
	violations, consumers := windowOwnershipViolations(source)
	t.Logf("window consumers: %v", consumers)
	for _, violation := range violations {
		t.Error(violation)
	}
}

// Keep the existing lexical ownership rule independently testable with source
// mutations; constructor refactors do not authorize new runtime consumers.
func windowOwnershipViolations(source []byte) ([]string, map[string]bool) {
	var violations []string
	lines := strings.Split(string(source), "\n")

	// the function each line belongs to, derived by scanning back to the
	// nearest declaration at column zero
	enclosing := make([]string, len(lines))
	current := ""
	for i, line := range lines {
		// a closing brace at column zero ends the declaration, so a type
		// declared after a function is not counted as inside it
		if line == "}" {
			current = ""
		}
		if strings.HasPrefix(line, "func ") {
			name := line[len("func "):]
			if strings.HasPrefix(name, "(") {
				if close := strings.Index(name, ") "); 0 <= close {
					name = name[close+2:]
				}
			}
			if open := strings.Index(name, "("); 0 <= open {
				name = name[:open]
			}
			current = strings.TrimSpace(name)
		}
		enclosing[i] = current
	}

	// The ingredients of the window, and the only functions allowed to read
	// each. Everything else must read the estimate's Window.
	//
	// The settings derivation lives in applyWindowSizingForMemoryTarget and
	// defaultSendBufferSettingsWithMemoryTarget. Since 00d4f62b, the public
	// ApplyWindowSizing and DefaultSendBufferSettingsWithBufferSize only pass
	// the process memory target to them, so they read no ingredient.
	owners := map[string][]string{
		"DeliverySizedWindowCeilingByteCount": {
			"applyWindowSizingForMemoryTarget",
			"defaultSendBufferSettingsWithMemoryTarget",
			"estimateSendWindow",
		},
		"TargetGoodputByteRate": {
			"applyWindowSizingForMemoryTarget",
			"defaultSendBufferSettingsWithMemoryTarget",
			"estimateSendWindow",
			"bindingTerm",
		},
		"LendableByteCount": {
			"estimateSendWindow",
			"bindingTerm",
			"LendableByteCount",
		},
		"receivedWindowAdvertisement": {
			"estimateSendWindow",
			"bindingTerm",
			"receivedWindowAdvertisement",
		},
		"deliveredRate": {
			"estimateSendWindow",
			"deliveredRate",
		},
		"deliveredServiceRate": {
			"deliveredServiceRate",
		},
		"deliveryServiceEstimate": {
			"estimateSendWindow",
			"deliveredServiceRate",
			"deliveryServiceEstimate",
		},
	}

	for symbol, allowed := range owners {
		allowedSet := map[string]bool{}
		for _, name := range allowed {
			allowedSet[name] = true
		}
		found := 0
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || !strings.Contains(line, symbol) {
				continue
			}
			// a field or constant declared outside any function is the
			// definition rather than a reader of it
			if enclosing[i] == "" {
				continue
			}
			found += 1
			if !allowedSet[enclosing[i]] {
				violations = append(violations, fmt.Sprintf(
					"transfer.go:%d reads %s inside %s, which is not one of the window's owners (%s); a consumer that reaches for an ingredient rather than for the window the estimator returns is the shape of the three defects this program has already paid for",
					i+1, symbol, enclosing[i], strings.Join(allowed, ", "),
				))
			}
		}
		if found == 0 {
			violations = append(violations, fmt.Sprintf(
				"%s was not found at all, so this row is guarding a name that no longer exists and is asserting nothing",
				symbol,
			))
		}
	}

	// The same estimator serves admission, its physical pacing projection,
	// and read-only statistics; no consumer duplicates window arithmetic.
	consumers := map[string]bool{}
	for i, line := range lines {
		if strings.Contains(line, "estimateSendWindow(") &&
			!strings.HasPrefix(strings.TrimSpace(line), "//") &&
			enclosing[i] != "estimateSendWindow" {
			consumers[enclosing[i]] = true
		}
	}
	if len(consumers) != 3 || !consumers["sendWindowEstimate"] || !consumers["sendWindowPacingEstimate"] || !consumers["sendWindowSnapshot"] {
		violations = append(violations, fmt.Sprintf(
			"%d functions reach the window arithmetic; admission, physical pacing and read-only statistics must use the same owner",
			len(consumers),
		))
	}
	return violations, consumers
}

// Every protected ingredient and the shared estimator still reject an added
// consumer. Missing ingredients cannot silently turn this guard into a no-op.
func TestWindowOwnerGuardRejectsIngredientAndConsumerMutations(t *testing.T) {
	sourcePath := os.Getenv("CONNECT_TEST_WINDOW_OWNER_SOURCE")
	if sourcePath == "" {
		sourcePath = "transfer.go"
	}
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if failures, _ := windowOwnershipViolations(source); len(failures) != 0 {
		t.Fatalf("unmodified ownership source failed: %v", failures)
	}
	for _, symbol := range []string{
		"DeliverySizedWindowCeilingByteCount", "TargetGoodputByteRate",
		"LendableByteCount", "receivedWindowAdvertisement", "deliveredRate",
		"deliveredServiceRate", "deliveryServiceEstimate",
	} {
		consumer := "\nfunc windowIngredientIntruder() {\n\t_ = owner." + symbol + "\n}\n"
		mutated := append(append([]byte(nil), source...), consumer...)
		failures, _ := windowOwnershipViolations(mutated)
		if len(failures) != 1 || !strings.Contains(failures[0], "reads "+symbol+" inside windowIngredientIntruder") {
			t.Errorf("ingredient %s escaped exact owner guard: %v", symbol, failures)
		}
		missing := []byte(strings.ReplaceAll(string(source), symbol, "removedWindowIngredient"))
		failures, _ = windowOwnershipViolations(missing)
		expected := []string{symbol + " was not found at all,"}
		if symbol == "deliveredServiceRate" {
			// Renaming this helper also withdraws its authority to call the
			// distinct service estimator. Both violations must remain visible.
			expected = append(expected, " reads deliveryServiceEstimate inside removedWindowIngredient, which is not one of the window's owners (")
		}
		if len(failures) != len(expected) {
			t.Errorf("missing ingredient %s produced %d errors, want %d: %v", symbol, len(failures), len(expected), failures)
		}
		// Guard iteration uses a map, so require each distinct cause once
		// without assuming which valid diagnostic arrives first.
		for _, cause := range expected {
			count := 0
			for _, failure := range failures {
				if strings.Contains(failure, cause) {
					count++
				}
			}
			if count != 1 {
				t.Errorf("missing ingredient %s reported cause %q %d times, want once: %v", symbol, cause, count, failures)
			}
		}
	}
	consumer := "\nfunc windowArithmeticIntruder() {\n\t_ = owner.estimateSendWindow(now, false, false)\n}\n"
	mutated := append(append([]byte(nil), source...), consumer...)
	failures, consumers := windowOwnershipViolations(mutated)
	if len(failures) != 1 || len(consumers) != 4 || !consumers["windowArithmeticIntruder"] ||
		!strings.Contains(failures[0], "4 functions reach the window arithmetic") {
		t.Errorf("additional estimator consumer escaped guard: %v consumers=%v", failures, consumers)
	}
}
