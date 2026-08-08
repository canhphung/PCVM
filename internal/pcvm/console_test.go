package pcvm

import (
	"bytes"
	"testing"
)

func TestClearConsole(t *testing.T) {
	var output bytes.Buffer
	if err := ClearConsole(&output, true); err != nil {
		t.Fatal(err)
	}
	if output.String() != clearConsoleSequence {
		t.Fatalf("sequence=%q", output.String())
	}
	output.Reset()
	if err := ClearConsole(&output, false); err != nil || output.Len() != 0 {
		t.Fatalf("disabled clear wrote %q: %v", output.String(), err)
	}
}
