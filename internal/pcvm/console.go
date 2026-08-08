package pcvm

import "io"

const clearConsoleSequence = "\x1b[2J\x1b[H\x1b[3J"

// ClearConsole erases the visible terminal and scrollback without touching logs or files.
func ClearConsole(out io.Writer, enabled bool) error {
	if !enabled {
		return nil
	}
	_, err := io.WriteString(out, clearConsoleSequence)
	return err
}
