package main

import (
	"fmt"

	"github.com/orlenko/aiq/internal/longrun"
)

const handoffUsage = "usage: aiq handoff redact [--apply]"

// cmdHandoff manages the notes already on disk. New notes are redacted as they
// are written and offered; this command is for the ones from before that.
func cmdHandoff(args []string) error {
	if len(args) == 0 || args[0] != "redact" {
		return fmt.Errorf(handoffUsage)
	}
	apply := false
	for _, arg := range args[1:] {
		if arg != "--apply" {
			return fmt.Errorf(handoffUsage)
		}
		apply = true
	}
	return handoffRedact(apply)
}

// handoffRedact prints kinds and counts only, so its output is safe to paste.
func handoffRedact(apply bool) error {
	files, err := longrun.NoteFiles()
	if err != nil {
		return err
	}
	if apply {
		longrun.LockNoteDir()
	}
	found := 0
	failed := 0
	for _, p := range files {
		var hits map[string]int
		if apply {
			hits, err = longrun.RedactNote(p)
		} else {
			hits, err = longrun.ScanNote(p)
		}
		if err != nil {
			failed++
			fmt.Printf("%s: %v\n", p, err)
			continue
		}
		if len(hits) > 0 {
			found++
			fmt.Printf("%s: %s\n", p, longrun.DescribeHits(hits))
		}
	}
	verb := "would be redacted; run again with --apply to rewrite them, with no backup"
	if apply {
		verb = "redacted"
	}
	fmt.Printf("%d of %d note(s) %s.\n", found, len(files), verb)
	fmt.Println("This matches known secret formats only, and leaves session transcripts and state.db as they are.")
	if failed > 0 {
		return fmt.Errorf("%d note(s) could not be read or rewritten", failed)
	}
	return nil
}
